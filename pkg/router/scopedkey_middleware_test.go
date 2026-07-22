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
	"regexp"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
)

func scopedKeyClaims(sandboxID string, actionGroups ...string) *Claims {
	return &Claims{Sub: "sandbox-runtime", SandboxID: sandboxID, ActionGroups: actionGroups}
}

// scopedTestFixture builds a real Server (all routes registered via
// NewServer, exactly like production) plus a small standalone mux that
// mounts requireSandboxScope in front of a 200-OK dummy handler — mirroring
// how the route-wiring task (#16) will mount it on the real /api/v1
// subrouter, but avoiding a dependency on that task landing first.
func scopedTestFixture(t *testing.T) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, agenttierv1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return NewServer(&Config{ListenAddr: ":0", InstallNamespace: "agenttier", DevAuth: false}, c, nil)
}

// dummyOKHandler is what every route resolves to in the single-route test
// harnesses below — requireSandboxScope's decision (403 vs pass-through) is
// what's under test, not any particular handler's business logic.
func dummyOKHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// singleRouteRouter builds a fresh mux with exactly one route — method +
// path template, always mounted under an "/api/v1" prefix so
// route.GetPathTemplate() (what requireSandboxScope keys scopedKeyRouteActions
// lookups on) matches production exactly — wrapped in requireSandboxScope,
// then a dummy 200 handler. Used to test requireSandboxScope's decision for
// one (method, template) pair in isolation, the same way the real /api/v1
// subrouter will apply it once task #16 mounts it. template may be passed
// with or without the "/api/v1" prefix; it is normalized here.
func (s *Server) singleRouteRouter(method, template string) http.Handler {
	template = strings.TrimPrefix(template, "/api/v1")
	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()
	api.Use(s.requireSandboxScope)
	api.HandleFunc(template, dummyOKHandler).Methods(method)
	return r
}

var pathVarPattern = regexp.MustCompile(`\{[^}]+\}`)

// fillPathTemplate replaces every {var} / {var:regex} segment in a gorilla
// mux path template with a literal "x" so a concrete request URL can be
// built against it. "x" is also what tests use as the scoped key's
// SandboxID when they want the {id} comparison to succeed.
func fillPathTemplate(template string) string {
	return pathVarPattern.ReplaceAllString(template, "x")
}

// scopedRequest builds a request against path, always under the "/api/v1"
// prefix (added if not already present) to match singleRouteRouter's
// mounting — the actual URL the mux subrouter serves.
func scopedRequest(method, path string, claims *Claims) *http.Request {
	if !strings.HasPrefix(path, "/api/v1") {
		path = "/api/v1" + path
	}
	req := httptest.NewRequest(method, path, nil)
	return req.WithContext(context.WithValue(req.Context(), ClaimsContextKey, claims))
}

// --- unit tests for requireSandboxScope's decision logic ---

func TestRequireSandboxScope_NonScopedKeyPassesThrough(t *testing.T) {
	s := scopedTestFixture(t)
	r := s.singleRouteRouter(http.MethodGet, "/sandboxes/{id}")
	// No claims at all.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/x", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("unauthenticated request: code = %d, want 200 (middleware is a no-op with no claims)", rec.Code)
	}

	// User-level claims (empty SandboxID).
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, scopedRequest(http.MethodGet, "/sandboxes/x", &Claims{Sub: "u1"}))
	if rec2.Code != http.StatusOK {
		t.Errorf("user-level key: code = %d, want 200 (not a scoped key)", rec2.Code)
	}
}

func TestRequireSandboxScope_AllowsMatchingSandboxAndAction(t *testing.T) {
	s := scopedTestFixture(t)
	r := s.singleRouteRouter(http.MethodPost, "/sandboxes/{id}/exec")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, scopedRequest(http.MethodPost, "/sandboxes/x/exec", scopedKeyClaims("x", "run-command")))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (own sandbox + granted action)", rec.Code)
	}
}

