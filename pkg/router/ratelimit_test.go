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
	"testing"
	"time"

	"github.com/gorilla/mux"
)

func TestRateLimiter_PerIP_AllowsBurstThenThrottles(t *testing.T) {
	rl := newRateLimiter(RateLimitConfig{
		PerIPRate:  1.0, // 1/sec steady state
		PerIPBurst: 3,
	})
	defer rl.stopCleanup()

	const ip = "10.0.0.1"

	// Burst of 3 should all pass.
	for i := 0; i < 3; i++ {
		ok, _ := rl.gateRequest(ip, "")
		if !ok {
			t.Fatalf("burst request %d unexpectedly throttled", i)
		}
	}

	// 4th request inside the same instant must hit the cap.
	ok, retryAfter := rl.gateRequest(ip, "")
	if ok {
		t.Fatal("expected 4th request to be throttled, got allowed")
	}
	if retryAfter <= 0 {
		t.Errorf("expected retryAfter > 0 on throttled request, got %v", retryAfter)
	}
}

func TestRateLimiter_PerIP_DifferentIPsAreIndependent(t *testing.T) {
	rl := newRateLimiter(RateLimitConfig{
		PerIPRate:  1.0,
		PerIPBurst: 1,
	})
	defer rl.stopCleanup()

	// First IP exhausts its burst.
	if ok, _ := rl.gateRequest("10.0.0.1", ""); !ok {
		t.Fatal("first IP first request rejected")
	}
	if ok, _ := rl.gateRequest("10.0.0.1", ""); ok {
		t.Fatal("first IP second request unexpectedly allowed (burst=1)")
	}

	// Second IP has its own bucket and must not inherit the first's
	// exhaustion. This is the test that fails if we accidentally key by
	// something other than IP.
	if ok, _ := rl.gateRequest("10.0.0.2", ""); !ok {
		t.Fatal("second IP rejected — buckets are sharing state across IPs")
	}
}

func TestRateLimiter_PerUser_BypassedWhenAnonymous(t *testing.T) {
	rl := newRateLimiter(RateLimitConfig{
		PerUserRate:  1.0,
		PerUserBurst: 1,
	})
	defer rl.stopCleanup()

	// Anonymous request — no Sub claim. The per-user layer must not
	// engage at all.
	for i := 0; i < 5; i++ {
		ok, _ := rl.gateRequest("10.0.0.1", "")
		if !ok {
			t.Errorf("anonymous request %d unexpectedly throttled by per-user limiter", i)
		}
	}
}

func TestRateLimiter_DisabledByDefault(t *testing.T) {
	// Zero config = both rates are 0. gateRequest must always allow,
	// matching today's behavior so an operator who doesn't opt in sees
	// no change.
	rl := newRateLimiter(RateLimitConfig{})
	defer rl.stopCleanup()

	for i := 0; i < 1000; i++ {
		ok, _ := rl.gateRequest("10.0.0.1", "user-1")
		if !ok {
			t.Fatalf("request %d throttled despite zero config", i)
		}
	}
}

func TestRateLimitMiddleware_ExemptHealthEndpoints(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz", "/metrics", "/ws/terminal/sb-1"} {
		if !isExemptFromRateLimit(path) {
			t.Errorf("expected %q to be exempt from rate limiting", path)
		}
	}
	for _, path := range []string{"/api/v1/sandboxes", "/api/v1/sandboxes/sb-1/exec"} {
		if isExemptFromRateLimit(path) {
			t.Errorf("expected %q to NOT be exempt from rate limiting", path)
		}
	}
}

