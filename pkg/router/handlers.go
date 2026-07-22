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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
	"github.com/agenttier/agenttier/pkg/controller/warmpool"
	"github.com/agenttier/agenttier/pkg/governance"
	"github.com/agenttier/agenttier/pkg/router/portforward"
	"github.com/agenttier/agenttier/pkg/router/sharelinks"
	"github.com/agenttier/agenttier/pkg/router/terminal"
	"github.com/agenttier/agenttier/pkg/storage"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// validSandboxName matches RFC 1123 label rules — what Kubernetes requires
// for resource names. Lowercase alphanumeric + dash, start/end alphanumeric,
// max 63 chars. Used to validate user-supplied clone names before we
// attempt to create the new Sandbox.
var validSandboxName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// --- Sandbox CRUD Handlers ---

type CreateSandboxRequest struct {
	Name        string                               `json:"name"`
	Namespace   string                               `json:"namespace,omitempty"`
	TemplateRef *agenttierv1alpha1.TemplateReference `json:"templateRef"`
	Timeout     string                               `json:"timeout,omitempty"`
	IdleTimeout string                               `json:"idleTimeout,omitempty"`
	Storage     *agenttierv1alpha1.StorageSpec       `json:"storage,omitempty"`
}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req CreateSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.Name == "" {
		respondError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.TemplateRef == nil || req.TemplateRef.Name == "" {
		respondError(w, http.StatusBadRequest, "templateRef.name is required")
		return
	}

	namespace := req.Namespace
	if namespace == "" {
		namespace = s.config.SandboxNamespace
	}
	if namespace == "" {
		namespace = "default"
	}

	// Build the Sandbox CR
	sandbox := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
		},
		Spec: agenttierv1alpha1.SandboxSpec{
			TemplateRef: req.TemplateRef,
			CreatedBy: &agenttierv1alpha1.UserIdentity{
				Sub:         claims.Sub,
				Email:       claims.Email,
				DisplayName: claims.Name,
			},
		},
	}

	// Inherit mode from the referenced template so the agent endpoints
	// know which API surface the sandbox accepts. Without this the CRD
	// default of "code" wins and POST /configure / /invoke return 400 on
	// agent-template sandboxes. Best-effort: if the template lookup fails
	// (NotFound, RBAC), the controller's reconciler will still resolve and
	// the sandbox stays in code mode — which is the conservative default.
	if mode := s.resolveTemplateMode(r.Context(), req.TemplateRef, namespace); mode != "" {
		sandbox.Spec.Mode = mode
	}

	if req.Storage != nil {
		sandbox.Spec.Storage = req.Storage
	}

	// Parse timeout / idleTimeout if provided. A bad value is a 400, not a
	// silent drop — operators rely on these for cost + governance ceilings.
	if req.Timeout != "" {
		d, err := parseDuration(req.Timeout)
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid timeout: "+err.Error())
			return
		}
		sandbox.Spec.Timeout = d
	}
	if req.IdleTimeout != "" {
		d, err := parseDuration(req.IdleTimeout)
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid idleTimeout: "+err.Error())
			return
		}
		sandbox.Spec.IdleTimeout = d
	}

	// Governance enforcement. Violations short-circuit before the CR ever
	// reaches the API server so users get a crisp 403 with details instead of
	// a half-created sandbox that trips over a later webhook.
	if s.governanceStore != nil {
		policy, err := governance.Resolve(r.Context(), s.governanceStore, namespace)
		if err != nil {
			s.logger.Warn("failed to resolve governance policy; proceeding without enforcement", "namespace", namespace, "error", err)
		} else if !policy.IsEmpty() {
			existing := &agenttierv1alpha1.SandboxList{}
			if err := s.k8sClient.List(r.Context(), existing, client.InNamespace(namespace)); err != nil {
				respondError(w, http.StatusInternalServerError, "failed to check namespace usage: "+err.Error())
				return
			}
			usage := governance.CountUsage(existing, claims.Sub)
			if v := governance.Check(policy, usage, sandbox); v.Violated() {
				respondJSON(w, http.StatusForbidden, map[string]interface{}{
					"error":      "policy_violation",
					"violations": v,
				})
				return
			}
		}
	}

	// Create the Sandbox CR in Kubernetes
	if err := s.k8sClient.Create(r.Context(), sandbox); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create sandbox: "+err.Error())
		return
	}

	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"sandboxId":   sandbox.Name,
		"name":        sandbox.Name,
		"namespace":   sandbox.Namespace,
		"status":      "Creating",
		"templateRef": sandbox.Spec.TemplateRef.Name,
	})
	s.recordAudit(r.Context(), claims, "create", "sandbox", sandbox.Name)
}

func (s *Server) handleListSandboxes(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	// List all sandboxes (controller will filter by namespace RBAC)
	sandboxList := &agenttierv1alpha1.SandboxList{}
	listOpts := []client.ListOption{}

	// Filter by namespace if specified
	ns := r.URL.Query().Get("namespace")
	if ns != "" {
		listOpts = append(listOpts, client.InNamespace(ns))
	}

	if err := s.k8sClient.List(r.Context(), sandboxList, listOpts...); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list sandboxes: "+err.Error())
		return
	}

	// Filter by ownership (non-admin only sees their own + shared)
	var results []map[string]interface{}
	for _, sb := range sandboxList.Items {
		// Non-admin: only show own sandboxes
		if !claims.IsAdmin {
			if sb.Spec.CreatedBy == nil || sb.Spec.CreatedBy.Sub != claims.Sub {
				continue
			}
		}

		// Apply status filter
		statusFilter := r.URL.Query().Get("status")
		if statusFilter != "" && string(sb.Status.Phase) != statusFilter {
			continue
		}

		results = append(results, sandboxToJSON(&sb))
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"sandboxes": results})
}

func (s *Server) handleGetSandbox(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	sandboxID := mux.Vars(r)["id"]
	sandbox, err := s.getSandboxForRead(r.Context(), sandboxID, claims)
	if err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, sandboxToJSON(sandbox))
}

func (s *Server) handleDeleteSandbox(w http.ResponseWriter, r *http.Request) {
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

	if err := s.k8sClient.Delete(r.Context(), sandbox); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete sandbox: "+err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStopSandbox(w http.ResponseWriter, r *http.Request) {
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

	if sandbox.Status.Phase != agenttierv1alpha1.SandboxPhaseRunning {
		respondError(w, http.StatusConflict, fmt.Sprintf("cannot stop sandbox in phase %s", sandbox.Status.Phase))
		return
	}

	// Annotate to signal stop (controller watches for this)
	if sandbox.Annotations == nil {
		sandbox.Annotations = make(map[string]string)
	}
	sandbox.Annotations["agenttier.io/action"] = "stop"
	if err := s.k8sClient.Update(r.Context(), sandbox); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to stop sandbox: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "stopping"})
}

func (s *Server) handleResumeSandbox(w http.ResponseWriter, r *http.Request) {
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

	if sandbox.Status.Phase != agenttierv1alpha1.SandboxPhaseStopped && sandbox.Status.Phase != agenttierv1alpha1.SandboxPhaseError {
		respondError(w, http.StatusConflict, fmt.Sprintf("cannot resume sandbox in phase %s", sandbox.Status.Phase))
		return
	}

	// Set phase back to Creating to trigger reconciliation
	sandbox.Status.Phase = agenttierv1alpha1.SandboxPhaseCreating
	sandbox.Status.Message = "Resuming"
	if err := s.k8sClient.Status().Update(r.Context(), sandbox); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to resume sandbox: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "resuming"})
}

