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
	"sort"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
	"github.com/agenttier/agenttier/pkg/apikeystore"
	"github.com/agenttier/agenttier/pkg/router/auth"
)

// scopedKeyFixture builds a Server (with a real fake k8s client, so
// getSandboxWithAuthCheck resolves) plus a standalone mux wired directly to
// the api-key handlers, bypassing authMiddleware — same pattern as
// patchFixture/backupFixture, needed here because authMiddleware only
// accepts context-injected claims in --dev-auth mode (which forces a fixed
// admin identity, useless for exercising per-user ownership checks).
func scopedKeyFixture(t *testing.T, objs ...client.Object) (*Server, client.Client, http.Handler) {
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
	s := NewServer(&Config{ListenAddr: ":0", InstallNamespace: "agenttier"}, c, nil)

	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()
	api.HandleFunc("/user/api-keys", s.handleListAPIKeys).Methods("GET")
	api.HandleFunc("/user/api-keys", s.handleCreateAPIKey).Methods("POST")
	api.HandleFunc("/user/api-keys/{keyId}", s.handleRevokeAPIKey).Methods("DELETE")

	return s, c, r
}

// sandboxOwnedBy returns a minimal Sandbox object owned by sub, in the
// default namespace, so getSandboxWithAuthCheck's ownership check passes for
// that identity.
func sandboxOwnedBy(name, sub string) *agenttierv1alpha1.Sandbox {
	return &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: agenttierv1alpha1.SandboxSpec{
			Mode:        agenttierv1alpha1.SandboxModeCode,
			TemplateRef: &agenttierv1alpha1.TemplateReference{Name: "general-coding", Kind: "ClusterSandboxTemplate"},
			CreatedBy:   &agenttierv1alpha1.UserIdentity{Sub: sub},
		},
		Status: agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
}

// mintRequest builds an authenticated POST /api/v1/user/api-keys request
// carrying claims for sub (non-admin unless admin=true).
func mintRequest(sub, body string, admin bool) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/user/api-keys", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = int64(len(body))
	claims := &Claims{Sub: sub, Email: sub + "@agenttier.local", Name: sub, IsAdmin: admin}
	return req.WithContext(context.WithValue(req.Context(), ClaimsContextKey, claims))
}

type mintResponse struct {
	ID           string   `json:"id"`
	Key          string   `json:"key"`
	Name         string   `json:"name"`
	SandboxID    string   `json:"sandboxId"`
	ActionGroups []string `json:"actionGroups"`
}

func TestCreateAPIKey_ScopedMint_PersistsSandboxIDAndActionGroups(t *testing.T) {
	_, c, r := scopedKeyFixture(t, sandboxOwnedBy("sbx-1", "u-owner"))

	body := `{"name":"agent-key","sandboxId":"sbx-1","scopes":["run-command","files:read"]}`
	req := mintRequest("u-owner", body, false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp mintResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid response: %v", err)
	}
	if resp.SandboxID != "sbx-1" {
		t.Errorf("response sandboxId = %q, want sbx-1", resp.SandboxID)
	}
	sort.Strings(resp.ActionGroups)
	want := []string{"files:read", "run-command"}
	if len(resp.ActionGroups) != len(want) || resp.ActionGroups[0] != want[0] || resp.ActionGroups[1] != want[1] {
		t.Errorf("response actionGroups = %v, want %v", resp.ActionGroups, want)
	}

	// Persisted record must carry the same fields — not just the response.
	store := newSecretAPIKeyStore(c, "agenttier")
	rec2, err := store.GetAPIKeyByHash(context.Background(), auth.HashAPIKey(resp.Key))
	if err != nil {
		t.Fatalf("stored record not found: %v", err)
	}
	if rec2.SandboxID != "sbx-1" {
		t.Errorf("stored SandboxID = %q, want sbx-1", rec2.SandboxID)
	}
	sort.Strings(rec2.ActionGroups)
	if len(rec2.ActionGroups) != 2 || rec2.ActionGroups[0] != "files:read" || rec2.ActionGroups[1] != "run-command" {
		t.Errorf("stored ActionGroups = %v, want [files:read run-command]", rec2.ActionGroups)
	}
}

