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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
	"github.com/agenttier/agenttier/pkg/governance"
)

// patchFixture builds a Server plus a standalone mux wired to
// handlePatchSandbox. Route wiring into the real s.router happens in the
// Group 2b serialization task (server.go); tests here register their own
// minimal router the same way backup_handlers_test.go does.
func patchFixture(t *testing.T, objs ...client.Object) (*Server, client.Client, http.Handler) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, agenttierv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&agenttierv1alpha1.Sandbox{}).
		Build()
	s := NewServer(&Config{ListenAddr: ":0", DevAuth: true}, c, nil)

	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()
	api.HandleFunc("/sandboxes/{id}", s.handlePatchSandbox).Methods("PATCH")

	return s, c, r
}

func patchRequest(sandboxID, body string, admin bool) *http.Request {
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/sandboxes/"+sandboxID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	claims := &Claims{
		Sub:     "u-test",
		Email:   "test@agenttier.local",
		Name:    "Test User",
		IsAdmin: admin,
	}
	return req.WithContext(context.WithValue(req.Context(), ClaimsContextKey, claims))
}

func runningSandbox(name, namespace string) *agenttierv1alpha1.Sandbox {
	return &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: agenttierv1alpha1.SandboxSpec{
			Mode:        agenttierv1alpha1.SandboxModeCode,
			TemplateRef: &agenttierv1alpha1.TemplateReference{Name: "general-coding", Kind: "ClusterSandboxTemplate"},
		},
		Status: agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
}