func (s *Server) handleCloneSandbox(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	sourceID := mux.Vars(r)["id"]
	source, err := s.getSandboxWithAuthCheck(r.Context(), sourceID, claims)
	if err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}
	if source.Status.PVCName == "" {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("sandbox %s has no PVC to clone (phase=%s)", source.Name, source.Status.Phase))
		return
	}

	// Body shape: { "name": "<new-sandbox-id>", "snapshotClass": "<optional>" }.
	// Both fields optional — name auto-generated as "<source>-clone-<ts>" if
	// empty; snapshotClass falls back to the cluster's default
	// VolumeSnapshotClass when empty.
	var req struct {
		Name          string `json:"name,omitempty"`
		SnapshotClass string `json:"snapshotClass,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
	}

	cloneName := strings.TrimSpace(req.Name)
	if cloneName == "" {
		cloneName = fmt.Sprintf("%s-clone-%d", source.Name, time.Now().Unix())
	}
	if !validSandboxName.MatchString(cloneName) {
		respondError(w, http.StatusBadRequest, "name must match RFC 1123 label rules: lowercase alphanumeric or '-', start/end alphanumeric, max 63 chars")
		return
	}

	// Step 1: take a VolumeSnapshot of the source PVC. Snapshot lives in
	// the same namespace as the source sandbox; the EBS CSI driver
	// (or any other CSI snapshotter) hydrates it asynchronously.
	snapName := fmt.Sprintf("%s-snap-%d", source.Name, time.Now().Unix())
	snap := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      snapName,
			Namespace: source.Namespace,
			Labels: map[string]string{
				"agenttier.io/source-sandbox": source.Name,
				"agenttier.io/clone-target":   cloneName,
			},
		},
		Spec: snapshotv1.VolumeSnapshotSpec{
			Source: snapshotv1.VolumeSnapshotSource{
				PersistentVolumeClaimName: &source.Status.PVCName,
			},
		},
	}
	if req.SnapshotClass != "" {
		snap.Spec.VolumeSnapshotClassName = &req.SnapshotClass
	}
	if err := s.k8sClient.Create(r.Context(), snap); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create VolumeSnapshot: "+err.Error())
		return
	}

	// Step 2: build the cloned Sandbox CR. Copy the source's spec as a
	// baseline so the clone inherits template, image, env, ports, etc.
	// CreatedBy is re-stamped from the caller; CloneFromSnapshot is what
	// tells the controller this is a hydrated PVC.
	cloneSpec := *source.Spec.DeepCopy()
	cloneSpec.CloneFromSnapshot = snapName
	cloneSpec.CreatedBy = &agenttierv1alpha1.UserIdentity{
		Sub:         claims.Sub,
		Email:       claims.Email,
		DisplayName: claims.Name,
	}

	clone := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cloneName,
			Namespace: source.Namespace,
			Labels: map[string]string{
				"agenttier.io/cloned-from": source.Name,
			},
		},
		Spec: cloneSpec,
	}
	if err := s.k8sClient.Create(r.Context(), clone); err != nil {
		// Clean up the orphan snapshot — no point keeping it without
		// a target sandbox.
		_ = s.k8sClient.Delete(r.Context(), snap)
		respondError(w, http.StatusInternalServerError, "failed to create cloned sandbox: "+err.Error())
		return
	}

	// Step 3: stamp clonedFrom on the new Sandbox status. Best-effort —
	// the field's purpose is auditability, not correctness, so a status
	// update failure here doesn't fail the clone operation.
	clone.Status.ClonedFrom = source.Name
	if err := s.k8sClient.Status().Update(r.Context(), clone); err != nil {
		s.logger.LogAttrs(r.Context(), slog.LevelWarn, "clone created but ClonedFrom status update failed",
			slog.String("source", source.Name),
			slog.String("clone", clone.Name),
			slog.String("error", err.Error()),
		)
	}

	respondJSON(w, http.StatusAccepted, map[string]any{
		"name":       cloneName,
		"namespace":  clone.Namespace,
		"snapshot":   snapName,
		"clonedFrom": source.Name,
		"phase":      "Pending",
		"message":    "Clone in progress. Poll GET /api/v1/sandboxes/" + cloneName + " for status; PVC provisioning waits on the VolumeSnapshot becoming readyToUse (typically 30-90 seconds on EBS).",
	})
}

// --- Command Execution ---

type ExecRequest struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

func (s *Server) handleExecCommand(w http.ResponseWriter, r *http.Request) {
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

	if sandbox.Status.Phase != agenttierv1alpha1.SandboxPhaseRunning {
		respondError(w, http.StatusConflict, "sandbox is not running")
		return
	}

	var req ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Command == "" {
		respondError(w, http.StatusBadRequest, "command is required")
		return
	}
	if req.Timeout == 0 {
		req.Timeout = 30
	}

	// Execute via bridge
	if s.bridge == nil {
		respondError(w, http.StatusServiceUnavailable, "terminal bridge not initialized")
		return
	}

	// Dispatch picks HTTP-exec when the sandbox is opted in and the
	// in-pod runtime is reachable, else falls back to SPDY transparently.
	// See pkg/router/exec_dispatch.go for the decision tree.
	result, err := s.dispatchExec(r.Context(), sandbox, []string{"/bin/sh", "-c", req.Command}, req.Timeout)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "exec failed: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, result)
}

// --- WebSocket Terminal ---

func (s *Server) handleTerminalWebSocket(w http.ResponseWriter, r *http.Request) {
	sandboxID := mux.Vars(r)["sandboxId"]

	// Auth: mirror the REST authMiddleware gate exactly so the terminal fails
	// closed. Dev-admin is granted ONLY behind the explicit --dev-auth flag
	// (config.DevAuth); a production install that simply has no OIDC issuer
	// must reject anonymous connections, not hand out an admin shell. When
	// dev-auth is off we require a valid API key or OIDC JWT. The WebSocket
	// handshake can't reliably set custom headers from a browser, so the JWT
	// may also arrive as a ?token= query param.
	var claims *Claims
	if s.config.DevAuth {
		claims = &Claims{
			Sub:     "dev-user",
			Email:   "dev@agenttier.local",
			Name:    "Dev User",
			Groups:  []string{"agenttier-admins"},
			IsAdmin: true,
		}
	} else if apiKey := r.Header.Get("X-API-Key"); apiKey != "" {
		var err error
		claims, err = s.validateAPIKey(r.Context(), apiKey)
		if err != nil {
			http.Error(w, "invalid api key", http.StatusUnauthorized)
			return
		}
	} else {
		// Extract a bearer token from the ?token= query param or the
		// Authorization header.
		token := r.URL.Query().Get("token")
		if token == "" {
			authHeader := r.Header.Get("Authorization")
			token = strings.TrimPrefix(authHeader, "Bearer ")
			if token == authHeader {
				token = ""
			}
		}
		if token == "" {
			http.Error(w, "missing authentication", http.StatusUnauthorized)
			return
		}

		var err error
		claims, err = s.validateJWT(r.Context(), token)
		if err != nil {
			http.Error(w, "invalid token: "+err.Error(), http.StatusUnauthorized)
			return
		}
	}

	// Lookup sandbox (resolves across namespaces, honoring ?namespace=).
	sandbox, err := s.resolveSandbox(r.Context(), sandboxID, r.URL.Query().Get("namespace"))
	if err != nil {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}

	// Check access — owner, admin, or a user/group the sandbox is shared with.
	if !userCanAccessSandbox(sandbox, claims) {
		http.Error(w, "access denied", http.StatusForbidden)
		return
	}

	// Check sandbox is running
	if sandbox.Status.Phase != agenttierv1alpha1.SandboxPhaseRunning {
		http.Error(w, "sandbox is not running", http.StatusConflict)
		return
	}

	// Upgrade to WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("WebSocket upgrade failed", "error", err)
		return
	}

	// Determine shell
	shell := "/bin/bash"

	// Create session
	session := s.sessionManager.CreateSession(
		sandboxID, sandbox.Namespace,
		claims.Sub, claims.Email,
		sandbox.Status.PodName, shell,
		conn, false,
	)

	s.logger.Info("terminal session started",
		"sessionId", session.ID,
		"sandboxId", sandboxID,
		"userId", claims.Sub,
	)

	// Start the keepalive goroutine: WS control pings + app-level heartbeats
	// every 30s. Ties its lifecycle to the request context so shutdown and
	// client disconnect cleanly stop the ticker.
	keepaliveCtx, cancelKeepalive := context.WithCancel(r.Context())
	defer cancelKeepalive()
	session.StartKeepalive(keepaliveCtx, s.logger)

	// Bridge the WebSocket to the pod exec stream. The dispatcher picks
	// HTTP-PTY (in-pod runtime) when the sandbox is opted in and the
	// runtime is healthy, falling back to SPDY transparently otherwise.
	// See pkg/router/pty_dispatch.go for the decision tree.
	if s.bridge != nil {
		if err := s.dispatchTerminal(r.Context(), sandbox, session); err != nil {
			s.logger.Error("terminal dispatch error", "sessionId", session.ID, "error", err)
		}
	}

	// Cleanup
	s.sessionManager.RemoveSession(session.ID)
	_ = conn.Close() // best-effort close; if the peer already hung up, ignore
}

// --- Template Handlers ---

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	templateList := &agenttierv1alpha1.ClusterSandboxTemplateList{}
	if err := s.k8sClient.List(r.Context(), templateList); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list templates: "+err.Error())
		return
	}

	var results []map[string]interface{}
	for _, t := range templateList.Items {
		results = append(results, templateToJSON(&t))
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"templates": results})
}

func (s *Server) handleCreateTemplate(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req struct {
		Name string                                `json:"name"`
		Spec agenttierv1alpha1.SandboxTemplateSpec `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Name == "" {
		respondError(w, http.StatusBadRequest, "name is required")
		return
	}

	tmpl := &agenttierv1alpha1.ClusterSandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Name,
			Labels: map[string]string{
				"agenttier.io/managed": "true",
			},
		},
		Spec: req.Spec,
	}

	if err := s.k8sClient.Create(r.Context(), tmpl); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create template: "+err.Error())
		return
	}

	respondJSON(w, http.StatusCreated, templateToJSON(tmpl))
}

