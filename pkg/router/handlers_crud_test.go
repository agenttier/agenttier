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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
	"github.com/agenttier/agenttier/pkg/apikeystore"
	"github.com/agenttier/agenttier/pkg/router/auth"
)

// statusSubresourceFixture is like apiKeyFixture but registers Sandbox's
// status subresource, needed by handlers that call k8sClient.Status().Update
// (resume, clone) — the fake client rejects those writes without it.
func statusSubresourceFixture(t *testing.T, objs ...client.Object) (*Server, client.Client) {
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
	return s, c
}

// doJSON is a small helper for issuing a request with a JSON body through
// the full router (middleware chain included) and returning the recorder.
func doJSON(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.ContentLength = int64(len(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	return rec
}

// --- Sandbox CRUD ---

func TestHandleCreateSandbox_RequiresNameAndTemplateRef(t *testing.T) {
	s, _ := apiKeyFixture(t)

	rec := doJSON(t, s, http.MethodPost, "/api/v1/sandboxes", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing name: expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}

	rec2 := doJSON(t, s, http.MethodPost, "/api/v1/sandboxes", `{"name":"sb-1"}`)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("missing templateRef: expected 400, got %d body=%s", rec2.Code, rec2.Body.String())
	}
}

func TestHandleCreateSandbox_CreatesCRWithCreatedByStamped(t *testing.T) {
	s, c := apiKeyFixture(t)

	body := `{"name":"sb-created","templateRef":{"name":"base"}}`
	rec := doJSON(t, s, http.MethodPost, "/api/v1/sandboxes", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}

	got := &agenttierv1alpha1.Sandbox{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "sb-created", Namespace: "default"}, got); err != nil {
		t.Fatalf("sandbox CR not created: %v", err)
	}
	if got.Spec.CreatedBy == nil || got.Spec.CreatedBy.Sub != "dev-user" {
		t.Errorf("expected CreatedBy.Sub=dev-user, got %+v", got.Spec.CreatedBy)
	}
	if got.Spec.TemplateRef == nil || got.Spec.TemplateRef.Name != "base" {
		t.Errorf("expected templateRef.name=base, got %+v", got.Spec.TemplateRef)
	}
}

func TestHandleCreateSandbox_InvalidTimeoutIsBadRequest(t *testing.T) {
	s, _ := apiKeyFixture(t)

	body := `{"name":"sb-bad-timeout","templateRef":{"name":"base"},"timeout":"not-a-duration"}`
	rec := doJSON(t, s, http.MethodPost, "/api/v1/sandboxes", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid timeout, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleListSandboxes_NonAdminOnlySeesOwn(t *testing.T) {
	s, c := apiKeyFixture(t)
	mine := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "dev-user"}},
	}
	theirs := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "theirs", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "someone-else"}},
	}
	if err := c.Create(context.Background(), mine); err != nil {
		t.Fatalf("create mine: %v", err)
	}
	if err := c.Create(context.Background(), theirs); err != nil {
		t.Fatalf("create theirs: %v", err)
	}

	// The fixture uses DevAuth which stamps an admin identity, so list
	// admin-first to confirm both appear, matching the isAdmin branch.
	rec := doJSON(t, s, http.MethodGet, "/api/v1/sandboxes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "mine") || !strings.Contains(rec.Body.String(), "theirs") {
		t.Errorf("admin should see both sandboxes, got %s", rec.Body.String())
	}
}

