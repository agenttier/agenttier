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
	webhookdelivery "github.com/agenttier/agenttier/pkg/controller/webhook_delivery"
)

// webhookFixture builds a Server plus a standalone mux wired to the FR5
// webhook handlers. Route wiring into the real s.router happens in the
// Group 2b serialization task (server.go); tests here register their own
// minimal router, matching backup_handlers_test.go's approach. objs seeds
// the fake client (e.g. Sandbox fixtures for the sandboxId-ownership checks
// added in task #50) exactly like bulkFixture/backupFixture already do.
func webhookFixture(t *testing.T, objs ...client.Object) (*Server, http.Handler) {
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
	api.HandleFunc("/webhooks", s.handleCreateWebhook).Methods("POST")
	api.HandleFunc("/webhooks", s.handleListWebhooks).Methods("GET")
	api.HandleFunc("/webhooks/{id}", s.handleDeleteWebhook).Methods("DELETE")
	api.HandleFunc("/webhooks/{id}/deliveries", s.handleGetWebhookDeliveries).Methods("GET")

	return s, r
}

// ownedSandbox returns a running Sandbox created by ownerSub — the shape
// handleCreateWebhook's getSandboxWithAuthCheck call needs to find and
// authorize against for a scoped (sandboxId-bound) subscription.
func ownedSandbox(name, namespace, ownerSub string) *agenttierv1alpha1.Sandbox {
	sb := runningSandbox(name, namespace)
	sb.Spec.CreatedBy = &agenttierv1alpha1.UserIdentity{Sub: ownerSub}
	return sb
}

func webhookRequest(method, path, body string, claims *Claims) *http.Request {
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

func webhookUserClaims(sub string) *Claims {
	return &Claims{Sub: sub, Email: sub + "@agenttier.local", Name: "Test User"}
}

// --- handleCreateWebhook ---

func TestCreateWebhook_ReturnsSecretOnce(t *testing.T) {
	sandbox := ownedSandbox("sbx-1", "default", "u1")
	_, r := webhookFixture(t, sandbox)
	body := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"sbx-1"}`
	req := webhookRequest(http.MethodPost, "/api/v1/webhooks", body, webhookUserClaims("u1"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var created map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	secret, _ := created["secret"].(string)
	if secret == "" {
		t.Fatal("expected non-empty secret in create response")
	}

	// A subsequent list must never surface the secret.
	listReq := webhookRequest(http.MethodGet, "/api/v1/webhooks", "", webhookUserClaims("u1"))
	listRec := httptest.NewRecorder()
	r.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", listRec.Code)
	}
	var listed struct {
		Webhooks []map[string]interface{} `json:"webhooks"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("invalid list JSON: %v", err)
	}
	if len(listed.Webhooks) != 1 {
		t.Fatalf("expected 1 webhook, got %d", len(listed.Webhooks))
	}
	if _, has := listed.Webhooks[0]["secret"]; has {
		t.Error("list response must never include the HMAC secret")
	}
}