func (s *Server) handleGetTemplate(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	tmpl := &agenttierv1alpha1.ClusterSandboxTemplate{}
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Name: name}, tmpl); err != nil {
		respondError(w, http.StatusNotFound, "template not found")
		return
	}
	respondJSON(w, http.StatusOK, templateToJSON(tmpl))
}

func (s *Server) handleUpdateTemplate(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	name := mux.Vars(r)["name"]

	// Fetch existing template
	tmpl := &agenttierv1alpha1.ClusterSandboxTemplate{}
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Name: name}, tmpl); err != nil {
		respondError(w, http.StatusNotFound, "template not found")
		return
	}

	// Decode the new spec from the request body
	var req struct {
		Spec agenttierv1alpha1.SandboxTemplateSpec `json:"spec"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	// Update the spec in-place (preserves metadata like resourceVersion)
	tmpl.Spec = req.Spec

	if err := s.k8sClient.Update(r.Context(), tmpl); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update template: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, templateToJSON(tmpl))
}

func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	name := mux.Vars(r)["name"]

	// Check if any running sandboxes reference this template
	sandboxList := &agenttierv1alpha1.SandboxList{}
	if err := s.k8sClient.List(r.Context(), sandboxList); err == nil {
		for _, sb := range sandboxList.Items {
			if sb.Status.ResolvedTemplate == name && sb.Status.Phase == agenttierv1alpha1.SandboxPhaseRunning {
				respondError(w, http.StatusConflict, fmt.Sprintf("cannot delete template: sandbox %q is still using it", sb.Name))
				return
			}
		}
	}

	tmpl := &agenttierv1alpha1.ClusterSandboxTemplate{}
	if err := s.k8sClient.Get(r.Context(), types.NamespacedName{Name: name}, tmpl); err != nil {
		respondError(w, http.StatusNotFound, "template not found")
		return
	}

	if err := s.k8sClient.Delete(r.Context(), tmpl); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete template: "+err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// templateToJSON serializes a ClusterSandboxTemplate to a JSON-friendly map
// including the full spec so the UI can render it in a YAML editor.
func templateToJSON(t *agenttierv1alpha1.ClusterSandboxTemplate) map[string]interface{} {
	result := map[string]interface{}{
		"name":            t.Name,
		"description":     t.Spec.Description,
		"image":           imageFromSpec(t.Spec.Image),
		"resourceVersion": t.ResourceVersion,
		"spec":            t.Spec,
	}
	return result
}

// --- Placeholder handlers for features not yet wired ---

// --- File Transfer Handlers (task 7.4) ---
//
// Implementation strategy: drive sandbox-side `sh`, `ls`, `cat`, `base64`
// through the same SPDY exec bridge the terminal uses. This avoids adding a
// second transport, reuses the auth flow, and works identically on any
// cluster without needing a sidecar or an extra port.
//
// The `mountPath` default is /workspace to match the reference templates. A
// caller may request any path inside the container; we reject anything that
// escapes through ".." to keep a hostile client from reading arbitrary
// container paths via URL traversal.

// Maximum size of an uploaded / downloaded file. Larger transfers should use
// streaming (planned follow-up); exec-over-tar isn't great for multi-GB
// payloads because the whole stream sits in the Router's memory.
const fileTransferMaxBytes = 32 * 1024 * 1024 // 32 MiB

func sandboxFilePath(raw string) (string, error) {
	p := path.Clean("/" + strings.TrimPrefix(raw, "/"))
	// path.Clean collapses "..", but we still want to reject any path that
	// doesn't live under the sandbox workspace tree. Allow any absolute path
	// since some tools legitimately need /tmp, /home, etc., but never allow
	// shell metacharacters that would escape the `sh -c` we'll pipe through.
	for _, ch := range p {
		if ch == '\'' || ch == '\\' || ch == '`' || ch == '\n' || ch == '\r' {
			return "", fmt.Errorf("path contains disallowed characters")
		}
	}
	return p, nil
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
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
	if sandbox.Status.Phase != agenttierv1alpha1.SandboxPhaseRunning {
		respondError(w, http.StatusConflict, "sandbox is not running")
		return
	}
	if s.bridge == nil {
		respondError(w, http.StatusServiceUnavailable, "terminal bridge not initialized")
		return
	}

	dir := r.URL.Query().Get("path")
	if dir == "" {
		dir = "/workspace"
	}
	cleaned, err := sandboxFilePath(dir)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// `ls -la --time-style=+%s` gives us parseable tokens: mode, links, user,
	// group, size, mtime-seconds, name. We filter out `.` and `..` so the
	// response only lists real children.
	cmd := []string{"/bin/sh", "-c", fmt.Sprintf(
		"cd '%s' 2>/dev/null && ls -la --time-style=+%%s | tail -n +2",
		cleaned,
	)}
	result, err := s.dispatchExec(r.Context(), sandbox, cmd, 10)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}
	if result.ExitCode != 0 {
		respondError(w, http.StatusNotFound, "path not found: "+cleaned)
		return
	}

	entries := make([]map[string]interface{}, 0)
	for _, line := range strings.Split(strings.TrimRight(result.Stdout, "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		// ls -la format: <mode> <nlink> <user> <group> <size> <mtime> <name...>
		mode := fields[0]
		if mode == "total" { // BusyBox prints "total N" on the first line; harmless to skip.
			continue
		}
		name := strings.Join(fields[6:], " ")
		if name == "." || name == ".." {
			continue
		}
		size, _ := strconv.ParseInt(fields[4], 10, 64)
		mtime, _ := strconv.ParseInt(fields[5], 10, 64)
		entries = append(entries, map[string]interface{}{
			"name":       name,
			"size":       size,
			"isDir":      strings.HasPrefix(mode, "d"),
			"mode":       mode,
			"modifiedAt": mtime,
		})
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"path":    cleaned,
		"entries": entries,
	})
}

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
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
	if sandbox.Status.Phase != agenttierv1alpha1.SandboxPhaseRunning {
		respondError(w, http.StatusConflict, "sandbox is not running")
		return
	}
	if s.bridge == nil {
		respondError(w, http.StatusServiceUnavailable, "terminal bridge not initialized")
		return
	}

	raw := mux.Vars(r)["path"]
	cleaned, err := sandboxFilePath("/" + raw)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Use `base64` so we don't have to deal with PTY munging or byte-for-byte
	// JSON escape of binary payloads. We enforce fileTransferMaxBytes via a
	// pre-flight `stat` — a hostile / giant file is rejected before we read.
	statCmd := []string{"/bin/sh", "-c", fmt.Sprintf(
		"stat -c %%s '%s' 2>/dev/null || wc -c < '%s'", cleaned, cleaned,
	)}
	statResult, err := s.dispatchExec(r.Context(), sandbox, statCmd, 5)
	if err != nil || statResult.ExitCode != 0 {
		respondError(w, http.StatusNotFound, "file not found: "+cleaned)
		return
	}
	size, _ := strconv.ParseInt(strings.TrimSpace(statResult.Stdout), 10, 64)
	if size > fileTransferMaxBytes {
		respondError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"file is %d bytes, max %d — use the terminal for large files", size, fileTransferMaxBytes,
		))
		return
	}

	readCmd := []string{"/bin/sh", "-c", fmt.Sprintf("base64 -w0 '%s' 2>/dev/null || base64 '%s'", cleaned, cleaned)}
	result, err := s.dispatchExec(r.Context(), sandbox, readCmd, 30)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "read failed: "+err.Error())
		return
	}
	if result.ExitCode != 0 {
		respondError(w, http.StatusNotFound, "file not found: "+cleaned)
		return
	}

	encoded := strings.ReplaceAll(strings.TrimSpace(result.Stdout), "\n", "")
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "decode failed: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	// Restrict the Content-Disposition filename to a safe subset so user-
	// supplied path components can't inject header control characters. We
	// then wrap it in %q which escapes quotes; combined this is safe against
	// header-splitting and XSS-via-filename tricks. gosec's G705 taint check
	// also considers this clean because the only character classes that
	// survive the sanitizer are [A-Za-z0-9._-].
	safe := sanitizeFilename(path.Base(cleaned))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", safe))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// sanitizeFilename strips anything outside [A-Za-z0-9._-] from a filename so
