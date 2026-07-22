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

// bulkFixture builds a Server plus a standalone mux wired to the FR4 bulk
// handlers. Route wiring on the real s.router happens in the Group 2b
// serialization task (server.go); tests here register their own minimal
// router, matching backup_handlers_test.go's approach for the pre-wiring
// era of that handler.
func bulkFixture(t *testing.T, objs ...client.Object) (*Server, client.Client, http.Handler) {
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
	s := NewServer(&Config{ListenAddr: ":0", InstallNamespace: "agenttier", DevAuth: true}, c, nil)

	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()
	api.HandleFunc("/sandboxes/bulk", s.handleBulkCreate).Methods("POST")
	api.HandleFunc("/sandboxes/bulk-action", s.handleBulkAction).Methods("POST")

	return s, c, r
}

func bulkRequest(method, path, body string, claims *Claims) *http.Request {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	return req.WithContext(context.WithValue(req.Context(), ClaimsContextKey, claims))
}

func adminClaims() *Claims {
	return &Claims{Sub: "u-test", Email: "test@agenttier.local", Name: "Test User", IsAdmin: true}
}

// --- handleBulkCreate ---

func TestBulkCreate_PerItemPartialSuccess(t *testing.T) {
	_, c, r := bulkFixture(t)

	body := `{"items":[
		{"name":"sbx-ok","templateRef":{"name":"general-coding"}},
		{"name":"","templateRef":{"name":"general-coding"}},
		{"name":"sbx-bad-timeout","templateRef":{"name":"general-coding"},"timeout":"not-a-duration"}
	]}`
	req := bulkRequest(http.MethodPost, "/api/v1/sandboxes/bulk", body, adminClaims())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (per-item results, not a batch error), got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Results []bulkCreateItemResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Results) != 3 {
		t.Fatalf("expected 3 results, got %d: %+v", len(resp.Results), resp.Results)
	}
	if resp.Results[0].Status != "created" || resp.Results[0].SandboxID != "sbx-ok" {
		t.Errorf("item 0 (valid): expected created/sbx-ok, got %+v", resp.Results[0])
	}
	if resp.Results[1].Status != "error" {
		t.Errorf("item 1 (empty name): expected error, got %+v", resp.Results[1])
	}
	if resp.Results[2].Status != "error" {
		t.Errorf("item 2 (bad timeout): expected error, got %+v", resp.Results[2])
	}

	// The valid item's sandbox must actually exist despite siblings failing.
	got := &agenttierv1alpha1.Sandbox{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "sbx-ok", Namespace: "default"}, got); err != nil {
		t.Errorf("expected sbx-ok to be created despite sibling failures: %v", err)
	}
	// The failed items must NOT have left partial sandboxes behind.
	if err := c.Get(context.Background(), client.ObjectKey{Name: "sbx-bad-timeout", Namespace: "default"}, &agenttierv1alpha1.Sandbox{}); err == nil {
		t.Error("sbx-bad-timeout should not have been created (invalid timeout)")
	}
}

func TestBulkCreate_RejectsEmptyItems(t *testing.T) {
	_, _, r := bulkFixture(t)
	req := bulkRequest(http.MethodPost, "/api/v1/sandboxes/bulk", `{"items":[]}`, adminClaims())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty items, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBulkCreate_GovernanceCapFailsWholeBatch(t *testing.T) {
	s, c, r := bulkFixture(t)
	ctx := context.Background()

	// One existing sandbox in the "default" namespace; cap of 2 means
	// creating 2 more items below would land total usage at 3 — over the
	// cap of 2, so the whole batch must be rejected fail-fast (DD4).
	existing := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-existing", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "u-test"}},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
	if err := c.Create(ctx, existing); err != nil {
		t.Fatalf("create existing: %v", err)
	}

	policies := map[string]governance.Policy{"default": {MaxSandboxesTotal: 2}}
	policyCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: governance.ConfigMapName, Namespace: governance.ConfigMapNamespace},
		Data:       map[string]string{"policies": mustMarshalPolicies(t, policies)},
	}
	if err := c.Create(ctx, policyCM); err != nil {
		t.Fatalf("seed governance policy: %v", err)
	}
	_ = s

	body := `{"items":[
		{"name":"sbx-new-1","templateRef":{"name":"general-coding"}},
		{"name":"sbx-new-2","templateRef":{"name":"general-coding"}}
	]}`
	req := bulkRequest(http.MethodPost, "/api/v1/sandboxes/bulk", body, adminClaims())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 (whole-batch cap rejection), got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "quota_would_exceed") {
		t.Errorf("expected quota_would_exceed error shape, got %s", rec.Body.String())
	}

	// Fail-fast: NEITHER item should have been created.
	if err := c.Get(ctx, client.ObjectKey{Name: "sbx-new-1", Namespace: "default"}, &agenttierv1alpha1.Sandbox{}); err == nil {
		t.Error("sbx-new-1 should not exist — batch was rejected fail-fast")
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "sbx-new-2", Namespace: "default"}, &agenttierv1alpha1.Sandbox{}); err == nil {
		t.Error("sbx-new-2 should not exist — batch was rejected fail-fast")
	}
}