func TestCreateWebhook_RejectsNonHTTPS(t *testing.T) {
	sandbox := ownedSandbox("sbx-1", "default", "u1")
	_, r := webhookFixture(t, sandbox)
	body := `{"url":"http://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"sbx-1"}`
	req := webhookRequest(http.MethodPost, "/api/v1/webhooks", body, webhookUserClaims("u1"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-https url, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateWebhook_RejectsSSRFTargets(t *testing.T) {
	sandbox := ownedSandbox("sbx-1", "default", "u1")
	_, r := webhookFixture(t, sandbox)
	ssrfURLs := []string{
		"https://127.0.0.1/hook",       // loopback
		"https://169.254.169.254/hook", // cloud metadata
		"https://10.0.0.5/hook",        // RFC 1918
		"https://172.16.0.5/hook",      // RFC 1918
		"https://192.168.1.5/hook",     // RFC 1918
	}
	for _, url := range ssrfURLs {
		body := `{"url":"` + url + `","eventTypes":["sandbox.running"],"sandboxId":"sbx-1"}`
		req := webhookRequest(http.MethodPost, "/api/v1/webhooks", body, webhookUserClaims("u1"))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("url %q: expected 400 (SSRF guard), got %d body=%s", url, rec.Code, rec.Body.String())
		}
	}
}

func TestCreateWebhook_RejectsEmptyOrUnknownEventTypes(t *testing.T) {
	sandbox := ownedSandbox("sbx-1", "default", "u1")
	_, r := webhookFixture(t, sandbox)
	cases := []string{
		`{"url":"https://8.8.8.8/hook","eventTypes":[],"sandboxId":"sbx-1"}`,
		`{"url":"https://8.8.8.8/hook","eventTypes":["not.a.real.event"],"sandboxId":"sbx-1"}`,
		`{"url":"https://8.8.8.8/hook","sandboxId":"sbx-1"}`,
	}
	for _, body := range cases {
		req := webhookRequest(http.MethodPost, "/api/v1/webhooks", body, webhookUserClaims("u1"))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: expected 400, got %d body=%s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestCreateWebhook_RejectsMissingURL(t *testing.T) {
	sandbox := ownedSandbox("sbx-1", "default", "u1")
	_, r := webhookFixture(t, sandbox)
	body := `{"eventTypes":["sandbox.running"],"sandboxId":"sbx-1"}`
	req := webhookRequest(http.MethodPost, "/api/v1/webhooks", body, webhookUserClaims("u1"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing url, got %d", rec.Code)
	}
}

func TestCreateWebhook_RequiresAuth(t *testing.T) {
	_, r := webhookFixture(t)
	body := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"sbx-1"}`
	req := webhookRequest(http.MethodPost, "/api/v1/webhooks", body, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no claims, got %d", rec.Code)
	}
}

// --- authorization gap fix (task #50, CRITICAL) ---

// TestCreateWebhook_RejectsSandboxNotOwnedByCaller is the primary
// regression test for the cross-tenant disclosure hole: a non-admin
// caller naming a sandboxId they don't own must get 404 (not 403 — the
// repo's "don't confirm existence to a caller with no claim to it"
// convention), and the subscription must never be created.
func TestCreateWebhook_RejectsSandboxNotOwnedByCaller(t *testing.T) {
	victimSandbox := ownedSandbox("victims-sandbox", "default", "victim")
	_, r := webhookFixture(t, victimSandbox)

	body := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"victims-sandbox"}`
	req := webhookRequest(http.MethodPost, "/api/v1/webhooks", body, webhookUserClaims("attacker"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a sandbox the caller doesn't own, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Confirm no subscription was persisted despite the attempt.
	listReq := webhookRequest(http.MethodGet, "/api/v1/webhooks", "", webhookUserClaims("attacker"))
	listRec := httptest.NewRecorder()
	r.ServeHTTP(listRec, listReq)
	var listed struct {
		Webhooks []map[string]interface{} `json:"webhooks"`
	}
	_ = json.Unmarshal(listRec.Body.Bytes(), &listed)
	if len(listed.Webhooks) != 0 {
		t.Fatalf("expected no subscription to be created on a rejected request, got %d", len(listed.Webhooks))
	}
}

// TestCreateWebhook_RejectsUnscopedSubscriptionFromNonAdmin is the second
// half of the disclosure hole: omitting sandboxId entirely (the
// "receive-everything" case) must require admin, not just any
// authenticated caller.
func TestCreateWebhook_RejectsUnscopedSubscriptionFromNonAdmin(t *testing.T) {
	_, r := webhookFixture(t)
	body := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"]}`
	req := webhookRequest(http.MethodPost, "/api/v1/webhooks", body, webhookUserClaims("u1"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an unscoped subscription from a non-admin, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCreateWebhook_AdminCanCreateUnscopedSubscription confirms the admin
// escape hatch still works — the fix must not accidentally block the
// legitimate cluster-wide-monitoring use case for admins.
func TestCreateWebhook_AdminCanCreateUnscopedSubscription(t *testing.T) {
	_, r := webhookFixture(t)
	admin := webhookUserClaims("admin")
	admin.IsAdmin = true
	body := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"]}`
	req := webhookRequest(http.MethodPost, "/api/v1/webhooks", body, admin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for an admin's unscoped subscription, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCreateWebhook_OwnerCanCreateScopedSubscription confirms the fix
// doesn't over-correct: a caller registering a subscription scoped to a
// sandbox they DO own must still succeed.
func TestCreateWebhook_OwnerCanCreateScopedSubscription(t *testing.T) {
	sandbox := ownedSandbox("my-sandbox", "default", "u1")
	_, r := webhookFixture(t, sandbox)
	body := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"my-sandbox"}`
	req := webhookRequest(http.MethodPost, "/api/v1/webhooks", body, webhookUserClaims("u1"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a subscription scoped to the caller's own sandbox, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCreateWebhook_AdminCanCreateSubscriptionForAnySandbox confirms an
// admin's existing "act on any sandbox" privilege extends to webhook
// subscription creation, not just user-level subscriptions.
func TestCreateWebhook_AdminCanCreateSubscriptionForAnySandbox(t *testing.T) {
	sandbox := ownedSandbox("someone-elses-sandbox", "default", "someone-else")
	_, r := webhookFixture(t, sandbox)
	admin := webhookUserClaims("admin")
	admin.IsAdmin = true
	body := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"someone-elses-sandbox"}`
	req := webhookRequest(http.MethodPost, "/api/v1/webhooks", body, admin)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for an admin subscribing to another user's sandbox, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- handleListWebhooks ---

func TestListWebhooks_OwnOnly(t *testing.T) {
	sbx1 := ownedSandbox("sbx-u1", "default", "u1")
	sbx2 := ownedSandbox("sbx-u2", "default", "u2")
	_, r := webhookFixture(t, sbx1, sbx2)

	// u1 creates one subscription.
	req1 := webhookRequest(http.MethodPost, "/api/v1/webhooks", `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"sbx-u1"}`, webhookUserClaims("u1"))
	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("u1 create: expected 201, got %d body=%s", rec1.Code, rec1.Body.String())
	}

	// u2 creates a different one.
	req2 := webhookRequest(http.MethodPost, "/api/v1/webhooks", `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"sbx-u2"}`, webhookUserClaims("u2"))
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("u2 create: expected 201, got %d body=%s", rec2.Code, rec2.Body.String())
	}

	// u1's list must show only u1's subscription.
	listReq := webhookRequest(http.MethodGet, "/api/v1/webhooks", "", webhookUserClaims("u1"))
	listRec := httptest.NewRecorder()
	r.ServeHTTP(listRec, listReq)
	var listed struct {
		Webhooks []map[string]interface{} `json:"webhooks"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(listed.Webhooks) != 1 {
		t.Fatalf("u1 should see exactly 1 webhook (their own), got %d", len(listed.Webhooks))
	}
}

// --- handleDeleteWebhook ---

func TestDeleteWebhook_OwnerCanDelete(t *testing.T) {
	sandbox := ownedSandbox("sbx-1", "default", "u1")
	_, r := webhookFixture(t, sandbox)
	createBody := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"sbx-1"}`
	createReq := webhookRequest(http.MethodPost, "/api/v1/webhooks", createBody, webhookUserClaims("u1"))
	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createReq)
	var created map[string]interface{}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)
	id := created["id"].(string)

	delReq := webhookRequest(http.MethodDelete, "/api/v1/webhooks/"+id, "", webhookUserClaims("u1"))
	delRec := httptest.NewRecorder()
	r.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%s", delRec.Code, delRec.Body.String())
	}
}

func TestDeleteWebhook_NonOwnerForbidden(t *testing.T) {
	sandbox := ownedSandbox("sbx-1", "default", "u1")
	_, r := webhookFixture(t, sandbox)
	createBody := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"sbx-1"}`
	createReq := webhookRequest(http.MethodPost, "/api/v1/webhooks", createBody, webhookUserClaims("u1"))
	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createReq)
	var created map[string]interface{}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)
	id := created["id"].(string)

	delReq := webhookRequest(http.MethodDelete, "/api/v1/webhooks/"+id, "", webhookUserClaims("u2"))
	delRec := httptest.NewRecorder()
	r.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-owner delete, got %d body=%s", delRec.Code, delRec.Body.String())
	}
}

func TestDeleteWebhook_AdminCanDeleteAnyones(t *testing.T) {
	sandbox := ownedSandbox("sbx-1", "default", "u1")
	_, r := webhookFixture(t, sandbox)
	createBody := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"sbx-1"}`
	createReq := webhookRequest(http.MethodPost, "/api/v1/webhooks", createBody, webhookUserClaims("u1"))
	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createReq)
	var created map[string]interface{}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)
	id := created["id"].(string)

	admin := webhookUserClaims("admin")
	admin.IsAdmin = true
	delReq := webhookRequest(http.MethodDelete, "/api/v1/webhooks/"+id, "", admin)
	delRec := httptest.NewRecorder()
	r.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNoContent {
		t.Fatalf("expected 204 for admin delete, got %d body=%s", delRec.Code, delRec.Body.String())
	}
}

func TestDeleteWebhook_NotFound(t *testing.T) {
	_, r := webhookFixture(t)
	delReq := webhookRequest(http.MethodDelete, "/api/v1/webhooks/wh_nonexistent", "", webhookUserClaims("u1"))
	delRec := httptest.NewRecorder()
	r.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", delRec.Code)
	}
}

// --- handleGetWebhookDeliveries ---

func TestGetWebhookDeliveries_OwnerSeesEmptyList(t *testing.T) {
	sandbox := ownedSandbox("sbx-1", "default", "u1")
	_, r := webhookFixture(t, sandbox)
	createBody := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"sbx-1"}`
	createReq := webhookRequest(http.MethodPost, "/api/v1/webhooks", createBody, webhookUserClaims("u1"))
	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createReq)
	var created map[string]interface{}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)
	id := created["id"].(string)

	req := webhookRequest(http.MethodGet, "/api/v1/webhooks/"+id+"/deliveries", "", webhookUserClaims("u1"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Deliveries []interface{} `json:"deliveries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if out.Deliveries == nil {
		t.Error("deliveries field must be present (empty array, not null)")
	}
}

// TestGetWebhookDeliveries_ReadsControllerRecordedHistory is the primary
// regression test for task #52: handleGetWebhookDeliveries must actually
// read the controller's delivery-state ConfigMap, not return a hardcoded
// empty array. Seeds the agenttier-webhook-cursor ConfigMap the exact way
// pkg/controller/webhook_delivery.Store.RecordAttempt would write it, then
// confirms the Router surfaces that subscription's history verbatim.
func TestGetWebhookDeliveries_ReadsControllerRecordedHistory(t *testing.T) {
	sandbox := ownedSandbox("sbx-1", "default", "u1")
	s, r := webhookFixture(t, sandbox)
	createBody := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"sbx-1"}`
	createReq := webhookRequest(http.MethodPost, "/api/v1/webhooks", createBody, webhookUserClaims("u1"))
	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createReq)
	var created map[string]interface{}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)
	id := created["id"].(string)

	// Seed the ConfigMap exactly as the controller's Store would, using its
	// own exported StateConfigMapName + DeliveryAttempt JSON shape so this
	// test breaks (loudly) if that cross-process contract ever drifts.
	stateJSON := `{"subscriptions":{"` + id + `":{"history":[` +
		`{"eventType":"sandbox.running","timestamp":"2026-07-21T12:00:00Z","statusCode":200,"attempt":1,"success":true},` +
		`{"eventType":"sandbox.stopped","timestamp":"2026-07-21T12:05:00Z","statusCode":500,"attempt":5,"success":false}` +
		`]}}}`
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      webhookdelivery.StateConfigMapName,
			Namespace: "agenttier",
		},
		Data: map[string]string{"state": stateJSON},
	}
	if err := s.k8sClient.Create(context.Background(), cm); err != nil {
		t.Fatalf("seed delivery state configmap: %v", err)
	}

	req := webhookRequest(http.MethodGet, "/api/v1/webhooks/"+id+"/deliveries", "", webhookUserClaims("u1"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Deliveries []webhookdelivery.DeliveryAttempt `json:"deliveries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(out.Deliveries) != 2 {
		t.Fatalf("expected 2 delivery attempts surfaced from the controller's history, got %d: %+v", len(out.Deliveries), out.Deliveries)
	}
	if out.Deliveries[0].EventType != "sandbox.running" || !out.Deliveries[0].Success || out.Deliveries[0].StatusCode != 200 {
		t.Errorf("unexpected first delivery attempt: %+v", out.Deliveries[0])
	}
	if out.Deliveries[1].EventType != "sandbox.stopped" || out.Deliveries[1].Success || out.Deliveries[1].StatusCode != 500 || out.Deliveries[1].Attempt != 5 {
		t.Errorf("unexpected second delivery attempt: %+v", out.Deliveries[1])
	}
}