func TestPatchSandbox_IdleTimeoutAppliedImmediately(t *testing.T) {
	sandbox := runningSandbox("sbx-1", "default")
	_, c, r := patchFixture(t, sandbox)

	req := patchRequest("sbx-1", `{"idleTimeout":"30m"}`, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		SandboxID       string            `json:"sandboxId"`
		Applied         map[string]string `json:"applied"`
		RestartRequired bool              `json:"restartRequired"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v body=%s", err, rec.Body.String())
	}
	if resp.Applied["idleTimeout"] != "immediately" {
		t.Errorf("applied.idleTimeout = %q, want immediately", resp.Applied["idleTimeout"])
	}
	if resp.RestartRequired {
		t.Error("idleTimeout-only PATCH must not set restartRequired")
	}

	updated := &agenttierv1alpha1.Sandbox{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "sbx-1", Namespace: "default"}, updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Spec.IdleTimeout == nil || updated.Spec.IdleTimeout.Duration.String() != "30m0s" {
		t.Errorf("Spec.IdleTimeout = %v, want 30m0s", updated.Spec.IdleTimeout)
	}
}

func TestPatchSandbox_ResourcesRequireRestart(t *testing.T) {
	sandbox := runningSandbox("sbx-1", "default")
	_, c, r := patchFixture(t, sandbox)

	body := `{"resources":{"requests":{"cpu":"1","memory":"2Gi"},"limits":{"cpu":"2","memory":"4Gi"}}}`
	req := patchRequest("sbx-1", body, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Applied         map[string]string `json:"applied"`
		RestartRequired bool              `json:"restartRequired"`
		Message         string            `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if resp.Applied["resources"] != "on-restart" {
		t.Errorf("applied.resources = %q, want on-restart", resp.Applied["resources"])
	}
	if !resp.RestartRequired {
		t.Error("resources PATCH must set restartRequired:true")
	}
	if resp.Message == "" {
		t.Error("expected a human message explaining restart is required")
	}

	updated := &agenttierv1alpha1.Sandbox{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "sbx-1", Namespace: "default"}, updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Spec.Resources == nil {
		t.Fatal("Spec.Resources was not persisted")
	}
	if cpu := updated.Spec.Resources.Limits.Cpu(); cpu.String() != "2" {
		t.Errorf("Spec.Resources.Limits.cpu = %v, want 2", cpu)
	}
}

func TestPatchSandbox_LabelsAndAnnotationsMerge(t *testing.T) {
	sandbox := runningSandbox("sbx-1", "default")
	sandbox.Labels = map[string]string{"existing": "keep-me"}
	_, c, r := patchFixture(t, sandbox)

	req := patchRequest("sbx-1", `{"labels":{"team":"x"},"annotations":{"note":"y"}}`, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	updated := &agenttierv1alpha1.Sandbox{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "sbx-1", Namespace: "default"}, updated); err != nil {
		t.Fatalf("get: %v", err)
	}
	if updated.Labels["team"] != "x" || updated.Labels["existing"] != "keep-me" {
		t.Errorf("labels = %+v, want team=x AND existing=keep-me (merge, not replace)", updated.Labels)
	}
	if updated.Annotations["note"] != "y" {
		t.Errorf("annotations = %+v, want note=y", updated.Annotations)
	}
}

func TestPatchSandbox_RejectsEmptyBody(t *testing.T) {
	sandbox := runningSandbox("sbx-1", "default")
	_, _, r := patchFixture(t, sandbox)

	req := patchRequest("sbx-1", `{}`, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (empty patch), got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPatchSandbox_RejectsInvalidIdleTimeout(t *testing.T) {
	sandbox := runningSandbox("sbx-1", "default")
	_, _, r := patchFixture(t, sandbox)

	req := patchRequest("sbx-1", `{"idleTimeout":"not-a-duration"}`, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (invalid idleTimeout), got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestPatchSandbox_UnauthorizedOtherUser(t *testing.T) {
	sandbox := runningSandbox("sbx-1", "default")
	sandbox.Spec.CreatedBy = &agenttierv1alpha1.UserIdentity{Sub: "owner"}
	_, _, r := patchFixture(t, sandbox)

	req := patchRequest("sbx-1", `{"idleTimeout":"30m"}`, false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (ownership-masked), got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPatchSandbox_GovernanceRejectsOverIdleTimeoutCap verifies NFR3:
// governance is re-checked at PATCH time, not only at original create time.
func TestPatchSandbox_GovernanceRejectsOverIdleTimeoutCap(t *testing.T) {
	sandbox := runningSandbox("sbx-1", "default")
	s, c, r := patchFixture(t, sandbox)

	policies := map[string]governance.Policy{"default": {MaxIdleTimeout: "10m"}}
	policyCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: governance.ConfigMapName, Namespace: governance.ConfigMapNamespace},
		Data:       map[string]string{"policies": mustMarshalPolicies(t, policies)},
	}
	if err := c.Create(context.Background(), policyCM); err != nil {
		t.Fatalf("seed governance policy: %v", err)
	}
	_ = s

	req := patchRequest("sbx-1", `{"idleTimeout":"30m"}`, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (governance cap), got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "policy_violation") {
		t.Errorf("expected policy_violation error shape, got %s", rec.Body.String())
	}
}

// TestPatchSandbox_GovernanceRejectsOverCPUCap covers the resources axis of
// NFR3 governance re-check (maxCpu), separate from the idleTimeout case above.
func TestPatchSandbox_GovernanceRejectsOverCPUCap(t *testing.T) {
	sandbox := runningSandbox("sbx-1", "default")
	s, c, r := patchFixture(t, sandbox)

	policies := map[string]governance.Policy{"default": {MaxCPU: "1"}}
	policyCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: governance.ConfigMapName, Namespace: governance.ConfigMapNamespace},
		Data:       map[string]string{"policies": mustMarshalPolicies(t, policies)},
	}
	if err := c.Create(context.Background(), policyCM); err != nil {
		t.Fatalf("seed governance policy: %v", err)
	}
	_ = s

	req := patchRequest("sbx-1", `{"resources":{"limits":{"cpu":"4"}}}`, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (governance CPU cap), got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestPatchSandbox_SucceedsAtSandboxCountCap is the review-2 regression test:
// PATCH must never evaluate the sandbox-COUNT quotas (MaxSandboxesTotal/
// MaxSandboxesPerUser) since it creates no new sandbox. A namespace already
// AT its count cap must still accept a value-only PATCH (e.g. idleTimeout
// within the idle-timeout cap) — verifies handlePatchSandbox calls
// governance.CheckResourceLimits, not governance.Check, for its re-check.
func TestPatchSandbox_SucceedsAtSandboxCountCap(t *testing.T) {
	sandbox := runningSandbox("sbx-1", "default")
	// A second sandbox in the same namespace so the fake client's List sees
	// TotalSandboxes==1 already, matching MaxSandboxesTotal below (AT the cap
	// via Check's `>=` comparison).
	other := runningSandbox("sbx-2", "default")
	_, c, r := patchFixture(t, sandbox, other)

	policies := map[string]governance.Policy{
		"default": {MaxSandboxesTotal: 2, MaxSandboxesPerUser: 2, MaxIdleTimeout: "1h"},
	}
	policyCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: governance.ConfigMapName, Namespace: governance.ConfigMapNamespace},
		Data:       map[string]string{"policies": mustMarshalPolicies(t, policies)},
	}
	if err := c.Create(context.Background(), policyCM); err != nil {
		t.Fatalf("seed governance policy: %v", err)
	}

	req := patchRequest("sbx-1", `{"idleTimeout":"30m"}`, true)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (count quota must not gate PATCH), got %d body=%s", rec.Code, rec.Body.String())
	}
}