// TestBulkCreate_PerItemGovernanceViolation_TemplateNotAllowed regresses the
// gap found in a post-hoc review of this handler: createOneBulkSandbox used
// to only rely on the aggregate CheckBulk (sandbox-COUNT quotas), never the
// full per-item governance.Check that handleCreateSandbox runs for a single
// create. A disallowed template must fail THAT item — and only that item —
// not sail through uncontested (per requirements.md's own edge case).
func TestBulkCreate_PerItemGovernanceViolation_TemplateNotAllowed(t *testing.T) {
	_, c, r := bulkFixture(t)
	ctx := context.Background()

	policies := map[string]governance.Policy{"default": {AllowedTemplates: []string{"general-coding"}}}
	policyCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: governance.ConfigMapName, Namespace: governance.ConfigMapNamespace},
		Data:       map[string]string{"policies": mustMarshalPolicies(t, policies)},
	}
	if err := c.Create(ctx, policyCM); err != nil {
		t.Fatalf("seed governance policy: %v", err)
	}

	body := `{"items":[
		{"name":"sbx-allowed","templateRef":{"name":"general-coding"}},
		{"name":"sbx-disallowed","templateRef":{"name":"not-on-the-allowlist"}}
	]}`
	req := bulkRequest(http.MethodPost, "/api/v1/sandboxes/bulk", body, adminClaims())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (per-item results, not a batch abort), got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Results []bulkCreateItemResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d: %+v", len(resp.Results), resp.Results)
	}
	if resp.Results[0].Status != "created" {
		t.Errorf("item 0 (allowed template): expected created, got %+v", resp.Results[0])
	}
	if resp.Results[1].Status != "error" || !strings.Contains(resp.Results[1].Error, "policy_violation") {
		t.Errorf("item 1 (disallowed template): expected a policy_violation error, got %+v", resp.Results[1])
	}

	// The allowed item must actually exist; the disallowed one must not.
	if err := c.Get(ctx, client.ObjectKey{Name: "sbx-allowed", Namespace: "default"}, &agenttierv1alpha1.Sandbox{}); err != nil {
		t.Errorf("expected sbx-allowed to be created: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "sbx-disallowed", Namespace: "default"}, &agenttierv1alpha1.Sandbox{}); err == nil {
		t.Error("sbx-disallowed should not have been created — template is not on the allowlist")
	}
}

// TestBulkCreate_PerItemGovernanceViolation_TimeoutExceedsCap covers the
// resource/timeout VALUE-cap side of the same gap (as opposed to the
// allowlist side above) — maxTimeout is enforced per-item via
// governance.Check's CheckResourceLimits subset, same as a single create.
func TestBulkCreate_PerItemGovernanceViolation_TimeoutExceedsCap(t *testing.T) {
	_, c, r := bulkFixture(t)
	ctx := context.Background()

	policies := map[string]governance.Policy{"default": {MaxTimeout: "1h"}}
	policyCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: governance.ConfigMapName, Namespace: governance.ConfigMapNamespace},
		Data:       map[string]string{"policies": mustMarshalPolicies(t, policies)},
	}
	if err := c.Create(ctx, policyCM); err != nil {
		t.Fatalf("seed governance policy: %v", err)
	}

	body := `{"items":[
		{"name":"sbx-within-cap","templateRef":{"name":"general-coding"},"timeout":"30m"},
		{"name":"sbx-over-cap","templateRef":{"name":"general-coding"},"timeout":"5h"}
	]}`
	req := bulkRequest(http.MethodPost, "/api/v1/sandboxes/bulk", body, adminClaims())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (per-item results, not a batch abort), got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Results []bulkCreateItemResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Results) != 2 {
		t.Fatalf("expected 2 results, got %d: %+v", len(resp.Results), resp.Results)
	}
	if resp.Results[0].Status != "created" {
		t.Errorf("item 0 (within cap): expected created, got %+v", resp.Results[0])
	}
	if resp.Results[1].Status != "error" || !strings.Contains(resp.Results[1].Error, "policy_violation") {
		t.Errorf("item 1 (over cap): expected a policy_violation error, got %+v", resp.Results[1])
	}

	if err := c.Get(ctx, client.ObjectKey{Name: "sbx-over-cap", Namespace: "default"}, &agenttierv1alpha1.Sandbox{}); err == nil {
		t.Error("sbx-over-cap should not have been created — timeout exceeds governance cap")
	}
}