// TestRequireSandboxScope_OutOfScopeSandbox404sBecomesA403 is checklist
// item #1: a scoped key bound to sandbox A, presented against
// /sandboxes/{B}/... (B != A), for every route in the allow-map.
func TestRequireSandboxScope_OutOfScopeSandbox(t *testing.T) {
	for tmpl, action := range scopedKeyRouteActions {
		method, path := splitMethodTemplate(t, tmpl)
		t.Run(tmpl, func(t *testing.T) {
			s := scopedTestFixture(t)
			r := s.singleRouteRouter(method, path)
			// Claims bound to "other-sandbox"; request path carries "x".
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, scopedRequest(method, fillPathTemplate(path), scopedKeyClaims("other-sandbox", action)))
			if rec.Code != http.StatusForbidden {
				t.Errorf("out-of-scope sandbox: code = %d, want 403", rec.Code)
			}
		})
	}
}

// TestRequireSandboxScope_ActionNotGranted confirms a scoped key whose
// bound sandbox matches but whose ActionGroups don't include the route's
// required action still 403s.
func TestRequireSandboxScope_ActionNotGranted(t *testing.T) {
	s := scopedTestFixture(t)
	r := s.singleRouteRouter(http.MethodPost, "/sandboxes/{id}/exec")
	rec := httptest.NewRecorder()
	// Own sandbox, but no action groups granted at all.
	r.ServeHTTP(rec, scopedRequest(http.MethodPost, "/sandboxes/x/exec", scopedKeyClaims("x")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403 (action not granted)", rec.Code)
	}
}

// TestRequireSandboxScope_BulkRejected is checklist item #2.
func TestRequireSandboxScope_BulkRejected(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodPost, "/sandboxes/bulk"},
		{http.MethodPost, "/sandboxes/bulk-action"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			s := scopedTestFixture(t)
			r := s.singleRouteRouter(c.method, c.path)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, scopedRequest(c.method, c.path, scopedKeyClaims("x", defaultScopedKeyActionGroups()...)))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s: code = %d, want 403 (bulk is never sandbox-scoped)", c.method, c.path, rec.Code)
			}
		})
	}
}

// TestRequireSandboxScope_WebhookRejected is checklist item #3.
func TestRequireSandboxScope_WebhookRejected(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodPost, "/webhooks"},
		{http.MethodGet, "/webhooks"},
		{http.MethodDelete, "/webhooks/{id}"},
		{http.MethodGet, "/webhooks/{id}/deliveries"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			s := scopedTestFixture(t)
			r := s.singleRouteRouter(c.method, c.path)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, scopedRequest(c.method, fillPathTemplate(c.path), scopedKeyClaims("x", defaultScopedKeyActionGroups()...)))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s: code = %d, want 403", c.method, c.path, rec.Code)
			}
		})
	}
}

// TestRequireSandboxScope_AdminAndGlobalRejected is checklist item #4.
func TestRequireSandboxScope_AdminAndGlobalRejected(t *testing.T) {
	cases := []struct{ method, path string }{
		{http.MethodGet, "/admin/sandboxes"},
		{http.MethodGet, "/admin/sharing"},
		{http.MethodGet, "/audit/events"},
		{http.MethodGet, "/analytics/usage"},
		{http.MethodGet, "/analytics/costs"},
		{http.MethodGet, "/governance/policies"},
		{http.MethodPut, "/governance/policies"},
		{http.MethodGet, "/governance/policies/{namespace}"},
		{http.MethodPut, "/governance/policies/{namespace}"},
		{http.MethodDelete, "/governance/policies/{namespace}"},
		{http.MethodGet, "/governance/effective"},
		{http.MethodGet, "/cluster/nodes"},
		{http.MethodPut, "/warmpool/config"},
		{http.MethodGet, "/user/me"},
		{http.MethodGet, "/user/preferences"},
		{http.MethodPut, "/user/preferences"},
		{http.MethodGet, "/user/api-keys"},
		{http.MethodPost, "/user/api-keys"},
		{http.MethodDelete, "/user/api-keys/{keyId}"},
		{http.MethodPost, "/templates"},
		{http.MethodPut, "/templates/{name}"},
		{http.MethodDelete, "/templates/{name}"},
		{http.MethodGet, "/sandboxes"},
		{http.MethodPost, "/sandboxes"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			s := scopedTestFixture(t)
			r := s.singleRouteRouter(c.method, c.path)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, scopedRequest(c.method, fillPathTemplate(c.path), scopedKeyClaims("x", defaultScopedKeyActionGroups()...)))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s: code = %d, want 403", c.method, c.path, rec.Code)
			}
		})
	}
}