// TestGetWebhookDeliveries_NoHistoryConfigMapReturnsEmptyArray confirms the
// "webhook-delivery isn't enabled / nothing delivered yet" case (the
// agenttier-webhook-cursor ConfigMap doesn't exist at all) still returns a
// clean empty array rather than a 500 — this is a legitimate state, not an
// error, on any install that hasn't enabled --webhook-delivery.
func TestGetWebhookDeliveries_NoHistoryConfigMapReturnsEmptyArray(t *testing.T) {
	sandbox := ownedSandbox("sbx-1", "default", "u1")
	_, r := webhookFixture(t, sandbox)
	createBody := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"sbx-1"}`
	createReq := webhookRequest(http.MethodPost, "/api/v1/webhooks", createBody, webhookUserClaims("u1"))
	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createReq)
	var created map[string]interface{}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)
	id := created["id"].(string)

	// No ConfigMap seeded at all — the well-known name is simply absent.
	req := webhookRequest(http.MethodGet, "/api/v1/webhooks/"+id+"/deliveries", "", webhookUserClaims("u1"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 even with no delivery-state configmap, got %d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Deliveries []webhookdelivery.DeliveryAttempt `json:"deliveries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if out.Deliveries == nil || len(out.Deliveries) != 0 {
		t.Errorf("expected an empty (non-nil) deliveries array, got %+v", out.Deliveries)
	}
}