// the Content-Disposition header cannot carry attacker-controlled control
// characters. Returns "file" when sanitation would leave an empty string.
func sanitizeFilename(name string) string {
	cleaned := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		isAlpha := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		isDigit := c >= '0' && c <= '9'
		if isAlpha || isDigit || c == '.' || c == '_' || c == '-' {
			cleaned = append(cleaned, c)
		}
	}
	if len(cleaned) == 0 {
		return "file"
	}
	return string(cleaned)
}

func (s *Server) handlePutFile(w http.ResponseWriter, r *http.Request) {
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
	if sandbox.Status.Phase != agenttierv1alpha1.SandboxPhaseRunning {
		respondError(w, http.StatusConflict, "sandbox is not running")
		return
	}
	if s.bridge == nil {
		respondError(w, http.StatusServiceUnavailable, "terminal bridge not initialized")
		return
	}

	raw := mux.Vars(r)["path"]
	cleaned, err := sandboxFilePath("/" + raw)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, fileTransferMaxBytes+1))
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to read request body: "+err.Error())
		return
	}
	if int64(len(body)) > fileTransferMaxBytes {
		respondError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"payload exceeds %d bytes", fileTransferMaxBytes,
		))
		return
	}

	encoded := base64.StdEncoding.EncodeToString(body)
	// Create the parent directory first, then decode the base64 payload onto
	// disk. We pipe the encoded bytes over stdin via a here-doc so we never
	// exceed the shell's ARG_MAX on larger files.
	dir := path.Dir(cleaned)
	writeCmd := []string{"/bin/sh", "-c", fmt.Sprintf(
		"mkdir -p '%s' && printf '%%s' %q | base64 -d > '%s'",
		dir, encoded, cleaned,
	)}
	result, err := s.dispatchExec(r.Context(), sandbox, writeCmd, 60)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "write failed: "+err.Error())
		return
	}
	if result.ExitCode != 0 {
		respondError(w, http.StatusInternalServerError, "write failed: "+strings.TrimSpace(result.Stderr))
		return
	}
	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"path":  cleaned,
		"bytes": len(body),
	})
}
func (s *Server) handleListPorts(w http.ResponseWriter, r *http.Request) {
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
	ports, err := s.portForward.List(r.Context(), sandbox)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list ports: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"ports": ports})
}