func TestCreateAPIKey_ScopedMint_DefaultActionGroupsWhenScopesOmitted(t *testing.T) {
	_, _, r := scopedKeyFixture(t, sandboxOwnedBy("sbx-2", "u-owner"))

	body := `{"sandboxId":"sbx-2"}`
	req := mintRequest("u-owner", body, false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp mintResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	hasResume, hasStop, hasDelete := false, false, false
	for _, g := range resp.ActionGroups {
		switch g {
		case "resume":
			hasResume = true
		case "stop":
			hasStop = true
		case "delete":
			hasDelete = true
		}
	}
	if !hasResume || !hasStop {
		t.Errorf("default action groups = %v, want resume+stop present (FR6.1.1 self-lockout fix)", resp.ActionGroups)
	}
	if hasDelete {
		t.Errorf("default action groups = %v, must never include delete (DD3)", resp.ActionGroups)
	}
}

func TestCreateAPIKey_ScopedMint_RejectsDeleteActionGroup(t *testing.T) {
	_, _, r := scopedKeyFixture(t, sandboxOwnedBy("sbx-3", "u-owner"))

	body := `{"sandboxId":"sbx-3","scopes":["run-command","delete"]}`
	req := mintRequest("u-owner", body, false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for delete in scopes, got %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"key"`) {
		t.Error("a rejected mint request must not have generated/returned a key")
	}
}

func TestCreateAPIKey_ScopedMint_RejectsUnknownActionGroup(t *testing.T) {
	_, _, r := scopedKeyFixture(t, sandboxOwnedBy("sbx-4", "u-owner"))

	body := `{"sandboxId":"sbx-4","scopes":["not-a-real-scope"]}`
	req := mintRequest("u-owner", body, false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown scope, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateAPIKey_ScopedMint_RequiresOwnershipOfSandbox(t *testing.T) {
	_, _, r := scopedKeyFixture(t, sandboxOwnedBy("sbx-5", "u-owner"))

	// A different, non-admin user tries to mint a key scoped to someone
	// else's sandbox.
	body := `{"sandboxId":"sbx-5"}`
	req := mintRequest("u-attacker", body, false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code == http.StatusCreated {
		t.Fatalf("expected mint to be rejected for a non-owned sandbox, got 201 body=%s", rec.Body.String())
	}
}

func TestCreateAPIKey_ScopesWithoutSandboxIDRejected(t *testing.T) {
	_, _, r := scopedKeyFixture(t)

	body := `{"scopes":["run-command"]}`
	req := mintRequest("u-owner", body, false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for scopes without sandboxId, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateAPIKey_UserLevelMintUnaffectedWhenSandboxIDAbsent(t *testing.T) {
	_, c, r := scopedKeyFixture(t)

	body := `{"name":"plain-user-key"}`
	req := mintRequest("u-owner", body, false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp mintResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.SandboxID != "" {
		t.Errorf("user-level key must not carry a sandboxId, got %q", resp.SandboxID)
	}
	if len(resp.ActionGroups) != 0 {
		t.Errorf("user-level key must not carry actionGroups, got %v", resp.ActionGroups)
	}

	store := newSecretAPIKeyStore(c, "agenttier")
	rec2, err := store.GetAPIKeyByHash(context.Background(), auth.HashAPIKey(resp.Key))
	if err != nil {
		t.Fatalf("stored record not found: %v", err)
	}
	if rec2.SandboxID != "" || len(rec2.ActionGroups) != 0 {
		t.Errorf("stored user-level record carries scope fields: sandboxId=%q actionGroups=%v", rec2.SandboxID, rec2.ActionGroups)
	}
}

// TestCreateAPIKey_RejectsUnknownField_ActionGroupsTypo recreates the exact
// footgun class found during task #44's doc-fix (a caller sending
// "actionGroups" instead of the real "scopes" field name). Before this fix,
// json.Decode silently ignored the unknown field, left req.Scopes nil, and
// fell through to defaultScopedKeyActionGroups() — minting a key with the
// FULL default 8-group set instead of the caller's intended narrow scope. A
// wrong field name must 400, not silently mint a broader-than-intended key.
func TestCreateAPIKey_RejectsUnknownField_ActionGroupsTypo(t *testing.T) {
	_, c, r := scopedKeyFixture(t, sandboxOwnedBy("sbx-7", "u-owner"))

	body := `{"sandboxId":"sbx-7","actionGroups":["run-command"]}`
	req := mintRequest("u-owner", body, false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown field %q, got %d body=%s", "actionGroups", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"key"`) {
		t.Error("a rejected mint request must not have generated/returned a key")
	}

	// Belt-and-suspenders: confirm no broader-than-intended key was persisted
	// either (the 400 must have short-circuited before createAPIKeySecret).
	list := &corev1.SecretList{}
	if err := c.List(context.Background(), list); err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	for _, s := range list.Items {
		if s.Labels[apikeystore.SecretPurposeLabel] == apikeystore.SecretPurpose {
			t.Errorf("no api-key secret should have been created for the rejected request, found %s", s.Name)
		}
	}
}

// TestCreateAPIKey_RejectsUnknownField_GenericTypo confirms the guard isn't
// specific to "actionGroups" — any field outside the known set is rejected.
func TestCreateAPIKey_RejectsUnknownField_GenericTypo(t *testing.T) {
	_, _, r := scopedKeyFixture(t)

	body := `{"name":"ci","nmae":"typo"}` //nolint:misspell // intentional typo — the whole point of this test
	req := mintRequest("u-owner", body, false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown field, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCreateAPIKey_ValidScopedMintUnaffectedByUnknownFieldGuard confirms the
// DisallowUnknownFields guard doesn't collaterally break a correctly-shaped
// scoped-mint request (the "scopes" field name, spelled correctly).
func TestCreateAPIKey_ValidScopedMintUnaffectedByUnknownFieldGuard(t *testing.T) {
	_, _, r := scopedKeyFixture(t, sandboxOwnedBy("sbx-8", "u-owner"))

	body := `{"sandboxId":"sbx-8","scopes":["run-command"]}`
	req := mintRequest("u-owner", body, false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a correctly-shaped scoped mint, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestCreateAPIKey_ValidUserLevelMintUnaffectedByUnknownFieldGuard confirms a
// user-level mint (no sandboxId/scopes at all) still succeeds.
func TestCreateAPIKey_ValidUserLevelMintUnaffectedByUnknownFieldGuard(t *testing.T) {
	_, _, r := scopedKeyFixture(t)

	body := `{"name":"ci","expiresIn":"720h"}`
	req := mintRequest("u-owner", body, false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201 for a correctly-shaped user-level mint, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListAPIKeys_ShowsScopeMetadataNotSecret(t *testing.T) {
	_, _, r := scopedKeyFixture(t, sandboxOwnedBy("sbx-6", "u-owner"))

	body := `{"name":"scoped","sandboxId":"sbx-6","scopes":["ports"]}`
	req := mintRequest("u-owner", body, false)
	r.ServeHTTP(httptest.NewRecorder(), req)

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/user/api-keys", nil)
	listReq = listReq.WithContext(context.WithValue(listReq.Context(), ClaimsContextKey, &Claims{Sub: "u-owner"}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, listReq)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"sandboxId":"sbx-6"`) {
		t.Errorf("list response missing sandboxId metadata: %s", out)
	}
	if !strings.Contains(out, "ports") {
		t.Errorf("list response missing actionGroups metadata: %s", out)
	}
	if strings.Contains(out, "atk_") || strings.Contains(out, "keyHash") {
		t.Errorf("list leaked secret material: %s", out)
	}
}