func TestGetWebhookDeliveries_NonOwnerForbidden(t *testing.T) {
	sandbox := ownedSandbox("sbx-1", "default", "u1")
	_, r := webhookFixture(t, sandbox)
	createBody := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"sbx-1"}`
	createReq := webhookRequest(http.MethodPost, "/api/v1/webhooks", createBody, webhookUserClaims("u1"))
	createRec := httptest.NewRecorder()
	r.ServeHTTP(createRec, createReq)
	var created map[string]interface{}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)
	id := created["id"].(string)

	req := webhookRequest(http.MethodGet, "/api/v1/webhooks/"+id+"/deliveries", "", webhookUserClaims("u2"))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// --- scoped-key rejection (sa-review.md checklist item #3) ---
// requireSandboxScope itself is task #12's responsibility, but this
// documents the expectation at the handler-contract level: a scoped key's
// claims carry a non-empty SandboxID, and per DD3 the webhook routes are
// out-of-vocabulary entirely (no sandboxId-scoped semantics exist for
// webhooks). This test exercises the handlers directly with such claims to
// confirm they don't do anything scope-aware by accident (e.g. silently
// creating a sandbox-scoped webhook without going through the middleware)
// — the actual 403 enforcement is task #12's middleware, tested there.
func TestCreateWebhook_ScopedKeyClaimsStillWork_AtHandlerLevel(t *testing.T) {
	sandbox := ownedSandbox("sbx-1", "default", "sandbox-runtime")
	_, r := webhookFixture(t, sandbox)
	scoped := &Claims{Sub: "sandbox-runtime", SandboxID: "sbx-1", ActionGroups: []string{"agent:invoke"}}
	// The default-deny requireSandboxScope middleware (task #12) is the
	// primary enforcement point that keeps a scoped key off /webhooks*
	// entirely — that's tested at the middleware layer, not here. This
	// test instead documents the handler's OWN independent authz gate
	// (task #50's fix): even reaching handleCreateWebhook directly (as if
	// middleware were misconfigured/bypassed), a request naming a
	// sandboxId the caller's Sub actually created is accepted, and body
	// with no sandboxId is rejected since this claims object isn't admin
	// — both checked below.
	scopedOwnBody := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"],"sandboxId":"sbx-1"}`
	req := webhookRequest(http.MethodPost, "/api/v1/webhooks", scopedOwnBody, scoped)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a sandboxId the claims.Sub actually owns, got %d body=%s", rec.Code, rec.Body.String())
	}

	unscopedBody := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"]}`
	req2 := webhookRequest(http.MethodPost, "/api/v1/webhooks", unscopedBody, scoped)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for an unscoped body from a non-admin claims object, got %d body=%s", rec2.Code, rec2.Body.String())
	}
}

