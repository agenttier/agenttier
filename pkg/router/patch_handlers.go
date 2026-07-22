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
	"encoding/json"
	"net/http"

	"github.com/gorilla/mux"
	corev1 "k8s.io/api/core/v1"

	"github.com/agenttier/agenttier/pkg/governance"
)

// patchSandboxRequest is the FR2 PATCH body: every field is optional, but at
// least one must be present (design.md's contract).
type patchSandboxRequest struct {
	IdleTimeout string                       `json:"idleTimeout,omitempty"`
	Resources   *corev1.ResourceRequirements `json:"resources,omitempty"`
	Labels      map[string]string            `json:"labels,omitempty"`
	Annotations map[string]string            `json:"annotations,omitempty"`
}

func (req *patchSandboxRequest) isEmpty() bool {
	return req.IdleTimeout == "" && req.Resources == nil && req.Labels == nil && req.Annotations == nil
}

// handlePatchSandbox implements PATCH /api/v1/sandboxes/{id} (FR2): a partial
// update of idleTimeout/resources/labels/annotations on a running sandbox.
//
// idleTimeout/labels/annotations take effect immediately (reconcileRunning
// reads Spec.IdleTimeout live; labels/annotations are plain ObjectMeta).
// resources do NOT take effect until the sandbox's Pod is rebuilt (stop +
// resume, or an infra-failure auto-restart) — the controller builds the Pod
// exactly once in reconcileCreating and never diffs/rebuilds it on a later
// spec change (verified, see spec.md OQ1/DL3). The response reports
// per-field applicability instead of implying a live resize that doesn't
// happen.
func (s *Server) handlePatchSandbox(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	sandboxID := mux.Vars(r)["id"]
	sandbox, err := s.getSandboxWithAuthCheck(r.Context(), sandboxID, claims)
	if err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}

	var req patchSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.isEmpty() {
		respondError(w, http.StatusBadRequest, "at least one of idleTimeout, resources, labels, annotations is required")
		return
	}

	// Build the proposed spec on a copy first so governance.Check (which
	// takes a *Sandbox) can validate the patched values before anything is
	// persisted — mirrors handleCreateSandbox's short-circuit-before-mutate
	// pattern (handlers.go:152-173).
	proposed := sandbox.DeepCopy()
	if req.IdleTimeout != "" {
		d, err := parseDuration(req.IdleTimeout)
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid idleTimeout: "+err.Error())
			return
		}
		proposed.Spec.IdleTimeout = d
	}
	if req.Resources != nil {
		proposed.Spec.Resources = req.Resources
	}

	// Governance re-check (NFR3): a policy tightened after create still
	// gates PATCH exactly as it gates create. Only idleTimeout/resources are
	// governance-relevant; labels/annotations carry no policy dimension.
	//
	// PATCH creates no new sandbox, so it must use CheckResourceLimits, NOT
	// Check — Check also evaluates the sandbox-COUNT quotas
	// (MaxSandboxesTotal/MaxSandboxesPerUser/MaxAgentSandboxes), which would
	// spuriously reject a value-only PATCH (e.g. just idleTimeout) once a
	// namespace is already at its count cap, even though PATCH doesn't add a
	// sandbox to that count.
	if s.governanceStore != nil && (req.IdleTimeout != "" || req.Resources != nil) {
		policy, err := governance.Resolve(r.Context(), s.governanceStore, sandbox.Namespace)
		if err != nil {
			s.logger.Warn("failed to resolve governance policy; proceeding without enforcement", "namespace", sandbox.Namespace, "error", err)
		} else if !policy.IsEmpty() {
			if v := governance.CheckResourceLimits(policy, proposed); v.Violated() {
				respondJSON(w, http.StatusForbidden, map[string]interface{}{
					"error":      "policy_violation",
					"violations": v,
				})
				return
			}
		}
	}

	// Apply labels/annotations directly on the live object (merge, not
	// replace — a caller adjusting one label shouldn't wipe out unrelated
	// ones the controller or another caller set).
	if req.Labels != nil {
		if sandbox.Labels == nil {
			sandbox.Labels = map[string]string{}
		}
		for k, v := range req.Labels {
			sandbox.Labels[k] = v
		}
	}
	if req.Annotations != nil {
		if sandbox.Annotations == nil {
			sandbox.Annotations = map[string]string{}
		}
		for k, v := range req.Annotations {
			sandbox.Annotations[k] = v
		}
	}
	if req.IdleTimeout != "" {
		sandbox.Spec.IdleTimeout = proposed.Spec.IdleTimeout
	}
	if req.Resources != nil {
		sandbox.Spec.Resources = req.Resources
	}

	// Read-modify-Update, last-write-wins (DD2) — matches every other
	// spec-mutation handler in this package (handleStopSandbox,
	// handleShareSandbox, handleUpdateTemplate): no resourceVersion retry.
	if err := s.k8sClient.Update(r.Context(), sandbox); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update sandbox: "+err.Error())
		return
	}

	applied := map[string]string{}
	restartRequired := false
	if req.IdleTimeout != "" {
		applied["idleTimeout"] = "immediately"
	}
	if req.Labels != nil {
		applied["labels"] = "immediately"
	}
	if req.Annotations != nil {
		applied["annotations"] = "immediately"
	}
	if req.Resources != nil {
		applied["resources"] = "on-restart"
		restartRequired = true
	}

	resp := map[string]interface{}{
		"sandboxId":       sandbox.Name,
		"applied":         applied,
		"restartRequired": restartRequired,
	}
	if restartRequired {
		resp["message"] = "resource changes take effect after the sandbox is stopped and resumed"
	}
	respondJSON(w, http.StatusOK, resp)
	s.recordAudit(r.Context(), claims, "patch", "sandbox", sandbox.Name)
}