// TestBulkCreate_GovernanceCapFailsWholeBatch_StillWorksAlongsidePerItemCheck
// is a non-regression guard: adding the per-item governance.Check must not
// break the pre-existing whole-batch fail-fast behavior for the aggregate
// count cap (DD4) — that check still runs BEFORE any item-level check and
// still rejects everything, not just individual items.
func TestBulkCreate_GovernanceCapFailsWholeBatch_StillWorksAlongsidePerItemCheck(t *testing.T) {
	_, c, r := bulkFixture(t)
	ctx := context.Background()

	existing := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-existing-2", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "u-test"}},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
	if err := c.Create(ctx, existing); err != nil {
		t.Fatalf("create existing: %v", err)
	}

	// Cap of 1 total, 1 already exists, batch of 1 more item would put
	// usage at 2 — over cap, so CheckBulk must reject before per-item Check
	// ever runs (the item itself has an allowed template, so if per-item
	// Check ran and count-quota fired there too it would still be a 200
	// with a per-item error, not the whole-batch 409 this test expects).
	policies := map[string]governance.Policy{"default": {MaxSandboxesTotal: 1, AllowedTemplates: []string{"general-coding"}}}
	policyCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: governance.ConfigMapName, Namespace: governance.ConfigMapNamespace},
		Data:       map[string]string{"policies": mustMarshalPolicies(t, policies)},
	}
	if err := c.Create(ctx, policyCM); err != nil {
		t.Fatalf("seed governance policy: %v", err)
	}

	body := `{"items":[{"name":"sbx-new","templateRef":{"name":"general-coding"}}]}`
	req := bulkRequest(http.MethodPost, "/api/v1/sandboxes/bulk", body, adminClaims())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 (whole-batch cap rejection), got %d body=%s", rec.Code, rec.Body.String())
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "sbx-new", Namespace: "default"}, &agenttierv1alpha1.Sandbox{}); err == nil {
		t.Error("sbx-new should not exist — batch was rejected fail-fast at the aggregate cap")
	}
}

func TestBulkCreate_ScopedKeyRejected(t *testing.T) {
	_, _, r := bulkFixture(t)
	scoped := &Claims{Sub: "agent-1", SandboxID: "sbx-owner", ActionGroups: []string{"run-command"}}
	body := `{"items":[{"name":"sbx-x","templateRef":{"name":"general-coding"}}]}`
	req := bulkRequest(http.MethodPost, "/api/v1/sandboxes/bulk", body, scoped)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for sandbox-scoped key on bulk create, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBulkCreate_UnauthenticatedRejected(t *testing.T) {
	_, _, r := bulkFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/bulk", strings.NewReader(`{"items":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(`{"items":[]}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no claims in context, got %d", rec.Code)
	}
}

// --- handleBulkAction ---