// --- store-level unit tests ---

func TestValidateWebhookURL(t *testing.T) {
	cases := []struct {
		url     string
		wantErr bool
	}{
		{"https://8.8.8.8/hook", false},
		{"http://8.8.8.8/hook", true},
		{"https://127.0.0.1/hook", true},
		{"https://169.254.169.254/hook", true},
		{"https://10.1.2.3/hook", true},
		{"not-a-url", true},
		{"", true},
	}
	for _, c := range cases {
		err := validateWebhookURL(c.url)
		if c.wantErr && err == nil {
			t.Errorf("validateWebhookURL(%q): expected error, got nil", c.url)
		}
		if !c.wantErr && err != nil {
			t.Errorf("validateWebhookURL(%q): unexpected error %v", c.url, err)
		}
	}
}

func TestValidateWebhookEventTypes(t *testing.T) {
	if err := validateWebhookEventTypes(nil); err == nil {
		t.Error("expected error for nil event types")
	}
	if err := validateWebhookEventTypes([]string{}); err == nil {
		t.Error("expected error for empty event types")
	}
	if err := validateWebhookEventTypes([]string{"bogus"}); err == nil {
		t.Error("expected error for unknown event type")
	}
	if err := validateWebhookEventTypes([]string{"sandbox.running", "backup.created"}); err != nil {
		t.Errorf("expected no error for valid types, got %v", err)
	}
}