func TestClientIP_HonorsXForwardedFor(t *testing.T) {
	cases := []struct {
		name  string
		req   *http.Request
		trust bool
		want  string
	}{
		{
			name:  "no headers — falls back to RemoteAddr",
			req:   &http.Request{RemoteAddr: "192.0.2.10:55432"},
			trust: true,
			want:  "192.0.2.10",
		},
		{
			name: "trusted: X-Forwarded-For with one entry",
			req: func() *http.Request {
				r := httptest.NewRequest("GET", "/", nil)
				r.Header.Set("X-Forwarded-For", "203.0.113.1")
				return r
			}(),
			trust: true,
			want:  "203.0.113.1",
		},
		{
			name: "trusted: X-Forwarded-For with chained proxies — first wins",
			req: func() *http.Request {
				r := httptest.NewRequest("GET", "/", nil)
				r.Header.Set("X-Forwarded-For", "203.0.113.1, 10.0.0.5, 10.0.0.6")
				return r
			}(),
			trust: true,
			want:  "203.0.113.1",
		},
		{
			name: "trusted: X-Real-IP fallback",
			req: func() *http.Request {
				r := httptest.NewRequest("GET", "/", nil)
				r.Header.Set("X-Real-IP", "203.0.113.99")
				return r
			}(),
			trust: true,
			want:  "203.0.113.99",
		},
		{
			// SECURITY regression guard: with trust off (the default), a
			// spoofed X-Forwarded-For must be IGNORED so it can't mint a
			// fresh rate-limit bucket per request. The key is the real peer.
			name: "untrusted: spoofed X-Forwarded-For is ignored",
			req: func() *http.Request {
				r := httptest.NewRequest("GET", "/", nil)
				r.RemoteAddr = "192.0.2.50:40000"
				r.Header.Set("X-Forwarded-For", "1.2.3.4")
				return r
			}(),
			trust: false,
			want:  "192.0.2.50",
		},
		{
			name: "untrusted: spoofed X-Real-IP is ignored",
			req: func() *http.Request {
				r := httptest.NewRequest("GET", "/", nil)
				r.RemoteAddr = "192.0.2.51:40001"
				r.Header.Set("X-Real-IP", "5.6.7.8")
				return r
			}(),
			trust: false,
			want:  "192.0.2.51",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clientIP(tc.req, tc.trust); got != tc.want {
				t.Errorf("clientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRateLimiter_SteadyStateRefillAllowsContinuedRequests(t *testing.T) {
	// Burst of 1, rate of 100/sec → after 10ms one new token should be
	// available. Using fast-tick mode (rate=100) keeps the test under a
	// second while still exercising the refill path.
	rl := newRateLimiter(RateLimitConfig{
		PerIPRate:  100.0,
		PerIPBurst: 1,
	})
	defer rl.stopCleanup()

	const ip = "10.0.0.42"

	// First request consumes the burst.
	if ok, _ := rl.gateRequest(ip, ""); !ok {
		t.Fatal("first request unexpectedly throttled")
	}
	// Immediate second request must fail (no refill yet).
	if ok, _ := rl.gateRequest(ip, ""); ok {
		t.Fatal("immediate second request unexpectedly allowed (burst=1, no refill)")
	}
	// Wait long enough for refill (rate=100/sec → 10ms per token; allow
	// margin for scheduler jitter).
	time.Sleep(50 * time.Millisecond)
	if ok, _ := rl.gateRequest(ip, ""); !ok {
		t.Fatal("post-refill request unexpectedly throttled")
	}
}

// --- Per-route cost hook (DD5/FR4.5/NFR6) ---

func TestGateRequestN_ChargesExactlyNTokens(t *testing.T) {
	// Burst of 10, cost of 4 per call: exactly 2 calls (8 tokens) must
	// succeed and a 3rd (would need 4 more, only 2 left) must be throttled
	// — proving the token cost is N, not 1, per call.
	rl := newRateLimiter(RateLimitConfig{
		PerUserRate:  1.0,
		PerUserBurst: 10,
	})
	defer rl.stopCleanup()

	const user = "user-1"
	if ok, _ := rl.gateRequestN("", user, 4); !ok {
		t.Fatal("1st cost-4 call unexpectedly throttled (burst=10)")
	}
	if ok, _ := rl.gateRequestN("", user, 4); !ok {
		t.Fatal("2nd cost-4 call unexpectedly throttled (8/10 tokens spent)")
	}
	if ok, _ := rl.gateRequestN("", user, 4); ok {
		t.Fatal("3rd cost-4 call unexpectedly allowed — only 2 tokens should remain")
	}
}

func TestGateRequestN_CostBelowOneNormalizedToOne(t *testing.T) {
	// A buggy cost function returning 0 or negative must not grant free
	// requests — gateRequestN clamps to a minimum cost of 1.
	rl := newRateLimiter(RateLimitConfig{
		PerUserRate:  1.0,
		PerUserBurst: 1,
	})
	defer rl.stopCleanup()

	if ok, _ := rl.gateRequestN("", "user-1", 0); !ok {
		t.Fatal("first call (cost 0 -> normalized to 1) unexpectedly throttled")
	}
	if ok, _ := rl.gateRequestN("", "user-1", -5); ok {
		t.Fatal("second call (cost -5 -> normalized to 1) unexpectedly allowed — burst=1 should be exhausted")
	}
}

func TestGateRequest_IsCostOneEquivalent(t *testing.T) {
	// gateRequest (the pre-existing default-cost entry point used by the
	// per-IP layer) must behave identically to gateRequestN(..., 1) — no
	// regression from threading the cost parameter through.
	rl := newRateLimiter(RateLimitConfig{PerUserRate: 1.0, PerUserBurst: 1})
	defer rl.stopCleanup()

	if ok, _ := rl.gateRequest("", "user-1"); !ok {
		t.Fatal("first gateRequest call unexpectedly throttled")
	}
	if ok, _ := rl.gateRequest("", "user-1"); ok {
		t.Fatal("second gateRequest call unexpectedly allowed — burst=1 should be exhausted after 1 token")
	}
}

func TestCostForRequest_DefaultsToOneWhenUnregistered(t *testing.T) {
	rl := newRateLimiter(RateLimitConfig{})
	defer rl.stopCleanup()

	r := mux.NewRouter()
	var got int
	r.HandleFunc("/api/v1/sandboxes", func(w http.ResponseWriter, req *http.Request) {
		got = rl.costForRequest(req)
		w.WriteHeader(http.StatusOK)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes", nil))
	if got != 1 {
		t.Errorf("costForRequest() = %d, want 1 (no cost func registered)", got)
	}
}

func TestCostForRequest_UsesRegisteredCostFunc(t *testing.T) {
	rl := newRateLimiter(RateLimitConfig{})
	defer rl.stopCleanup()
	rl.registerRouteCost("/api/v1/sandboxes/bulk", func(req *http.Request) int {
		return 7 // stands in for "N items in the request body"
	})

	r := mux.NewRouter()
	var got int
	r.HandleFunc("/api/v1/sandboxes/bulk", func(w http.ResponseWriter, req *http.Request) {
		got = rl.costForRequest(req)
		w.WriteHeader(http.StatusOK)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/bulk", nil))
	if got != 7 {
		t.Errorf("costForRequest() = %d, want 7 (registered cost func)", got)
	}
}

func TestCostForRequest_RegisteredFuncBelowOneNormalizedToOne(t *testing.T) {
	rl := newRateLimiter(RateLimitConfig{})
	defer rl.stopCleanup()
	rl.registerRouteCost("/api/v1/sandboxes/bulk", func(req *http.Request) int {
		return 0
	})

	r := mux.NewRouter()
	var got int
	r.HandleFunc("/api/v1/sandboxes/bulk", func(w http.ResponseWriter, req *http.Request) {
		got = rl.costForRequest(req)
		w.WriteHeader(http.StatusOK)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/bulk", nil))
	if got != 1 {
		t.Errorf("costForRequest() = %d, want 1 (buggy cost func normalized up)", got)
	}
}

func TestCostForRequest_OtherRoutesUnaffectedByRegisteredCost(t *testing.T) {
	// Registering a cost func for the bulk route must not change the cost
	// of unrelated routes — the hook is per-route, not global.
	rl := newRateLimiter(RateLimitConfig{})
	defer rl.stopCleanup()
	rl.registerRouteCost("/api/v1/sandboxes/bulk", func(req *http.Request) int { return 50 })

	r := mux.NewRouter()
	var got int
	r.HandleFunc("/api/v1/sandboxes/{id}", func(w http.ResponseWriter, req *http.Request) {
		got = rl.costForRequest(req)
		w.WriteHeader(http.StatusOK)
	})
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/sandboxes/sbx-1", nil))
	if got != 1 {
		t.Errorf("costForRequest() = %d, want 1 (unrelated route must not inherit bulk's cost)", got)
	}
}

func TestRateLimitAuthenticatedMiddleware_BulkRouteChargesNTokens(t *testing.T) {
	// End-to-end: a route registered with a cost function of N must exhaust
	// a per-user burst after burst/N calls, not burst calls, proving the
	// middleware actually consults costForRequest instead of hardcoding 1.
	s := &Server{rateLimiter: newRateLimiter(RateLimitConfig{
		PerUserRate:  1.0,
		PerUserBurst: 10,
	})}
	defer s.rateLimiter.stopCleanup()
	s.rateLimiter.registerRouteCost("/api/v1/sandboxes/bulk", func(*http.Request) int { return 5 })

	r := mux.NewRouter()
	r.Use(s.rateLimitAuthenticatedMiddleware)
	r.HandleFunc("/api/v1/sandboxes/bulk", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	reqWithClaims := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/bulk", nil)
		ctx := context.WithValue(req.Context(), ClaimsContextKey, &Claims{Sub: "user-1"})
		return req.WithContext(ctx)
	}

	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, reqWithClaims())
	if rec1.Code != http.StatusOK {
		t.Fatalf("1st bulk call: status = %d, want 200 (5/10 tokens spent)", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, reqWithClaims())
	if rec2.Code != http.StatusOK {
		t.Fatalf("2nd bulk call: status = %d, want 200 (10/10 tokens spent)", rec2.Code)
	}

	rec3 := httptest.NewRecorder()
	r.ServeHTTP(rec3, reqWithClaims())
	if rec3.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd bulk call: status = %d, want 429 — burst should be exhausted after 2 calls at cost 5", rec3.Code)
	}
}
