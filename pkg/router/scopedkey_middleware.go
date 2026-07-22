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
	"net/http"

	"github.com/gorilla/mux"
)

// FR6 sandbox-scoped API key enforcement (design.md#FR6, DD3, and the DL7
// amendments from sa-review.md's threat model). A scoped key is any request
// whose Claims carry a non-empty SandboxID (see middleware.go's Claims
// doc comment) — set only when the caller authenticated with a sandbox-
// scoped API key rather than a JWT or user-level key.
//
// **Default-deny (DL7 Critical finding #2 — hard requirement).** The map
// below is the ONLY path to a 200 for a scoped key. Any route not present
// in scopedKeyRouteActions 403s unconditionally, regardless of method or
// path shape. This is deliberate: a future route added to server.go without
// an explicit entry here must fail closed, not fall through to "allowed."
// scopedkey_middleware_test.go's TestRequireSandboxScope_EveryRegisteredRouteHasADecision
// is the regression guard — it walks the actual mux route table and asserts
// every route is either in this map or verified to 403 a scoped key.
//
// **`delete` is not a member of any value in this map, ever** (DD3 / FR6.1.1
// / sa-review.md High finding). There is no entry for `DELETE
// /sandboxes/{id}` at all — a scoped key hitting it 403s via default-deny
// (route not in map), not via an "action present but not granted" check.
// This is intentional belt-and-suspenders: even if a stored record somehow
// carried "delete" in ActionGroups (a mint-time validation bug, a hand-
// edited Secret), there is no route this middleware would let it reach.
//
// The action groups below are the FR6.1 vocabulary: run-command,
// files:read, files:write, ports, agent:invoke, agent:configure, resume,
// stop. Every sandbox-`{id}` route that has no natural home in this
// vocabulary (GET /sandboxes/{id} status, PATCH, clone, sharing, backups)
// is deliberately absent — a scoped key represents an agent operating
// inside its own sandbox (run code, manage files/ports, invoke/configure
// itself, resume/stop itself), not a delegate for the owner's full
// sandbox-management surface. That broader surface still requires the
// owner's user-level key.
var scopedKeyRouteActions = map[string]string{
	"POST /api/v1/sandboxes/{id}/exec": "run-command",

	"GET /api/v1/sandboxes/{id}/files/":          "files:read",
	"GET /api/v1/sandboxes/{id}/files/{path:.*}": "files:read",
	"PUT /api/v1/sandboxes/{id}/files/{path:.*}": "files:write",
	"GET /api/v1/sandboxes/{id}/archive":         "files:read",

	"GET /api/v1/sandboxes/{id}/ports":           "ports",
	"POST /api/v1/sandboxes/{id}/ports":          "ports",
	"DELETE /api/v1/sandboxes/{id}/ports/{port}": "ports",

	"POST /api/v1/sandboxes/{id}/configure":            "agent:configure",
	"GET /api/v1/sandboxes/{id}/configure/install-log": "agent:configure",
	"POST /api/v1/sandboxes/{id}/invoke":               "agent:invoke",
	"POST /api/v1/sandboxes/{id}/invoke/cancel":        "agent:invoke",

	"POST /api/v1/sandboxes/{id}/resume": "resume",
	"POST /api/v1/sandboxes/{id}/stop":   "stop",
}

// requireSandboxScope is mounted on the `/api/v1` subrouter (after
// authMiddleware, so Claims are available) by the route-wiring task
// (#16/Group 2b). It is a no-op for any request that did not authenticate
// with a sandbox-scoped key — user-level keys and JWTs leave
// Claims.SandboxID empty and pass straight through to the handler's own
// RBAC (getSandboxWithAuthCheck etc.), unaffected.
//
// For a scoped key, the check is, in order:
//  1. Is `<method> <path-template>` in scopedKeyRouteActions? If not, 403
//     (default-deny — see the package doc above).
//  2. Does the request's `{id}` path variable equal Claims.SandboxID? If
//     not, 403 — a mismatch, not a 404, per design.md#FR6 ("a mismatch is
//     a 403, not a 404 (avoid leaking existence)"). Routes with no `{id}`
//     variable can never appear in the map (every entry above has one), so
//     this check always applies to a matched route.
//  3. Is the route's required action present in Claims.ActionGroups? If
//     not, 403.
//
// All three checks pass -> the request proceeds to the handler, which
// still runs its own ownership check (getSandboxWithAuthCheck) — this
// middleware narrows access further, it never widens it.
func (s *Server) requireSandboxScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := GetClaims(r.Context())
		if claims == nil || claims.SandboxID == "" {
			next.ServeHTTP(w, r)
			return
		}

		tmpl := routePathTemplate(r)
		if tmpl == "" {
			respondError(w, http.StatusForbidden, "scoped key not permitted for this route")
			return
		}

		action, ok := scopedKeyRouteActions[r.Method+" "+tmpl]
		if !ok {
			respondError(w, http.StatusForbidden, "scoped key not permitted for this route")
			return
		}

		if mux.Vars(r)["id"] != claims.SandboxID {
			respondError(w, http.StatusForbidden, "scoped key is not bound to this sandbox")
			return
		}

		if !stringSliceContains(claims.ActionGroups, action) {
			respondError(w, http.StatusForbidden, "scoped key lacks the \""+action+"\" action group")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// stringSliceContains reports whether target is present in list.
func stringSliceContains(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}