func (s *Server) handleForwardPort(w http.ResponseWriter, r *http.Request) {
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

	var req struct {
		Port     int32  `json:"port"`
		Protocol string `json:"protocol"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Port <= 0 {
		respondError(w, http.StatusBadRequest, "port must be > 0")
		return
	}

	fp, err := s.portForward.Create(r.Context(), sandbox, req.Port, req.Protocol)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to create port forward: "+err.Error())
		return
	}

	// Mirror the new port into sandbox status so `kubectl get sandbox -o yaml`
	// and the Web UI both see the same list. If the status update fails we
	// still return success — the cluster is the source of truth for routing.
	updated := appendForwardedPort(sandbox, fp)
	if updated {
		if err := s.k8sClient.Status().Update(r.Context(), sandbox); err != nil {
			s.logger.Warn("failed to update sandbox status with forwarded port",
				"sandbox", sandbox.Name, "port", req.Port, "error", err)
		}
	}

	respondJSON(w, http.StatusCreated, fp)
}

func (s *Server) handleRemovePort(w http.ResponseWriter, r *http.Request) {
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

	portStr := mux.Vars(r)["port"]
	port, err := strconv.ParseInt(portStr, 10, 32)
	if err != nil || port <= 0 {
		respondError(w, http.StatusBadRequest, "invalid port: "+portStr)
		return
	}

	if err := s.portForward.Delete(r.Context(), sandbox, int32(port)); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to remove port: "+err.Error())
		return
	}

	if removeForwardedPort(sandbox, int32(port)) {
		if err := s.k8sClient.Status().Update(r.Context(), sandbox); err != nil {
			s.logger.Warn("failed to update sandbox status after removing port",
				"sandbox", sandbox.Name, "port", port, "error", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// handlePortPreview proxies an authenticated HTTP request from the Router to
// the cluster-internal Service backing the sandbox's forwarded port. This is
// what lets developers hit a sandbox port from a browser without DNS /
// Ingress setup — especially useful in dev and for E2E testing.
func (s *Server) handlePortPreview(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	vars := mux.Vars(r)
	sandboxID := vars["id"]
	portStr := vars["port"]
	port, err := strconv.ParseInt(portStr, 10, 32)
	if err != nil || port <= 0 {
		respondError(w, http.StatusBadRequest, "invalid port: "+portStr)
		return
	}
	sandbox, err := s.getSandboxWithAuthCheck(r.Context(), sandboxID, claims)
	if err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}
	if sandbox.Status.Phase != agenttierv1alpha1.SandboxPhaseRunning {
		respondError(w, http.StatusConflict, "sandbox is not running")
		return
	}
	// Look up the forwarded port to confirm it's been explicitly exposed.
	ports, err := s.portForward.List(r.Context(), sandbox)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to verify port: "+err.Error())
		return
	}
	known := false
	for _, p := range ports {
		if p.Port == int32(port) {
			known = true
			break
		}
	}
	if !known {
		respondError(w, http.StatusNotFound, fmt.Sprintf("port %d is not forwarded", port))
		return
	}

	stripPrefix := fmt.Sprintf("/api/v1/sandboxes/%s/preview/%d", sandboxID, port)
	proxy := s.portForward.Proxy(sandbox.Name, sandbox.Namespace, int32(port), stripPrefix)
	proxy.ServeHTTP(w, r)
}

func appendForwardedPort(sandbox *agenttierv1alpha1.Sandbox, fp *portforward.ForwardedPort) bool {
	for _, existing := range sandbox.Status.ForwardedPorts {
		if existing.Port == fp.Port {
			return false
		}
	}
	sandbox.Status.ForwardedPorts = append(sandbox.Status.ForwardedPorts, agenttierv1alpha1.ForwardedPort{
		Port:       fp.Port,
		PreviewURL: fp.PreviewURL,
		Protocol:   fp.Protocol,
	})
	return true
}

func removeForwardedPort(sandbox *agenttierv1alpha1.Sandbox, port int32) bool {
	for i, existing := range sandbox.Status.ForwardedPorts {
		if existing.Port == port {
			sandbox.Status.ForwardedPorts = append(sandbox.Status.ForwardedPorts[:i], sandbox.Status.ForwardedPorts[i+1:]...)
			return true
		}
	}
	return false
}

// handleGetSharing returns the sandbox's sharing config (users, groups, and
// share-link metadata). Token hashes are never returned. Owner/admin only.
func (s *Server) handleGetSharing(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	sandbox, err := s.getSandboxWithAuthCheck(r.Context(), mux.Vars(r)["id"], claims)
	if err != nil {
		respondError(w, http.StatusForbidden, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, sharingToJSON(sandbox.Spec.Sharing))
}

// handleShareSandbox grants a user or group access at a permission level.
// Body: {"identity": "...", "level": "viewer|collaborator", "kind": "user|group"}.
// Idempotent: re-sharing an identity updates its level. Owner/admin only.
func (s *Server) handleShareSandbox(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Identity string `json:"identity"`
		Level    string `json:"level"`
		Kind     string `json:"kind,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Identity == "" {
		respondError(w, http.StatusBadRequest, "identity is required")
		return
	}
	if req.Level != "viewer" && req.Level != "collaborator" {
		respondError(w, http.StatusBadRequest, "level must be \"viewer\" or \"collaborator\"")
		return
	}
	sandbox, err := s.getSandboxWithAuthCheck(r.Context(), mux.Vars(r)["id"], claims)
	if err != nil {
		respondError(w, http.StatusForbidden, err.Error())
		return
	}
	if sandbox.Spec.Sharing == nil {
		sandbox.Spec.Sharing = &agenttierv1alpha1.SharingSpec{}
	}
	perm := agenttierv1alpha1.SharePermission{Identity: req.Identity, Level: req.Level}
	if req.Kind == "group" {
		sandbox.Spec.Sharing.Groups = upsertSharePermission(sandbox.Spec.Sharing.Groups, perm)
	} else {
		sandbox.Spec.Sharing.Users = upsertSharePermission(sandbox.Spec.Sharing.Users, perm)
	}
	if err := s.k8sClient.Update(r.Context(), sandbox); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update sharing: "+err.Error())
		return
	}
	s.emitSandboxEvent(r.Context(), sandbox, corev1.EventTypeNormal, "ShareGranted", req.Identity)
	respondJSON(w, http.StatusOK, sharingToJSON(sandbox.Spec.Sharing))
}

// handleRevokeShare removes a user's (or group's) access. The {userId} path
// segment is the identity (URL-encoded email/group). Owner/admin only.
func (s *Server) handleRevokeShare(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	identity := mux.Vars(r)["userId"]
	sandbox, err := s.getSandboxWithAuthCheck(r.Context(), mux.Vars(r)["id"], claims)
	if err != nil {
		respondError(w, http.StatusForbidden, err.Error())
		return
	}
	if sandbox.Spec.Sharing != nil {
		sandbox.Spec.Sharing.Users = removeSharePermission(sandbox.Spec.Sharing.Users, identity)
		sandbox.Spec.Sharing.Groups = removeSharePermission(sandbox.Spec.Sharing.Groups, identity)
		if err := s.k8sClient.Update(r.Context(), sandbox); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to update sharing: "+err.Error())
			return
		}
		s.emitSandboxEvent(r.Context(), sandbox, corev1.EventTypeNormal, "ShareRevoked", identity)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "revoked", "identity": identity})
}