func TestBulkAction_PerItemPartialSuccess(t *testing.T) {
	s, c, r := bulkFixture(t)
	ctx := context.Background()

	running := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-running", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "u-test"}},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
	stopped := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-stopped", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "u-test"}},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseStopped},
	}
	if err := c.Create(ctx, running); err != nil {
		t.Fatalf("create running: %v", err)
	}
	if err := c.Create(ctx, stopped); err != nil {
		t.Fatalf("create stopped: %v", err)
	}
	_ = s

	// stop sbx-running (ok), stop sbx-stopped (already stopped -> error),
	// stop a nonexistent id (-> error). One request, per-item results.
	body := `{"action":"stop","ids":["sbx-running","sbx-stopped","does-not-exist"]}`
	req := bulkRequest(http.MethodPost, "/api/v1/sandboxes/bulk-action", body, adminClaims())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (per-item results, not a batch error), got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Results []bulkActionItemResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v body=%s", err, rec.Body.String())
	}
	if len(resp.Results) != 3 {
		t.Fatalf("expected 3 results, got %d: %+v", len(resp.Results), resp.Results)
	}
	byID := map[string]bulkActionItemResult{}
	for _, res := range resp.Results {
		byID[res.ID] = res
	}
	if byID["sbx-running"].Status != "ok" {
		t.Errorf("sbx-running: expected ok, got %+v", byID["sbx-running"])
	}
	if byID["sbx-stopped"].Status != "error" {
		t.Errorf("sbx-stopped (already stopped): expected error, got %+v", byID["sbx-stopped"])
	}
	if byID["does-not-exist"].Status != "error" {
		t.Errorf("does-not-exist: expected error, got %+v", byID["does-not-exist"])
	}

	// The one valid item's effect must have actually applied.
	got := &agenttierv1alpha1.Sandbox{}
	if err := c.Get(ctx, client.ObjectKey{Name: "sbx-running", Namespace: "default"}, got); err != nil {
		t.Fatalf("get sbx-running: %v", err)
	}
	if got.Annotations["agenttier.io/action"] != "stop" {
		t.Errorf("expected stop annotation on sbx-running, got %+v", got.Annotations)
	}
}

func TestBulkAction_OtherUsersSandboxIsPerItemFailureNotBatchAbort(t *testing.T) {
	_, c, r := bulkFixture(t)
	ctx := context.Background()

	mine := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-mine", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "someone-else"}},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
	if err := c.Create(ctx, mine); err != nil {
		t.Fatalf("create: %v", err)
	}

	nonAdmin := &Claims{Sub: "u-test", IsAdmin: false}
	body := `{"action":"stop","ids":["sbx-mine"]}`
	req := bulkRequest(http.MethodPost, "/api/v1/sandboxes/bulk-action", body, nonAdmin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	// Batch itself still returns 200 — the failure is reported per-item.
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (batch never aborts), got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results []bulkActionItemResult `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Status != "error" {
		t.Fatalf("expected 1 per-item error result for another user's sandbox, got %+v", resp.Results)
	}
}

func TestBulkAction_RejectsUnknownAction(t *testing.T) {
	_, _, r := bulkFixture(t)
	body := `{"action":"reboot","ids":["sbx-1"]}`
	req := bulkRequest(http.MethodPost, "/api/v1/sandboxes/bulk-action", body, adminClaims())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown action, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBulkAction_RejectsEmptyIDs(t *testing.T) {
	_, _, r := bulkFixture(t)
	body := `{"action":"stop","ids":[]}`
	req := bulkRequest(http.MethodPost, "/api/v1/sandboxes/bulk-action", body, adminClaims())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty ids, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBulkAction_ScopedKeyRejected(t *testing.T) {
	_, c, r := bulkFixture(t)
	ctx := context.Background()
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-owner", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "agent-1"}},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
	if err := c.Create(ctx, sb); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Even a scoped key acting on its OWN bound sandbox must be rejected —
	// bulk-action is not a sandbox-scoped operation at all (DD3), so the
	// 403 here is not conditional on which IDs appear in the batch.
	scoped := &Claims{Sub: "agent-1", SandboxID: "sbx-owner", ActionGroups: []string{"stop", "resume"}}
	body := `{"action":"stop","ids":["sbx-owner"]}`
	req := bulkRequest(http.MethodPost, "/api/v1/sandboxes/bulk-action", body, scoped)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for sandbox-scoped key on bulk-action (even for its own sandbox), got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestBulkAction_DeleteRemovesCR(t *testing.T) {
	_, c, r := bulkFixture(t)
	ctx := context.Background()
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-to-delete", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "u-test"}},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
	if err := c.Create(ctx, sb); err != nil {
		t.Fatalf("create: %v", err)
	}

	body := `{"action":"delete","ids":["sbx-to-delete"]}`
	req := bulkRequest(http.MethodPost, "/api/v1/sandboxes/bulk-action", body, adminClaims())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "sbx-to-delete", Namespace: "default"}, &agenttierv1alpha1.Sandbox{}); err == nil {
		t.Error("expected sandbox to be deleted")
	}
}
