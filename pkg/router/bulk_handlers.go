/*
Copyright 2024 AgentTier Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
	"github.com/agenttier/agenttier/pkg/governance"
)

// bulkCreateItemRequest is one entry in POST /sandboxes/bulk's items array.
// It intentionally mirrors CreateSandboxRequest (handlers.go) field-for-field
// so a caller building a batch from N individual create bodies doesn't need
// to reshape anything.
type bulkCreateItemRequest struct {
	Name        string                               `json:"name"`
	Namespace   string                               `json:"namespace,omitempty"`
	TemplateRef *agenttierv1alpha1.TemplateReference `json:"templateRef"`
	Timeout     string                               `json:"timeout,omitempty"`
	IdleTimeout string                               `json:"idleTimeout,omitempty"`
	Storage     *agenttierv1alpha1.StorageSpec       `json:"storage,omitempty"`
}

// bulkCreateItemResult is one entry in POST /sandboxes/bulk's response array
// (FR4.1/FR4.3): per-item success/failure so a partial failure doesn't lose
// the sandboxes that did succeed.
type bulkCreateItemResult struct {
	Index     int    `json:"index"`
	Status    string `json:"status"` // "created" | "error"
	SandboxID string `json:"sandboxId,omitempty"`
	Error     string `json:"error,omitempty"`
}

// handleBulkCreate implements POST /sandboxes/bulk: create N sandboxes from
// one request body, `{"items":[<createSpec>...]}`.
//
// FR4.4's governance-cap check is the one exception to "each item is
// independent" (FR4.3): the aggregate cap is evaluated for the WHOLE batch
// up front via governance.CheckBulk, and a would-exceed batch is rejected in
// full — fail-fast, nothing created (DD4). Once past that gate, every item's
// own per-item concerns (bad template, image policy, etc.) are independent;
// a failure on one item never aborts the rest (FR4.3).
//
// Sandbox-scoped keys are rejected outright (403) — bulk operations are not
// sandbox-scoped (DD3, NFR2) regardless of which sandbox IDs happen to
// appear in the batch.
func (s *Server) handleBulkCreate(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if claims.SandboxID != "" {
		respondError(w, http.StatusForbidden, "sandbox-scoped keys may not perform bulk operations")
		return
	}

	var req struct {
		Items []bulkCreateItemRequest `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if len(req.Items) == 0 {
		respondError(w, http.StatusBadRequest, "items must be a non-empty array")
		return
	}

	// Sandboxes with no explicit namespace fall back to the router's
	// configured default, exactly like handleCreateSandbox.
	defaultNamespace := s.config.SandboxNamespace
	if defaultNamespace == "" {
		defaultNamespace = "default"
	}

	// Resolve namespace + governance policy once per distinct namespace in
	// the batch, then run the fail-fast aggregate check (FR4.4/DD4) before
	// creating anything. The resolved (policy, usage) per namespace is also
	// retained (nsPolicies/nsUsage below) so createOneBulkSandbox can run the
	// FULL per-item governance.Check (template/registry allowlists, resource
	// and timeout value caps) that the aggregate CheckBulk above deliberately
	// does not cover — CheckBulk only evaluates the sandbox-COUNT quotas
	// (FR4.4). Without this, an item with a disallowed template or an
	// over-cap resource/timeout would be created with zero per-item policy
	// enforcement at the Router layer (the admission webhook, if enabled,
	// would still catch it, but bulk create must not depend on that opt-in
	// path for its own baseline enforcement — see requirements.md's edge
	// case: "some items reference a template ... isn't allowed by
	// governance — those items fail individually").
	nsPolicies := make(map[string]governance.Policy)
	nsUsage := make(map[string]governance.Usage)
	if s.governanceStore != nil {
		byNamespace := make(map[string][]int)
		for i, item := range req.Items {
			ns := item.Namespace
			if ns == "" {
				ns = defaultNamespace
			}
			byNamespace[ns] = append(byNamespace[ns], i)
		}
		for ns, indices := range byNamespace {
			policy, err := governance.Resolve(r.Context(), s.governanceStore, ns)
			if err != nil {
				s.logger.Warn("failed to resolve governance policy; proceeding without enforcement", "namespace", ns, "error", err)
				continue
			}
			if policy.IsEmpty() {
				continue
			}
			existing := &agenttierv1alpha1.SandboxList{}
			if err := s.k8sClient.List(r.Context(), existing, client.InNamespace(ns)); err != nil {
				respondError(w, http.StatusInternalServerError, "failed to check namespace usage: "+err.Error())
				return
			}
			usage := governance.CountUsage(existing, claims.Sub)
			nsPolicies[ns] = policy
			nsUsage[ns] = usage

			agentN := 0
			for _, idx := range indices {
				if s.resolveTemplateMode(r.Context(), req.Items[idx].TemplateRef, ns) == agenttierv1alpha1.SandboxModeAgent {
					agentN++
				}
			}
			if v := governance.CheckBulk(policy, usage, len(indices), agentN); v.Violated() {
				respondJSON(w, http.StatusConflict, map[string]interface{}{
					"error":      "quota_would_exceed",
					"namespace":  ns,
					"violations": v,
				})
				return
			}
		}
	}

	results := make([]bulkCreateItemResult, len(req.Items))
	for i, item := range req.Items {
		ns := item.Namespace
		if ns == "" {
			ns = defaultNamespace
		}
		results[i] = s.createOneBulkSandbox(r.Context(), claims, i, item, defaultNamespace, nsPolicies[ns], nsUsage[ns])
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"results": results})
	s.recordAudit(r.Context(), claims, "bulk.create", "sandbox", "")
}

// createOneBulkSandbox creates a single sandbox from one bulk-create item,
// applying the same per-item validation handleCreateSandbox does
// (name/templateRef required, timeout/idleTimeout parse errors, and the full
// governance.Check — template/registry allowlists plus resource/timeout
// value caps) but returning a result struct instead of writing an HTTP
// response — a bad item never aborts sibling items (FR4.3). The aggregate
// sandbox-COUNT cap (CheckBulk) was already enforced by the caller for the
// whole batch; policy/usage here are the same values the caller resolved
// per namespace, passed through so this per-item Check doesn't re-resolve
// or re-List for every item. A zero-value Policy (namespace has no
// configured governance, or the store is nil) makes Check a no-op, matching
// handleCreateSandbox's own "skip enforcement when policy.IsEmpty()"
// behavior.
func (s *Server) createOneBulkSandbox(ctx context.Context, claims *Claims, index int, item bulkCreateItemRequest, defaultNamespace string, policy governance.Policy, usage governance.Usage) bulkCreateItemResult {
	if item.Name == "" {
		return bulkCreateItemResult{Index: index, Status: "error", Error: "name is required"}
	}
	if item.TemplateRef == nil || item.TemplateRef.Name == "" {
		return bulkCreateItemResult{Index: index, Status: "error", Error: "templateRef.name is required"}
	}

	namespace := item.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}

	sandbox := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      item.Name,
			Namespace: namespace,
		},
		Spec: agenttierv1alpha1.SandboxSpec{
			TemplateRef: item.TemplateRef,
			CreatedBy: &agenttierv1alpha1.UserIdentity{
				Sub:         claims.Sub,
				Email:       claims.Email,
				DisplayName: claims.Name,
			},
		},
	}

	if mode := s.resolveTemplateMode(ctx, item.TemplateRef, namespace); mode != "" {
		sandbox.Spec.Mode = mode
	}
	if item.Storage != nil {
		sandbox.Spec.Storage = item.Storage
	}
	if item.Timeout != "" {
		d, err := parseDuration(item.Timeout)
		if err != nil {
			return bulkCreateItemResult{Index: index, Status: "error", Error: "invalid timeout: " + err.Error()}
		}
		sandbox.Spec.Timeout = d
	}
	if item.IdleTimeout != "" {
		d, err := parseDuration(item.IdleTimeout)
		if err != nil {
			return bulkCreateItemResult{Index: index, Status: "error", Error: "invalid idleTimeout: " + err.Error()}
		}
		sandbox.Spec.IdleTimeout = d
	}

	// Per-item governance re-check (template/registry allowlists, resource
	// and timeout value caps) — the aggregate CheckBulk in the caller only
	// covers sandbox-COUNT quotas, never these. This is what makes bulk
	// create's per-item enforcement match a single handleCreateSandbox call
	// instead of silently skipping it.
	if !policy.IsEmpty() {
		if v := governance.Check(policy, usage, sandbox); v.Violated() {
			return bulkCreateItemResult{Index: index, Status: "error", Error: "policy_violation: " + v.Error()}
		}
	}

	if err := s.k8sClient.Create(ctx, sandbox); err != nil {
		return bulkCreateItemResult{Index: index, Status: "error", Error: "failed to create sandbox: " + err.Error()}
	}

	return bulkCreateItemResult{Index: index, Status: "created", SandboxID: sandbox.Name}
}

// bulkActionRequest is the body of POST /sandboxes/bulk-action:
// `{"action":"stop|resume|delete","ids":[...]}`.
type bulkActionRequest struct {
	Action string   `json:"action"`
	IDs    []string `json:"ids"`
}

// bulkActionItemResult is one entry in POST /sandboxes/bulk-action's
// response array: `{"id","status","error?"}` (FR4.2/FR4.3).
type bulkActionItemResult struct {
	ID     string `json:"id"`
	Status string `json:"status"` // "ok" | "error"
	Error  string `json:"error,omitempty"`
}

var validBulkActions = map[string]bool{"stop": true, "resume": true, "delete": true}

// handleBulkAction implements POST /sandboxes/bulk-action: apply stop,
// resume, or delete to a list of sandbox IDs in one call.
//
// Every ID is independent (FR4.3) — an unknown ID, a sandbox belonging to
// another user, or one already in an invalid phase for the requested action
// is reported as a per-item failure, never a whole-batch abort. Per-item
// RBAC uses the exact same getSandboxWithAuthCheck owner-or-admin gate as
// the single-sandbox stop/resume/delete handlers, so bulk cannot be used to
// bypass ownership checks that would apply to the equivalent individual
// calls.
//
// Sandbox-scoped keys are rejected outright (403), same as handleBulkCreate
// — bulk actions are not sandbox-scoped operations (DD3, NFR2).
func (s *Server) handleBulkAction(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if claims.SandboxID != "" {
		respondError(w, http.StatusForbidden, "sandbox-scoped keys may not perform bulk operations")
		return
	}

	var req bulkActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if !validBulkActions[req.Action] {
		respondError(w, http.StatusBadRequest, `action must be one of "stop", "resume", "delete"`)
		return
	}
	if len(req.IDs) == 0 {
		respondError(w, http.StatusBadRequest, "ids must be a non-empty array")
		return
	}

	results := make([]bulkActionItemResult, len(req.IDs))
	for i, id := range req.IDs {
		results[i] = s.applyOneBulkAction(r.Context(), claims, req.Action, id)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"results": results})
	s.recordAudit(r.Context(), claims, "bulk."+req.Action, "sandbox", "")
}

// applyOneBulkAction runs stop/resume/delete against a single sandbox ID,
// mirroring the exact RBAC and phase-validation logic of
// handleStopSandbox/handleResumeSandbox/handleDeleteSandbox but returning a
// result struct instead of writing an HTTP response.
func (s *Server) applyOneBulkAction(ctx context.Context, claims *Claims, action, id string) bulkActionItemResult {
	sandbox, err := s.getSandboxWithAuthCheck(ctx, id, claims)
	if err != nil {
		return bulkActionItemResult{ID: id, Status: "error", Error: err.Error()}
	}

	switch action {
	case "stop":
		if sandbox.Status.Phase != agenttierv1alpha1.SandboxPhaseRunning {
			return bulkActionItemResult{ID: id, Status: "error", Error: fmt.Sprintf("cannot stop sandbox in phase %s", sandbox.Status.Phase)}
		}
		if sandbox.Annotations == nil {
			sandbox.Annotations = make(map[string]string)
		}
		sandbox.Annotations["agenttier.io/action"] = "stop"
		if err := s.k8sClient.Update(ctx, sandbox); err != nil {
			return bulkActionItemResult{ID: id, Status: "error", Error: "failed to stop sandbox: " + err.Error()}
		}
		return bulkActionItemResult{ID: id, Status: "ok"}

	case "resume":
		if sandbox.Status.Phase != agenttierv1alpha1.SandboxPhaseStopped && sandbox.Status.Phase != agenttierv1alpha1.SandboxPhaseError {
			return bulkActionItemResult{ID: id, Status: "error", Error: fmt.Sprintf("cannot resume sandbox in phase %s", sandbox.Status.Phase)}
		}
		sandbox.Status.Phase = agenttierv1alpha1.SandboxPhaseCreating
		sandbox.Status.Message = "Resuming"
		if err := s.k8sClient.Status().Update(ctx, sandbox); err != nil {
			return bulkActionItemResult{ID: id, Status: "error", Error: "failed to resume sandbox: " + err.Error()}
		}
		return bulkActionItemResult{ID: id, Status: "ok"}

	case "delete":
		if err := s.k8sClient.Delete(ctx, sandbox); err != nil {
			return bulkActionItemResult{ID: id, Status: "error", Error: "failed to delete sandbox: " + err.Error()}
		}
		return bulkActionItemResult{ID: id, Status: "ok"}
	}

	// Unreachable: handleBulkAction already validated action against
	// validBulkActions before calling this.
	return bulkActionItemResult{ID: id, Status: "error", Error: "unknown action"}
}