// handleCreateShareLink mints an expiring share link. The raw token is
// returned EXACTLY ONCE in this response; only its SHA-256 hash is persisted
// on the CR. Body: {"level": "viewer|collaborator", "expiresIn": "24h",
// "maxUses": 0}. Owner/admin only.
func (s *Server) handleCreateShareLink(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Level     string `json:"level"`
		ExpiresIn string `json:"expiresIn,omitempty"`
		MaxUses   int    `json:"maxUses,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
	}
	if req.Level == "" {
		req.Level = "viewer"
	}
	if req.Level != "viewer" && req.Level != "collaborator" {
		respondError(w, http.StatusBadRequest, "level must be \"viewer\" or \"collaborator\"")
		return
	}
	var expiresAt *metav1.Time
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil || d <= 0 {
			respondError(w, http.StatusBadRequest, "expiresIn must be a positive Go duration (e.g. \"24h\")")
			return
		}
		t := metav1.NewTime(time.Now().Add(d))
		expiresAt = &t
	}
	if req.MaxUses < 0 {
		respondError(w, http.StatusBadRequest, "maxUses must be >= 0 (0 = unlimited)")
		return
	}
	sandbox, err := s.getSandboxWithAuthCheck(r.Context(), mux.Vars(r)["id"], claims)
	if err != nil {
		respondError(w, http.StatusForbidden, err.Error())
		return
	}
	id, rawToken, hash, err := sharelinks.Generate()
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to generate share link")
		return
	}
	if sandbox.Spec.Sharing == nil {
		sandbox.Spec.Sharing = &agenttierv1alpha1.SharingSpec{}
	}
	sandbox.Spec.Sharing.ShareLinks = append(sandbox.Spec.Sharing.ShareLinks, agenttierv1alpha1.ShareLink{
		ID:        id,
		TokenHash: hash,
		Level:     req.Level,
		ExpiresAt: expiresAt,
		MaxUses:   req.MaxUses,
	})
	if err := s.k8sClient.Update(r.Context(), sandbox); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to persist share link: "+err.Error())
		return
	}
	// The raw token is returned once, here, and never again.
	respondJSON(w, http.StatusCreated, map[string]interface{}{
		"id":        id,
		"token":     rawToken,
		"level":     req.Level,
		"expiresAt": expiresAt,
		"maxUses":   req.MaxUses,
		"warning":   "Store this token now — it is shown only once.",
	})
}

// upsertSharePermission adds perm, or updates the level if the identity is
// already present.
func upsertSharePermission(perms []agenttierv1alpha1.SharePermission, perm agenttierv1alpha1.SharePermission) []agenttierv1alpha1.SharePermission {
	for i := range perms {
		if perms[i].Identity == perm.Identity {
			perms[i].Level = perm.Level
			return perms
		}
	}
	return append(perms, perm)
}

// removeSharePermission drops any permission matching identity.
func removeSharePermission(perms []agenttierv1alpha1.SharePermission, identity string) []agenttierv1alpha1.SharePermission {
	out := perms[:0]
	for _, p := range perms {
		if p.Identity != identity {
			out = append(out, p)
		}
	}
	return out
}

// sharingToJSON renders a SharingSpec for the API, omitting share-link token
// hashes (only non-secret link metadata is exposed).
func sharingToJSON(sharing *agenttierv1alpha1.SharingSpec) map[string]interface{} {
	users := []map[string]interface{}{}
	groups := []map[string]interface{}{}
	links := []map[string]interface{}{}
	if sharing != nil {
		for _, u := range sharing.Users {
			users = append(users, map[string]interface{}{"identity": u.Identity, "level": u.Level})
		}
		for _, g := range sharing.Groups {
			groups = append(groups, map[string]interface{}{"identity": g.Identity, "level": g.Level})
		}
		for _, l := range sharing.ShareLinks {
			links = append(links, map[string]interface{}{
				"id": l.ID, "level": l.Level, "expiresAt": l.ExpiresAt,
				"maxUses": l.MaxUses, "usedCount": l.UsedCount,
			})
		}
	}
	return map[string]interface{}{"users": users, "groups": groups, "shareLinks": links}
}
func (s *Server) handleListPolicies(w http.ResponseWriter, r *http.Request) {
	policies, err := s.governanceStore.ListPolicies(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list policies: "+err.Error())
		return
	}
	// Shape the response so the UI can find the cluster default easily.
	var cluster *governance.Policy
	namespaces := make([]map[string]interface{}, 0, len(policies))
	for _, sp := range policies {
		if sp.Scope == "" {
			p := sp.Policy
			cluster = &p
			continue
		}
		namespaces = append(namespaces, map[string]interface{}{
			"namespace": sp.Scope,
			"policy":    sp.Policy,
		})
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"cluster":    cluster,
		"namespaces": namespaces,
	})
}

func (s *Server) handleGetPolicy(w http.ResponseWriter, r *http.Request) {
	namespace := mux.Vars(r)["namespace"]
	policy, err := s.governanceStore.GetPolicy(r.Context(), namespace)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get policy: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"namespace": namespace,
		"policy":    policy,
	})
}

func (s *Server) handleUpsertClusterPolicy(w http.ResponseWriter, r *http.Request) {
	var policy governance.Policy
	if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := s.governanceStore.SetPolicy(r.Context(), "", policy); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save policy: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"scope":  "cluster",
		"policy": policy,
	})
}

func (s *Server) handleSetPolicy(w http.ResponseWriter, r *http.Request) {
	namespace := mux.Vars(r)["namespace"]
	if namespace == "" {
		respondError(w, http.StatusBadRequest, "namespace is required")
		return
	}
	var policy governance.Policy
	if err := json.NewDecoder(r.Body).Decode(&policy); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := s.governanceStore.SetPolicy(r.Context(), namespace, policy); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save policy: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"namespace": namespace,
		"policy":    policy,
	})
}

func (s *Server) handleDeletePolicy(w http.ResponseWriter, r *http.Request) {
	namespace := mux.Vars(r)["namespace"]
	if namespace == "" {
		respondError(w, http.StatusBadRequest, "namespace is required")
		return
	}
	if err := s.governanceStore.DeletePolicy(r.Context(), namespace); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to delete policy: "+err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetEffectivePolicy(w http.ResponseWriter, r *http.Request) {
	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = s.config.SandboxNamespace
	}
	if namespace == "" {
		namespace = "default"
	}
	policy, err := governance.Resolve(r.Context(), s.governanceStore, namespace)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to resolve policy: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"namespace": namespace,
		"policy":    policy,
	})
}
func (s *Server) handleListAuditEvents(w http.ResponseWriter, r *http.Request) {
	// Get Kubernetes events for sandboxes as activity log
	var events []map[string]interface{}
	eventList := &corev1.EventList{}
	// List events from all namespaces
	if err := s.k8sClient.List(r.Context(), eventList); err == nil {
		for _, event := range eventList.Items {
			if event.InvolvedObject.Kind != "Sandbox" {
				continue
			}
			events = append(events, map[string]interface{}{
				"timestamp":   event.LastTimestamp.Time,
				"eventType":   event.Reason,
				"sandboxId":   event.InvolvedObject.Name,
				"sandboxName": event.InvolvedObject.Name,
				"namespace":   event.InvolvedObject.Namespace,
				"userEmail":   "",
				"details":     map[string]string{"reason": event.Message},
			})
		}
	}

	// Sort by timestamp descending (most recent first)
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"events": events})
}

func (s *Server) handleGetUsageAnalytics(w http.ResponseWriter, r *http.Request) {
	sandboxList := &agenttierv1alpha1.SandboxList{}
	if err := s.k8sClient.List(r.Context(), sandboxList); err != nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{})
		return
	}

	statusBreakdown := map[string]int{}
	templateBreakdown := map[string]int{}
	var totalStartupMs int64
	startupCount := 0

	for _, sb := range sandboxList.Items {
		phase := string(sb.Status.Phase)
		if phase == "" {
			phase = "creating"
		}
		statusBreakdown[phase]++

		tmpl := sb.Status.ResolvedTemplate
		if tmpl == "" {
			tmpl = "unknown"
		}
		templateBreakdown[tmpl]++

		// Calculate startup time if available
		if sb.Status.StartedAt != nil {
			dur := sb.Status.StartedAt.Time.Sub(sb.CreationTimestamp.Time)
			totalStartupMs += dur.Milliseconds()
			startupCount++
		}
	}

	avgStartupMs := int64(0)
	if startupCount > 0 {
		avgStartupMs = totalStartupMs / int64(startupCount)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"total_sandboxes":      len(sandboxList.Items),
		"status_breakdown":     statusBreakdown,
		"template_breakdown":   templateBreakdown,
		"avg_startup_ms":       avgStartupMs,
		"startup_sample_count": startupCount,
	})
}

func (s *Server) handleGetCostEstimates(w http.ResponseWriter, r *http.Request) {
	sandboxList := &agenttierv1alpha1.SandboxList{}
	if err := s.k8sClient.List(r.Context(), sandboxList); err != nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{})
		return
	}

	// Cost rates (USD per hour) — configurable via Helm values in production
	cpuPerHour := 0.04
	memPerGBHour := 0.005
	storagePerGBMonth := 0.10

	running := 0
	stopped := 0
	totalHourlyCost := 0.0
	var perTemplate []map[string]interface{}
	templateCosts := map[string]float64{}
	templateCounts := map[string]int{}

	for _, sb := range sandboxList.Items {
		tmpl := sb.Status.ResolvedTemplate
		if tmpl == "" {
			tmpl = "unknown"
		}

		if sb.Status.Phase == agenttierv1alpha1.SandboxPhaseRunning {
			running++
			// Estimate: 2 CPU, 4GB RAM per running sandbox (from template defaults)
			hourlyCost := (2 * cpuPerHour) + (4 * memPerGBHour)
			totalHourlyCost += hourlyCost
			templateCosts[tmpl] += hourlyCost
		} else if sb.Status.Phase == agenttierv1alpha1.SandboxPhaseStopped {
			stopped++
		}
		templateCounts[tmpl]++
	}

	// Storage cost (all sandboxes, running or stopped, have PVCs)
	storageCostMonthly := float64(len(sandboxList.Items)) * 10.0 * storagePerGBMonth / 30.0 * 24.0 // per hour

	for tmpl, cost := range templateCosts {
		perTemplate = append(perTemplate, map[string]interface{}{
			"template":    tmpl,
			"hourly_cost": cost,
			"count":       templateCounts[tmpl],
		})
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"running_sandboxes":       running,
		"stopped_sandboxes":       stopped,
		"total_hourly_compute":    totalHourlyCost,
		"total_hourly_storage":    storageCostMonthly,
		"total_estimated_monthly": (totalHourlyCost + storageCostMonthly) * 24 * 30,
		"per_template":            perTemplate,
	})
}

// handleAdminListSandboxes lists every sandbox cluster-wide (no ownership
// filter — this route is already requireAdmin-gated) and joins in each
// sandbox's scoped API keys (FR6.6): metadata only (id/actionGroups/
// createdAt), never the plaintext key or its hash, matching the existing
// GET /user/api-keys list-response shape. A sandbox with no scoped key
// gets an empty/absent scopedApiKeys field, not an error — most sandboxes
// (HTTP-exec/agent-mode opt-out) never mint one.
func (s *Server) handleAdminListSandboxes(w http.ResponseWriter, r *http.Request) {
	sandboxList := &agenttierv1alpha1.SandboxList{}
	listOpts := []client.ListOption{}
	if ns := r.URL.Query().Get("namespace"); ns != "" {
		listOpts = append(listOpts, client.InNamespace(ns))
	}
	if err := s.k8sClient.List(r.Context(), sandboxList, listOpts...); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list sandboxes: "+err.Error())
		return
	}

	// One list-all-then-group pass rather than a per-sandbox lookup — see
	// apikeystore.Store.ListAll's doc comment for why a full list is the
	// simplest correct approach at this scale.
	keys, err := s.secretStore().listAllAPIKeys(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list scoped api keys: "+err.Error())
		return
	}
	keysBySandbox := make(map[string][]apiKeyMetadata)
	for _, k := range keys {
		if k.SandboxID == "" {
			continue // user-level key, not sandbox-scoped
		}
		keysBySandbox[k.SandboxID] = append(keysBySandbox[k.SandboxID], k)
	}

	results := make([]map[string]interface{}, 0, len(sandboxList.Items))
	for i := range sandboxList.Items {
		sb := &sandboxList.Items[i]
		entry := sandboxToJSON(sb)
		if scoped := keysBySandbox[sb.Name]; len(scoped) > 0 {
			entry["scopedApiKeys"] = scoped
		}
		results = append(results, entry)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"sandboxes": results})
}
func (s *Server) handleAdminListSharing(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, map[string]interface{}{})
}

// userPrefsConfigMapName derives a stable ConfigMap name from the user's OIDC
// subject. The subject is hashed (not stored in plaintext) so the resource
// name is a valid RFC 1123 label and carries no PII.
func userPrefsConfigMapName(sub string) string {
	sum := sha256.Sum256([]byte(sub))
	return "agenttier-prefs-" + hex.EncodeToString(sum[:])[:16]
}

func (s *Server) prefsNamespace() string {
	if s.config.InstallNamespace != "" {
		return s.config.InstallNamespace
	}
	return "default"
}

func (s *Server) handleGetPreferences(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	cm := &corev1.ConfigMap{}
	err := s.k8sClient.Get(r.Context(),
		types.NamespacedName{Name: userPrefsConfigMapName(claims.Sub), Namespace: s.prefsNamespace()}, cm)
	if err != nil {
		// No preferences saved yet — return an empty object, not an error.
		respondJSON(w, http.StatusOK, map[string]interface{}{})
		return
	}
	prefs := map[string]interface{}{}
	if raw := cm.Data["preferences"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &prefs)
	}
	respondJSON(w, http.StatusOK, prefs)
}

func (s *Server) handleUpdatePreferences(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var prefs map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&prefs); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	data, err := json.Marshal(prefs)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid preferences: "+err.Error())
		return
	}

	name := userPrefsConfigMapName(claims.Sub)
	ns := s.prefsNamespace()
	cm := &corev1.ConfigMap{}
	if getErr := s.k8sClient.Get(r.Context(), types.NamespacedName{Name: name, Namespace: ns}, cm); getErr != nil {
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
				Labels:    map[string]string{"agenttier.io/component": "user-preferences"},
			},
			Data: map[string]string{"preferences": string(data)},
		}
		if err := s.k8sClient.Create(r.Context(), cm); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to save preferences: "+err.Error())
			return
		}
	} else {
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data["preferences"] = string(data)
		if err := s.k8sClient.Update(r.Context(), cm); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to save preferences: "+err.Error())
			return
		}
	}
	respondJSON(w, http.StatusOK, prefs)
}
func (s *Server) handleGetMe(w http.ResponseWriter, r *http.Request) {
	claims := GetClaims(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"sub":     claims.Sub,
		"email":   claims.Email,
		"name":    claims.Name,
		"groups":  claims.Groups,
		"isAdmin": claims.IsAdmin,
	})
}

// --- Warm Pool Handlers ---

func (s *Server) handleGetWarmPoolStatus(w http.ResponseWriter, r *http.Request) {
	status, err := warmpool.GetStatus(r.Context(), s.k8sClient, s.config.InstallNamespace, s.config.SandboxNamespace)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get pool status: "+err.Error())
		return
	}
	respondJSON(w, http.StatusOK, status)
}

func (s *Server) handleSetWarmPoolConfig(w http.ResponseWriter, r *http.Request) {
	// Accept either the new per-template shape (pools array) or the
	// legacy single-template shape (top-level desiredCount + template).
	// Both produce the same canonical Pools slice on the way out — see
	// warmpool.Config.normalize().
	var req struct {
		Pools        []warmpool.PoolConfig `json:"pools,omitempty"`
		DesiredCount int                   `json:"desiredCount,omitempty"`
		Template     string                `json:"template,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	cfg := warmpool.Config{Pools: req.Pools, DesiredCount: req.DesiredCount, Template: req.Template}
	// normalize() promotes legacy fields into Pools[0] and zeros the
	// legacy fields, so validation below works on the canonical shape
	// regardless of which version of the request body the caller sent.
	cfg.Normalize()

	for i, p := range cfg.Pools {
		if p.Template == "" {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("pools[%d].template is required", i))
			return
		}
		if p.DesiredCount < 0 || p.DesiredCount > 10 {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("pools[%d].desiredCount must be 0-10", i))
			return
		}
	}

	if err := warmpool.SetConfig(r.Context(), s.k8sClient, s.config.InstallNamespace, cfg); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save config: "+err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "updated", "pools": cfg.Pools})
}

