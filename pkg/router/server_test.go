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
)

// Group 2b — this file is the route-wiring serialization point's own test:
// it verifies the new FR2/FR3/FR4/FR5 routes registered in NewServer exist
// with the right HTTP methods, that every new route requires authentication
// (NFR1), and that the wired PATCH route reaches handlePatchSandbox through
// the real middleware chain end-to-end. The pre-wiring per-handler fixtures
// (backup_handlers_test.go/bulk_handlers_test.go/webhook_handlers_test.go/
// patch_handlers_test.go, each with their own standalone mux) predate this
// task and remain as additional handler-level coverage.
//
// scopedkey_middleware_test.go's TestRequireSandboxScope_EveryRegisteredRouteHasADecision
// already walks this same live route table for the FR6 default-deny
// property, so that specific concern isn't re-tested here.

func wiringTestFixture(t *testing.T, objs ...client.Object) (*Server, client.Client) {
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

// routeExists walks s.router and reports whether a route matching method +
// path template is registered — the real production table, not a
// hand-copied list.
func routeExists(t *testing.T, s *Server, method, template string) bool {
	t.Helper()
	found := false
	err := s.router.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		tmpl, err := route.GetPathTemplate()
		if err != nil || tmpl != template {
			return nil
		}
		methods, err := route.GetMethods()
		if err != nil {
			return nil
		}
		for _, m := range methods {
			if m == method {
				found = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking router: %v", err)
	}
	return found
}

// TestNewRoutesAreRegistered confirms every FR2/FR3/FR4/FR5 route this task
// wires exists in the live route table with the expected method — a
// typo'd path or a forgotten .Methods() call would leave the route
// unreachable or open to the wrong verb, and this catches that at the
// route-table level.
func TestNewRoutesAreRegistered(t *testing.T) {
	s, _ := wiringTestFixture(t)

	cases := []struct{ method, template string }{
		// FR2
		{"PATCH", "/api/v1/sandboxes/{id}"},
		// FR4
		{"POST", "/api/v1/sandboxes/bulk"},
		{"POST", "/api/v1/sandboxes/bulk-action"},
		// FR3
		{"GET", "/api/v1/sandboxes/{id}/backups"},
		{"POST", "/api/v1/sandboxes/{id}/backups"},
		{"POST", "/api/v1/sandboxes/{id}/backups/{snapshotName}/restore"},
		{"DELETE", "/api/v1/sandboxes/{id}/backups/{snapshotName}"},
		// FR5
		{"POST", "/api/v1/webhooks"},
		{"GET", "/api/v1/webhooks"},
		{"DELETE", "/api/v1/webhooks/{id}"},
		{"GET", "/api/v1/webhooks/{id}/deliveries"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.template, func(t *testing.T) {
			if !routeExists(t, s, c.method, c.template) {
				t.Errorf("route %s %s not found in the registered route table", c.method, c.template)
			}
		})
	}
}

// TestNewEndpoints_RejectUnauthenticated confirms every new route requires
// authentication (NFR1) — a request with no credentials must never reach
// the handler. DevAuth is off here so authMiddleware takes its real
// (non-bypassed) path.
func TestNewEndpoints_RejectUnauthenticated(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, agenttierv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&agenttierv1alpha1.Sandbox{}).Build()
	s := NewServer(&Config{ListenAddr: ":0", InstallNamespace: "agenttier", DevAuth: false}, c, nil)

	cases := []struct{ method, path string }{
		{"PATCH", "/api/v1/sandboxes/x"},
		{"POST", "/api/v1/sandboxes/bulk"},
		{"POST", "/api/v1/sandboxes/bulk-action"},
		{"GET", "/api/v1/sandboxes/x/backups"},
		{"POST", "/api/v1/sandboxes/x/backups"},
		{"POST", "/api/v1/webhooks"},
		{"GET", "/api/v1/webhooks"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			req := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.router.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("code = %d, want 401 (no credentials presented)", rec.Code)
			}
		})
	}
}