// TestRequireSandboxScope_DeleteNotInVocabulary is checklist item #5(b) and
// #8: DELETE /sandboxes/{id} 403s a scoped key even on its own bound
// sandbox and even if a stored record somehow carried "delete" in
// ActionGroups (simulated here directly, bypassing mint-time validation) —
// because the route is absent from the allow-map entirely (default-deny),
// not because "delete" is checked-and-denied as an action.
func TestRequireSandboxScope_DeleteNotInVocabulary(t *testing.T) {
	s := scopedTestFixture(t)
	r := s.singleRouteRouter(http.MethodDelete, "/sandboxes/{id}")
	rec := httptest.NewRecorder()
	// Simulate a record that somehow carries "delete" — the route being
	// absent from the map must still 403 regardless.
	r.ServeHTTP(rec, scopedRequest(http.MethodDelete, "/sandboxes/x", scopedKeyClaims("x", "delete")))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403 (delete is never in the allow-map)", rec.Code)
	}
	if _, ok := scopedKeyRouteActions["DELETE /api/v1/sandboxes/{id}"]; ok {
		t.Fatal("scopedKeyRouteActions must never contain an entry for DELETE /sandboxes/{id}")
	}
	for _, action := range scopedKeyRouteActions {
		if action == "delete" {
			t.Fatalf("no allow-map entry may map to the \"delete\" action, found one mapping to %q", action)
		}
	}
}

// TestRequireSandboxScope_OwnSandboxStopResumeWork is checklist item #8's
// positive half: default action groups let a scoped key stop/resume its
// own bound sandbox.
func TestRequireSandboxScope_OwnSandboxStopResumeWork(t *testing.T) {
	for _, verb := range []string{"stop", "resume"} {
		t.Run(verb, func(t *testing.T) {
			s := scopedTestFixture(t)
			r := s.singleRouteRouter(http.MethodPost, "/sandboxes/{id}/"+verb)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, scopedRequest(http.MethodPost, "/sandboxes/x/"+verb, scopedKeyClaims("x", defaultScopedKeyActionGroups()...)))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: code = %d, want 200 (default action groups include %s)", verb, rec.Code, verb)
			}
		})
	}
}

// TestRequireSandboxScope_ExfiltratedKeyStillBounded is checklist item #7:
// the security boundary is the credential, not network origin. A scoped
// key presented from a test client with no sandbox-network context still
// only works within its own scope and still 403s out-of-scope calls.
func TestRequireSandboxScope_ExfiltratedKeyStillBounded(t *testing.T) {
	s := scopedTestFixture(t)
	r := s.singleRouteRouter(http.MethodPost, "/sandboxes/{id}/exec")
	claims := scopedKeyClaims("bound-sandbox", "run-command")

	// In-scope call succeeds regardless of "where" the request notionally
	// came from — there is no network-origin check in this middleware.
	okRec := httptest.NewRecorder()
	r.ServeHTTP(okRec, scopedRequest(http.MethodPost, "/sandboxes/bound-sandbox/exec", claims))
	if okRec.Code != http.StatusOK {
		t.Fatalf("in-scope call: code = %d, want 200", okRec.Code)
	}

	// Out-of-scope call with the identical credential still 403s.
	r2 := s.singleRouteRouter(http.MethodPost, "/sandboxes/{id}/exec")
	badRec := httptest.NewRecorder()
	r2.ServeHTTP(badRec, scopedRequest(http.MethodPost, "/sandboxes/other-sandbox/exec", claims))
	if badRec.Code != http.StatusForbidden {
		t.Fatalf("out-of-scope call: code = %d, want 403", badRec.Code)
	}
}