// --- Helpers ---

func (s *Server) getSandboxWithAuthCheck(ctx context.Context, sandboxID string, claims *Claims) (*agenttierv1alpha1.Sandbox, error) {
	sandbox, err := s.resolveSandbox(ctx, sandboxID, "")
	if err != nil {
		return nil, err
	}

	// Check ownership (non-admin must own the sandbox)
	if !claims.IsAdmin {
		if sandbox.Spec.CreatedBy == nil || sandbox.Spec.CreatedBy.Sub != claims.Sub {
			return nil, fmt.Errorf("access denied to sandbox %s", sandboxID)
		}
	}

	return sandbox, nil
}

// userCanAccessSandbox reports whether claims may READ/use the sandbox:
// admins and the owner always can, plus any user or group it's been shared
// with (regardless of level — viewer and collaborator both get access). It is
// the read-path counterpart to getSandboxWithAuthCheck, which stays
// owner/admin-only for lifecycle and sharing-management mutations.
func userCanAccessSandbox(sandbox *agenttierv1alpha1.Sandbox, claims *Claims) bool {
	if claims == nil {
		return false
	}
	if claims.IsAdmin {
		return true
	}
	if sandbox.Spec.CreatedBy != nil && sandbox.Spec.CreatedBy.Sub == claims.Sub {
		return true
	}
	if sandbox.Spec.Sharing == nil {
		return false
	}
	for _, u := range sandbox.Spec.Sharing.Users {
		if u.Identity != "" && (u.Identity == claims.Sub || u.Identity == claims.Email) {
			return true
		}
	}
	for _, g := range sandbox.Spec.Sharing.Groups {
		for _, cg := range claims.Groups {
			if g.Identity == cg {
				return true
			}
		}
	}
	return false
}