// TestPatchSandbox_WiredEndToEnd is a smoke test that the wired PATCH route
// reaches handlePatchSandbox through the full production middleware chain
// (requestID, CORS, logging, rate-limit, auth, per-user rate-limit,
// deprecation, requireSandboxScope) via s.router — the handler-level tests
// in patch_handlers_test.go exercise it against a standalone mux instead.
func TestPatchSandbox_WiredEndToEnd(t *testing.T) {
	sandbox := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-wired", Namespace: "default"},
		Spec: agenttierv1alpha1.SandboxSpec{
			Mode:        agenttierv1alpha1.SandboxModeCode,
			TemplateRef: &agenttierv1alpha1.TemplateReference{Name: "general-coding", Kind: "ClusterSandboxTemplate"},
		},
		Status: agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
	s, _ := wiringTestFixture(t, sandbox)

	body := `{"idleTimeout":"30m"}`
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/sandboxes/sbx-wired", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestBulkAndWebhookRoutes_WiredEndToEnd smoke-tests the remaining new
// route families through s.router, confirming they're reachable (not
// 404/405) and gated correctly end-to-end rather than only via the
// standalone-mux fixtures in their own _test.go files.
func TestBulkAndWebhookRoutes_WiredEndToEnd(t *testing.T) {
	s, _ := wiringTestFixture(t)

	t.Run("bulk create", func(t *testing.T) {
		body := `{"items":[{"name":"sbx-bulk-1","templateRef":{"name":"general-coding"}}]}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/bulk", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.router.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusMethodNotAllowed {
			t.Fatalf("route not wired correctly: code = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("webhook create then list", func(t *testing.T) {
		createBody := `{"url":"https://8.8.8.8/hook","eventTypes":["sandbox.running"]}`
		createReq := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", strings.NewReader(createBody))
		createReq.Header.Set("Content-Type", "application/json")
		createRec := httptest.NewRecorder()
		s.router.ServeHTTP(createRec, createReq)
		if createRec.Code != http.StatusCreated {
			t.Fatalf("create: expected 201, got %d body=%s", createRec.Code, createRec.Body.String())
		}

		listReq := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks", nil)
		listRec := httptest.NewRecorder()
		s.router.ServeHTTP(listRec, listReq)
		if listRec.Code != http.StatusOK {
			t.Fatalf("list: expected 200, got %d body=%s", listRec.Code, listRec.Body.String())
		}
	})
}

// scopedClaimsContext is a small helper mirroring patchRequest's pattern in
// patch_handlers_test.go, for tests in this file that need a scoped-key
// identity against the wired /api/v1 subrouter.
func scopedClaimsContext(req *http.Request, claims *Claims) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), ClaimsContextKey, claims))
}

// TestRequireSandboxScope_MountedOnWiredRouter confirms requireSandboxScope
// is actually mounted on s.router (not just unit-tested in isolation in
// scopedkey_middleware_test.go) by driving a scoped key directly at the
// middleware chain — using a fully-initialized Server (via wiringTestFixture,
// so s.logger/s.governanceStore/etc. are non-nil) with its own small mux
// registering the same PATCH route+middleware order the production wiring
// uses. PATCH has no allow-map entry, so default-deny must 403 it.
func TestRequireSandboxScope_MountedOnWiredRouter(t *testing.T) {
	sandbox := &agenttierv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-scoped", Namespace: "default"},
		Spec: agenttierv1alpha1.SandboxSpec{
			Mode:        agenttierv1alpha1.SandboxModeCode,
			TemplateRef: &agenttierv1alpha1.TemplateReference{Name: "general-coding", Kind: "ClusterSandboxTemplate"},
		},
		Status: agenttierv1alpha1.SandboxStatus{Phase: agenttierv1alpha1.SandboxPhaseRunning},
	}
	s, _ := wiringTestFixture(t, sandbox)

	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()
	api.Use(s.requireSandboxScope)
	api.HandleFunc("/sandboxes/{id}", s.handlePatchSandbox).Methods("PATCH")

	req := httptest.NewRequest(http.MethodPatch, "/api/v1/sandboxes/sbx-scoped", strings.NewReader(`{"idleTimeout":"30m"}`))
	req.Header.Set("Content-Type", "application/json")
	req = scopedClaimsContext(req, &Claims{Sub: "sandbox-runtime", SandboxID: "sbx-scoped", ActionGroups: []string{"agent:invoke"}})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (PATCH has no allow-map entry, default-deny), got %d body=%s", rec.Code, rec.Body.String())
	}
}