// splitMethodTemplate parses a "METHOD /path/template" allow-map key back
// into its parts.
func splitMethodTemplate(t *testing.T, key string) (method, template string) {
	t.Helper()
	for i := 0; i < len(key); i++ {
		if key[i] == ' ' {
			return key[:i], key[i+1:]
		}
	}
	t.Fatalf("malformed allow-map key %q", key)
	return "", ""
}

// --- sa-review.md checklist item #6: the regression guard ---
//
// Walk the ACTUAL routes registered by NewServer (production route
// registration, not a hand-copied list) and assert every route under
// /api/v1 is either present in scopedKeyRouteActions (with a specific
// action) or verified to 403 a scoped key via default-deny. This must fail
// loudly if a future route is added to server.go without an explicit
// allow-map decision.
func TestRequireSandboxScope_EveryRegisteredRouteHasADecision(t *testing.T) {
	s := scopedTestFixture(t)

	type routeKey struct{ method, template string }
	var seen []routeKey

	err := s.router.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		tmpl, err := route.GetPathTemplate()
		if err != nil {
			return nil // routes with no path template (rare) aren't relevant here
		}
		// Scope to /api/v1 — requireSandboxScope is only ever mounted on
		// that subrouter (per its own doc comment); /healthz, /readyz,
		// /metrics, and /ws/terminal/{sandboxId} live outside it and are
		// unauthenticated or have their own bespoke auth, so they're not
		// this middleware's concern.
		if len(tmpl) < 8 || tmpl[:8] != "/api/v1/" {
			return nil
		}
		methods, err := route.GetMethods()
		if err != nil || len(methods) == 0 {
			return nil
		}
		for _, m := range methods {
			seen = append(seen, routeKey{method: m, template: tmpl})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking router: %v", err)
	}
	if len(seen) == 0 {
		t.Fatal("router.Walk found zero /api/v1 routes — the walk predicate is broken")
	}

	for _, rk := range seen {
		t.Run(rk.method+" "+rk.template, func(t *testing.T) {
			relTemplate := rk.template[len("/api/v1"):] // singleRouteRouter registers relative to "/"
			action, inMap := scopedKeyRouteActions[rk.method+" "+rk.template]

			fresh := scopedTestFixture(t)
			testRouter := fresh.singleRouteRouter(rk.method, relTemplate)
			path := fillPathTemplate(relTemplate)

			if inMap {
				// (a) In the allow-map with a specific action: a
				// well-formed scoped key (matching sandbox + granted
				// action) must be admitted.
				rec := httptest.NewRecorder()
				testRouter.ServeHTTP(rec, scopedRequest(rk.method, path, scopedKeyClaims("x", action)))
				if rec.Code == http.StatusForbidden {
					t.Errorf("route is in the allow-map with action %q but a well-formed scoped key was still 403'd", action)
				}
			} else {
				// (b) Not in the allow-map: default-deny must 403
				// unconditionally, regardless of what the key claims.
				rec := httptest.NewRecorder()
				testRouter.ServeHTTP(rec, scopedRequest(rk.method, path, scopedKeyClaims("x", defaultScopedKeyActionGroups()...)))
				if rec.Code != http.StatusForbidden {
					t.Errorf("route %s %s is NOT in scopedKeyRouteActions but a scoped key got %d, want 403 (default-deny regression — add an explicit allow-map decision for this route)", rk.method, rk.template, rec.Code)
				}
			}
		})
	}

	// Explicit assertion that DELETE /sandboxes/{id} was actually walked
	// and confirmed default-deny — the specific case sa-review.md calls
	// out by name (checklist #5b), not just implied by the loop above.
	foundDeleteSandbox := false
	for _, rk := range seen {
		if rk.method == http.MethodDelete && rk.template == "/api/v1/sandboxes/{id}" {
			foundDeleteSandbox = true
		}
	}
	if !foundDeleteSandbox {
		t.Fatal("expected DELETE /api/v1/sandboxes/{id} to be among the walked routes — route table changed shape unexpectedly")
	}
}