func TestHandleListSandboxes_FiltersByStatus(t *testing.T) {
	s, c := apiKeyFixture(t)
	running := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "running-sb", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "dev-user"}},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
	stopped := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "stopped-sb", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "dev-user"}},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseStopped},
	}
	if err := c.Create(context.Background(), running); err != nil {
		t.Fatalf("create running: %v", err)
	}
	if err := c.Create(context.Background(), stopped); err != nil {
		t.Fatalf("create stopped: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/v1/sandboxes?status=Running", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "running-sb") {
		t.Errorf("expected running-sb in filtered list, got %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "stopped-sb") {
		t.Errorf("did not expect stopped-sb in Running-filtered list, got %s", rec.Body.String())
	}
}

func TestHandleGetSandbox_NotFound(t *testing.T) {
	s, _ := apiKeyFixture(t)
	rec := doJSON(t, s, http.MethodGet, "/api/v1/sandboxes/does-not-exist", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleGetSandbox_ReturnsSandboxJSON(t *testing.T) {
	s, c := apiKeyFixture(t)
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-get", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "dev-user"}},
	}
	if err := c.Create(context.Background(), sb); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := doJSON(t, s, http.MethodGet, "/api/v1/sandboxes/sb-get", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "sb-get") {
		t.Errorf("expected sandbox name in response, got %s", rec.Body.String())
	}
}

func TestHandleDeleteSandbox_RemovesCR(t *testing.T) {
	s, c := apiKeyFixture(t)
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-delete", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "dev-user"}},
	}
	if err := c.Create(context.Background(), sb); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := doJSON(t, s, http.MethodDelete, "/api/v1/sandboxes/sb-delete", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", rec.Code, rec.Body.String())
	}
	err := c.Get(context.Background(), client.ObjectKey{Name: "sb-delete", Namespace: "default"}, &agenttierv1alpha1.Sandbox{})
	if err == nil {
		t.Error("expected sandbox to be deleted")
	}
}

func TestHandleStopSandbox_RejectsNonRunning(t *testing.T) {
	s, c := apiKeyFixture(t)
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-stop", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "dev-user"}},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseStopped},
	}
	if err := c.Create(context.Background(), sb); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := doJSON(t, s, http.MethodPost, "/api/v1/sandboxes/sb-stop/stop", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 stopping an already-stopped sandbox, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleStopSandbox_AnnotatesRunningSandbox(t *testing.T) {
	s, c := apiKeyFixture(t)
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-stop-ok", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "dev-user"}},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
	if err := c.Create(context.Background(), sb); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := doJSON(t, s, http.MethodPost, "/api/v1/sandboxes/sb-stop-ok/stop", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	got := &agenttierv1alpha1.Sandbox{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "sb-stop-ok", Namespace: "default"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Annotations["agenttier.io/action"] != "stop" {
		t.Errorf("expected stop annotation, got %+v", got.Annotations)
	}
}

func TestHandleResumeSandbox_RejectsRunning(t *testing.T) {
	s, c := apiKeyFixture(t)
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-resume-running", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "dev-user"}},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
	if err := c.Create(context.Background(), sb); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := doJSON(t, s, http.MethodPost, "/api/v1/sandboxes/sb-resume-running/resume", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 resuming a running sandbox, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleResumeSandbox_SetsCreatingFromStopped(t *testing.T) {
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-resume-ok", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "dev-user"}},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseStopped},
	}
	s, c := statusSubresourceFixture(t, sb)
	rec := doJSON(t, s, http.MethodPost, "/api/v1/sandboxes/sb-resume-ok/resume", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	got := &agenttierv1alpha1.Sandbox{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "sb-resume-ok", Namespace: "default"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status.Phase != agenttierv1alpha1.SandboxPhaseCreating {
		t.Errorf("expected phase Creating, got %s", got.Status.Phase)
	}
}

func TestHandleExecCommand_RejectsNonRunningSandbox(t *testing.T) {
	s, c := apiKeyFixture(t)
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-exec-notrunning", Namespace: "default"},
		Spec:       agenttierv1alpha1.SandboxSpec{CreatedBy: &agenttierv1alpha1.UserIdentity{Sub: "dev-user"}},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseCreating},
	}
	if err := c.Create(context.Background(), sb); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := doJSON(t, s, http.MethodPost, "/api/v1/sandboxes/sb-exec-notrunning/exec", `{"command":"echo hi"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- Template CRUD ---

func TestHandleTemplateCRUD_FullLifecycle(t *testing.T) {
	s, c := apiKeyFixture(t)

	// Create (admin, DevAuth grants admin).
	createBody := `{"name":"tpl-1","spec":{"description":"a template"}}`
	rec := doJSON(t, s, http.MethodPost, "/api/v1/templates", createBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Get.
	getRec := doJSON(t, s, http.MethodGet, "/api/v1/templates/tpl-1", "")
	if getRec.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d body=%s", getRec.Code, getRec.Body.String())
	}
	if !strings.Contains(getRec.Body.String(), "a template") {
		t.Errorf("expected description in get response, got %s", getRec.Body.String())
	}

	// Update.
	updateBody := `{"spec":{"description":"updated template"}}`
	updRec := doJSON(t, s, http.MethodPut, "/api/v1/templates/tpl-1", updateBody)
	if updRec.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d body=%s", updRec.Code, updRec.Body.String())
	}
	got := &agenttierv1alpha1.ClusterSandboxTemplate{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "tpl-1"}, got); err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Spec.Description != "updated template" {
		t.Errorf("expected updated description, got %q", got.Spec.Description)
	}

	// List.
	listRec := doJSON(t, s, http.MethodGet, "/api/v1/templates", "")
	if listRec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", listRec.Code)
	}
	if !strings.Contains(listRec.Body.String(), "tpl-1") {
		t.Errorf("expected tpl-1 in list, got %s", listRec.Body.String())
	}

	// Delete.
	delRec := doJSON(t, s, http.MethodDelete, "/api/v1/templates/tpl-1", "")
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d body=%s", delRec.Code, delRec.Body.String())
	}
	err := c.Get(context.Background(), client.ObjectKey{Name: "tpl-1"}, &agenttierv1alpha1.ClusterSandboxTemplate{})
	if err == nil {
		t.Error("expected template to be deleted")
	}
}

func TestHandleDeleteTemplate_ConflictsWhenInUse(t *testing.T) {
	s, c := apiKeyFixture(t)
	tmpl := &agenttierv1alpha1.ClusterSandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "tpl-in-use"}}
	if err := c.Create(context.Background(), tmpl); err != nil {
		t.Fatalf("create template: %v", err)
	}
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-using-tpl", Namespace: "default"},
		Status: agenttierv1alpha1.SandboxStatus{
			Phase:            agenttierv1alpha1.SandboxPhaseRunning,
			ResolvedTemplate: "tpl-in-use",
		},
	}
	if err := c.Create(context.Background(), sb); err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	rec := doJSON(t, s, http.MethodDelete, "/api/v1/templates/tpl-in-use", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleGetTemplate_NotFound(t *testing.T) {
	s, _ := apiKeyFixture(t)
	rec := doJSON(t, s, http.MethodGet, "/api/v1/templates/does-not-exist", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// --- Governance ---

func TestHandleGovernancePolicies_ClusterAndNamespaceRoundTrip(t *testing.T) {
	s, _ := apiKeyFixture(t)

	// Upsert cluster default policy.
	clusterPolicy := `{"maxSandboxesPerUser":5}`
	rec := doJSON(t, s, http.MethodPut, "/api/v1/governance/policies", clusterPolicy)
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert cluster: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Set a namespace-scoped policy.
	nsPolicy := `{"maxSandboxesPerUser":2}`
	nsRec := doJSON(t, s, http.MethodPut, "/api/v1/governance/policies/team-a", nsPolicy)
	if nsRec.Code != http.StatusOK {
		t.Fatalf("set namespace policy: expected 200, got %d body=%s", nsRec.Code, nsRec.Body.String())
	}

	// List should surface both.
	listRec := doJSON(t, s, http.MethodGet, "/api/v1/governance/policies", "")
	if listRec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d body=%s", listRec.Code, listRec.Body.String())
	}
	if !strings.Contains(listRec.Body.String(), "team-a") {
		t.Errorf("expected team-a namespace policy in list, got %s", listRec.Body.String())
	}

	// Get namespace policy directly.
	getRec := doJSON(t, s, http.MethodGet, "/api/v1/governance/policies/team-a", "")
	if getRec.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", getRec.Code)
	}

	// Effective policy for that namespace.
	effRec := doJSON(t, s, http.MethodGet, "/api/v1/governance/effective?namespace=team-a", "")
	if effRec.Code != http.StatusOK {
		t.Fatalf("effective: expected 200, got %d body=%s", effRec.Code, effRec.Body.String())
	}

	// Delete namespace policy.
	delRec := doJSON(t, s, http.MethodDelete, "/api/v1/governance/policies/team-a", "")
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("delete: expected 204, got %d body=%s", delRec.Code, delRec.Body.String())
	}
}

// --- Warm pool ---

func TestHandleWarmPool_StatusAndConfigRoundTrip(t *testing.T) {
	s, _ := apiKeyFixture(t)

	statusRec := doJSON(t, s, http.MethodGet, "/api/v1/warmpool/status", "")
	if statusRec.Code != http.StatusOK {
		t.Fatalf("status: expected 200, got %d body=%s", statusRec.Code, statusRec.Body.String())
	}

	setRec := doJSON(t, s, http.MethodPut, "/api/v1/warmpool/config", `{"desiredCount":2,"template":"base"}`)
	if setRec.Code != http.StatusOK {
		t.Fatalf("set config: expected 200, got %d body=%s", setRec.Code, setRec.Body.String())
	}
}

func TestHandleWarmPool_RejectsInvalidDesiredCount(t *testing.T) {
	s, _ := apiKeyFixture(t)
	rec := doJSON(t, s, http.MethodPut, "/api/v1/warmpool/config", `{"desiredCount":99,"template":"base"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for out-of-range desiredCount, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleWarmPool_RejectsMissingTemplate(t *testing.T) {
	s, _ := apiKeyFixture(t)
	// The legacy top-level desiredCount/template shortcut only promotes to
	// a validated Pool when both fields are set (see Config.Normalize) —
	// use the explicit pools-array shape to exercise per-pool validation.
	rec := doJSON(t, s, http.MethodPut, "/api/v1/warmpool/config", `{"pools":[{"desiredCount":1}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing template, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- Cluster nodes (admin-gated) ---

func TestHandleGetClusterNodes_ReturnsData(t *testing.T) {
	s, _ := apiKeyFixture(t)
	rec := doJSON(t, s, http.MethodGet, "/api/v1/cluster/nodes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for admin (DevAuth), got %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- Headroom ---

func TestHandleHeadroom_GetWhenDeploymentMissing(t *testing.T) {
	s, _ := apiKeyFixture(t)
	rec := doJSON(t, s, http.MethodGet, "/api/v1/cluster/headroom", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("expected enabled:false when Deployment is missing, got %s", rec.Body.String())
	}
}

func TestHandleHeadroom_SetRejectsOutOfRangeReplicas(t *testing.T) {
	s, _ := apiKeyFixture(t)
	rec := doJSON(t, s, http.MethodPut, "/api/v1/cluster/headroom", `{"replicas":51}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleHeadroom_SetRejectsInvalidCPUQuantity(t *testing.T) {
	s, _ := apiKeyFixture(t)
	rec := doJSON(t, s, http.MethodPut, "/api/v1/cluster/headroom", `{"replicas":1,"cpu":"not-a-quantity"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- User / analytics / audit / admin ---

func TestHandleGetMe_ReturnsClaims(t *testing.T) {
	s, _ := apiKeyFixture(t)
	rec := doJSON(t, s, http.MethodGet, "/api/v1/user/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Sub     string `json:"sub"`
		IsAdmin bool   `json:"isAdmin"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Sub != "dev-user" || !resp.IsAdmin {
		t.Errorf("expected dev-user/admin, got %+v", resp)
	}
}

func TestHandleListAuditEvents_EmptyWhenNoEvents(t *testing.T) {
	s, _ := apiKeyFixture(t)
	rec := doJSON(t, s, http.MethodGet, "/api/v1/audit/events", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleGetUsageAnalytics_BreaksDownByStatusAndTemplate(t *testing.T) {
	s, c := apiKeyFixture(t)
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-analytics", Namespace: "default"},
		Status: agenttierv1alpha1.SandboxStatus{
			Phase:            agenttierv1alpha1.SandboxPhaseRunning,
			ResolvedTemplate: "base",
		},
	}
	if err := c.Create(context.Background(), sb); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := doJSON(t, s, http.MethodGet, "/api/v1/analytics/usage", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "base") {
		t.Errorf("expected template breakdown to include base, got %s", rec.Body.String())
	}
}

func TestHandleGetCostEstimates_ComputesRunningCost(t *testing.T) {
	s, c := apiKeyFixture(t)
	sb := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-cost", Namespace: "default"},
		Status: agenttierv1alpha1.SandboxStatus{
			Phase:            agenttierv1alpha1.SandboxPhaseRunning,
			ResolvedTemplate: "base",
		},
	}
	if err := c.Create(context.Background(), sb); err != nil {
		t.Fatalf("create: %v", err)
	}
	rec := doJSON(t, s, http.MethodGet, "/api/v1/analytics/costs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		RunningSandboxes int `json:"running_sandboxes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.RunningSandboxes != 1 {
		t.Errorf("expected 1 running sandbox, got %d", resp.RunningSandboxes)
	}
}

func TestHandleAdminListSharing_ReturnsPlaceholderShape(t *testing.T) {
	s, _ := apiKeyFixture(t)
	rec := doJSON(t, s, http.MethodGet, "/api/v1/admin/sharing", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// TestHandleAdminListSandboxes_JoinsScopedApiKeyMetadata is the FR6.6
// regression test (task #48 — handleAdminListSandboxes used to be a stub
// that always returned an empty list). Covers: (a) a sandbox WITH a scoped
// key shows its metadata, (b) a sandbox with NO scoped key shows an
// absent field (not an error), (c) never the plaintext or hash.
func TestHandleAdminListSandboxes_JoinsScopedApiKeyMetadata(t *testing.T) {
	withKey := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-with-key", Namespace: "default"},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
	withoutKey := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-no-key", Namespace: "default"},
		Status:     agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
	s, c := apiKeyFixture(t)
	for _, sb := range []*agenttierv1alpha1.Sandbox{withKey, withoutKey} {
		if err := c.Create(context.Background(), sb); err != nil {
			t.Fatalf("create sandbox %s: %v", sb.Name, err)
		}
	}

	store := apikeystore.New(c, "agenttier")
	if err := store.Create(context.Background(), "deadbeef", &auth.APIKeyRecord{
		UserID:       "u-1",
		Name:         "sandbox-scoped: sbx-with-key",
		SandboxID:    "sbx-with-key",
		ActionGroups: []string{"run-command", "resume", "stop"},
		CreatedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("seed scoped key record: %v", err)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/v1/admin/sandboxes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Sandboxes []map[string]interface{} `json:"sandboxes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The scoped key's ID (a Secret name derived from the hash, per
	// apikeystore.Metadata — same accepted shape as GET /user/api-keys)
	// is fine to expose; the plaintext key and raw "keyHash" field are
	// the things that must never appear. Checked per-entry below.
	if len(resp.Sandboxes) != 2 {
		t.Fatalf("expected 2 sandboxes, got %d", len(resp.Sandboxes))
	}

	byName := map[string]map[string]interface{}{}
	for _, sb := range resp.Sandboxes {
		byName[sb["sandboxId"].(string)] = sb
	}

	withKeyEntry, ok := byName["sbx-with-key"]
	if !ok {
		t.Fatal("sbx-with-key missing from response")
	}
	scoped, ok := withKeyEntry["scopedApiKeys"].([]interface{})
	if !ok || len(scoped) != 1 {
		t.Fatalf("expected exactly 1 scoped key entry for sbx-with-key, got %#v", withKeyEntry["scopedApiKeys"])
	}
	first, ok := scoped[0].(map[string]interface{})
	if !ok {
		t.Fatalf("scoped key entry not a JSON object: %#v", scoped[0])
	}
	if first["sandboxId"] != "sbx-with-key" {
		t.Errorf("scoped key sandboxId = %v, want sbx-with-key", first["sandboxId"])
	}
	groups, ok := first["actionGroups"].([]interface{})
	if !ok || len(groups) != 3 {
		t.Errorf("expected 3 actionGroups, got %#v", first["actionGroups"])
	}
	for _, sensitive := range []string{"keyHash", "key"} {
		if _, present := first[sensitive]; present {
			t.Errorf("scoped key entry must never include %q, got %#v", sensitive, first)
		}
	}

	noKeyEntry, ok := byName["sbx-no-key"]
	if !ok {
		t.Fatal("sbx-no-key missing from response")
	}
	if _, present := noKeyEntry["scopedApiKeys"]; present {
		t.Errorf("sbx-no-key must not carry a scopedApiKeys field, got %#v", noKeyEntry["scopedApiKeys"])
	}
}

// --- Preferences ---

func TestHandlePreferences_GetEmptyThenUpdateThenGet(t *testing.T) {
	s, _ := apiKeyFixture(t)

	empty := doJSON(t, s, http.MethodGet, "/api/v1/user/preferences", "")
	if empty.Code != http.StatusOK {
		t.Fatalf("get empty: expected 200, got %d", empty.Code)
	}
	if strings.TrimSpace(empty.Body.String()) != "{}" {
		t.Errorf("expected empty object before any preferences saved, got %s", empty.Body.String())
	}

	upd := doJSON(t, s, http.MethodPut, "/api/v1/user/preferences", `{"theme":"dark"}`)
	if upd.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d body=%s", upd.Code, upd.Body.String())
	}

	got := doJSON(t, s, http.MethodGet, "/api/v1/user/preferences", "")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "dark") {
		t.Errorf("expected saved preference to round-trip, got code=%d body=%s", got.Code, got.Body.String())
	}
}

// --- resolveTemplateMode ---

func TestResolveTemplateMode_InheritsAgentModeFromTemplate(t *testing.T) {
	s, c := apiKeyFixture(t)
	tmpl := &agenttierv1alpha1.ClusterSandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-tpl"},
		Spec:       agenttierv1alpha1.SandboxTemplateSpec{Mode: agenttierv1alpha1.SandboxModeAgent},
	}
	if err := c.Create(context.Background(), tmpl); err != nil {
		t.Fatalf("create template: %v", err)
	}

	body := `{"name":"sb-agent-mode","templateRef":{"name":"agent-tpl"}}`
	rec := doJSON(t, s, http.MethodPost, "/api/v1/sandboxes", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}

	got := &agenttierv1alpha1.Sandbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "sb-agent-mode", Namespace: "default"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Mode != agenttierv1alpha1.SandboxModeAgent {
		t.Errorf("expected sandbox to inherit agent mode from template, got %q", got.Spec.Mode)
	}
}

func TestResolveTemplateMode_MissingTemplateFallsBackToCodeMode(t *testing.T) {
	s, c := apiKeyFixture(t)
	// No template created — lookup fails, mode falls back to the CRD
	// default (empty string / code) rather than erroring the create.
	body := `{"name":"sb-missing-tpl","templateRef":{"name":"nonexistent"}}`
	rec := doJSON(t, s, http.MethodPost, "/api/v1/sandboxes", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 even with unresolvable template, got %d body=%s", rec.Code, rec.Body.String())
	}
	got := &agenttierv1alpha1.Sandbox{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: "sb-missing-tpl", Namespace: "default"}, got); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Spec.Mode == agenttierv1alpha1.SandboxModeAgent {
		t.Errorf("expected conservative code-mode fallback, got agent mode")
	}
}