func TestGenerateWebhookSecret_NeverEmpty(t *testing.T) {
	for i := 0; i < 10; i++ {
		secret, err := generateWebhookSecret()
		if err != nil {
			t.Fatalf("generateWebhookSecret() error = %v", err)
		}
		if secret == "" {
			t.Fatal("generateWebhookSecret() returned empty secret")
		}
		if len(secret) < 32 {
			t.Errorf("secret %q looks too short for 32 bytes of base64url entropy", secret)
		}
	}
}

func TestWebhookStore_CreateListDelete(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := newWebhookStore(c, "agenttier")

	id, secret, err := store.Create(context.Background(), "u1", webhookRecord{
		URL:        "https://8.8.8.8/hook",
		EventTypes: []string{"sandbox.running"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if id == "" || secret == "" {
		t.Fatalf("id=%q secret=%q, want both non-empty", id, secret)
	}

	list, err := store.ListForUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("ListForUser() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("list = %+v, want 1 entry with id %s", list, id)
	}

	if err := store.Delete(context.Background(), id, "u1", false); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	listAfter, err := store.ListForUser(context.Background(), "u1")
	if err != nil {
		t.Fatalf("ListForUser() after delete error = %v", err)
	}
	if len(listAfter) != 0 {
		t.Fatalf("expected 0 entries after delete, got %d", len(listAfter))
	}
}

func TestWebhookStore_DeleteDeniesNonOwner(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	store := newWebhookStore(c, "agenttier")

	id, _, err := store.Create(context.Background(), "u1", webhookRecord{
		URL:        "https://8.8.8.8/hook",
		EventTypes: []string{"sandbox.running"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := store.Delete(context.Background(), id, "u2", false); err == nil {
		t.Fatal("expected access-denied error for non-owner delete")
	}
	// Confirm it's still there.
	list, _ := store.ListForUser(context.Background(), "u1")
	if len(list) != 1 {
		t.Fatalf("expected subscription to survive a denied delete attempt, got %d entries", len(list))
	}
}