// getSandboxForRead resolves a sandbox and authorizes read/use access for
// the owner, admins, OR users/groups it's shared with.
func (s *Server) getSandboxForRead(ctx context.Context, sandboxID string, claims *Claims) (*agenttierv1alpha1.Sandbox, error) {
	sandbox, err := s.resolveSandbox(ctx, sandboxID, "")
	if err != nil {
		return nil, err
	}
	if !userCanAccessSandbox(sandbox, claims) {
		return nil, fmt.Errorf("access denied to sandbox %s", sandboxID)
	}
	return sandbox, nil
}

// recordAudit writes an audit event to the optional historical-records
// backend. Best-effort: a backend error is logged and never affects the
// request. A no-op when no SQL backend is configured (the default).
func (s *Server) recordAudit(ctx context.Context, claims *Claims, action, resource, sandboxID string) {
	if s.store == nil {
		return
	}
	actor := ""
	if claims != nil {
		actor = claims.Sub
	}
	if err := s.store.RecordAuditEvent(ctx, storage.AuditEvent{
		Actor:     actor,
		Action:    action,
		Resource:  resource,
		SandboxID: sandboxID,
	}); err != nil {
		s.logger.Warn("failed to record audit event", "action", action, "resource", resource, "error", err)
	}
}

// resolveSandbox finds a Sandbox by ID (name) without assuming it lives in
// "default". It tries the configured sandbox namespace first (a cheap Get —
// the common single-namespace case), then falls back to a cluster-wide
// list-by-name so sandboxes created in other namespaces are reachable (the
// multi-namespace / per-namespace-governance deployment the product targets).
// Previously every sandbox-scoped lookup hardcoded Namespace:"default",
// silently 404ing anything created elsewhere.
//
// nsHint, when non-empty (e.g. from a ?namespace= query param), pins the
// lookup to that namespace and skips the cluster-wide search — this also
// disambiguates the rare case of the same name existing in two namespaces.
func (s *Server) resolveSandbox(ctx context.Context, sandboxID, nsHint string) (*agenttierv1alpha1.Sandbox, error) {
	if nsHint != "" {
		sb := &agenttierv1alpha1.Sandbox{}
		if err := s.k8sClient.Get(ctx, types.NamespacedName{Name: sandboxID, Namespace: nsHint}, sb); err != nil {
			return nil, fmt.Errorf("sandbox %s not found in namespace %s", sandboxID, nsHint)
		}
		return sb, nil
	}

	// Fast path: the configured sandbox namespace.
	primaryNS := s.config.SandboxNamespace
	if primaryNS == "" {
		primaryNS = "default"
	}
	sb := &agenttierv1alpha1.Sandbox{}
	if err := s.k8sClient.Get(ctx, types.NamespacedName{Name: sandboxID, Namespace: primaryNS}, sb); err == nil {
		return sb, nil
	}

	// Fall back to a cluster-wide search by name.
	list := &agenttierv1alpha1.SandboxList{}
	if err := s.k8sClient.List(ctx, list); err != nil {
		return nil, fmt.Errorf("sandbox %s not found", sandboxID)
	}
	var match *agenttierv1alpha1.Sandbox
	count := 0
	for i := range list.Items {
		if list.Items[i].Name == sandboxID {
			match = &list.Items[i]
			count++
		}
	}
	switch count {
	case 0:
		return nil, fmt.Errorf("sandbox %s not found", sandboxID)
	case 1:
		return match, nil
	default:
		return nil, fmt.Errorf("sandbox %s exists in multiple namespaces; specify ?namespace=<ns>", sandboxID)
	}
}

func sandboxToJSON(sb *agenttierv1alpha1.Sandbox) map[string]interface{} {
	// Default mode to "code" when the field is empty so older CRs that
	// predate the agent-mode field still report a sensible value to the
	// Web UI. The kubebuilder default in the CRD covers new sandboxes;
	// this is the read-side defensive fallback for existing rows.
	mode := string(sb.Spec.Mode)
	if mode == "" {
		mode = string(agenttierv1alpha1.SandboxModeCode)
	}
	result := map[string]interface{}{
		"sandboxId":   sb.Name,
		"name":        sb.Name,
		"namespace":   sb.Namespace,
		"status":      string(sb.Status.Phase),
		"podName":     sb.Status.PodName,
		"pvcName":     sb.Status.PVCName,
		"templateRef": sb.Status.ResolvedTemplate,
		"mode":        mode,
		"createdAt":   sb.CreationTimestamp.Time.String(),
		"message":     sb.Status.Message,
	}
	if sb.Spec.CreatedBy != nil {
		result["createdBy"] = map[string]string{
			"email":       sb.Spec.CreatedBy.Email,
			"displayName": sb.Spec.CreatedBy.DisplayName,
		}
	}
	if sb.Status.LastActivityTimestamp != nil {
		result["lastActivityAt"] = sb.Status.LastActivityTimestamp.Time.String()
	}
	return result
}

func imageFromSpec(img *agenttierv1alpha1.ImageSpec) string {
	if img == nil {
		return ""
	}
	return img.Repository
}

// parseDuration parses a Go-style duration string ("30m", "8h", "24h") into a
// *metav1.Duration for the Sandbox spec. It rejects unparseable and
// non-positive values so a bad timeout surfaces as a 400 at the create handler
// instead of being silently dropped.
func parseDuration(s string) (*metav1.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return nil, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if d <= 0 {
		return nil, fmt.Errorf("duration %q must be positive", s)
	}
	return &metav1.Duration{Duration: d}, nil
}

func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func respondError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// Ensure terminal package is referenced
var _ = terminal.MessageTypeInput

// resolveTemplateMode looks up the directly referenced template (cluster-
// scoped or namespaced) and returns its mode field. Best-effort: returns
// empty string on any error so the caller can fall back to the CRD default.
// Inheritance chains are not walked here — child templates that override
// mode will surface correctly because their direct mode wins; templates
// that inherit mode from a parent get the parent's value at the next
// reconcile cycle, which is acceptable since the controller already runs
// full template resolution.
func (s *Server) resolveTemplateMode(ctx context.Context, ref *agenttierv1alpha1.TemplateReference, sandboxNamespace string) agenttierv1alpha1.SandboxMode {
	if ref == nil || ref.Name == "" {
		return ""
	}
	kind := ref.Kind
	if kind == "" {
		kind = "ClusterSandboxTemplate"
	}
	switch kind {
	case "ClusterSandboxTemplate":
		t := &agenttierv1alpha1.ClusterSandboxTemplate{}
		if err := s.k8sClient.Get(ctx, client.ObjectKey{Name: ref.Name}, t); err != nil {
			return ""
		}
		return t.Spec.Mode
	case "SandboxTemplate":
		ns := ref.Namespace
		if ns == "" {
			ns = sandboxNamespace
		}
		t := &agenttierv1alpha1.SandboxTemplate{}
		if err := s.k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: ref.Name}, t); err != nil {
			return ""
		}
		return t.Spec.Mode
	}
	return ""
}
