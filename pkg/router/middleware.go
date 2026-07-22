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
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/agenttier/agenttier/pkg/router/auth"
)

// contextKey is a custom type for context keys to avoid collisions.
type contextKey string

const (
	// ClaimsContextKey is the context key for authenticated user claims.
	ClaimsContextKey contextKey = "claims"
	// RequestIDContextKey is the context key for the request ID.
	RequestIDContextKey contextKey = "requestID"
)

// Claims represents the authenticated user's identity extracted from JWT or API key.
//
// SandboxID/ActionGroups are non-empty only when the request authenticated
// with a sandbox-scoped API key (FR6); requireSandboxScope middleware
// enforces the bound sandbox + action-group restriction using these fields.
// A JWT or user-level API key leaves both empty.
type Claims struct {
	Sub          string   `json:"sub"`
	Email        string   `json:"email"`
	Name         string   `json:"name"`
	Groups       []string `json:"groups"`
	IsAdmin      bool     `json:"isAdmin"`
	Namespaces   []string `json:"namespaces,omitempty"` // For API key scoping
	SandboxID    string   `json:"sandboxId,omitempty"`
	ActionGroups []string `json:"actionGroups,omitempty"`
}

// GetClaims extracts the authenticated claims from the request context.
func GetClaims(ctx context.Context) *Claims {
	claims, _ := ctx.Value(ClaimsContextKey).(*Claims)
	return claims
}

// requestIDMiddleware adds a unique request ID to each request.
func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" {
			requestID = generateRequestID()
		}
		w.Header().Set("X-Request-ID", requestID)
		ctx := context.WithValue(r.Context(), RequestIDContextKey, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// corsMiddleware handles Cross-Origin Resource Sharing headers.
//
// Only origins present in Config.CORSAllowedOrigins receive an
// Access-Control-Allow-Origin echo. A wildcard ("*") is never emitted because
// the API accepts Authorization and X-API-Key headers — a wildcard would strip
// the browser's same-origin guard on response reading. When the allowlist is
// empty, no CORS headers are set and cross-origin requests are blocked by the
// browser.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && s.isAllowedOrigin(origin) {
			// The allowed-origin echo is per-request, so caches must key on
			// Origin to avoid serving one origin's CORS response to another.
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Request-ID")
			w.Header().Set("Access-Control-Expose-Headers", "X-Request-ID")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}

		if r.Method == "OPTIONS" {
			switch {
			case origin == "":
				// Not a browser CORS preflight (no Origin). Non-CORS tools
				// like curl/Postman and same-origin OPTIONS get 204 so they
				// still work.
				w.WriteHeader(http.StatusNoContent)
			case s.isAllowedOrigin(origin):
				w.WriteHeader(http.StatusNoContent)
			default:
				// Cross-origin preflight from a disallowed origin — reject so
				// the browser blocks the actual request.
				w.WriteHeader(http.StatusForbidden)
			}
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isAllowedOrigin reports whether the given origin is in the configured
// CORSAllowedOrigins list. The comparison is case-sensitive per RFC 6454.
func (s *Server) isAllowedOrigin(origin string) bool {
	for _, allowed := range s.config.CORSAllowedOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
}

// loggingMiddleware logs each request with duration and status.
//
// We log via slog.LogAttrs(r.Context(), ...) so the request's trace
// context (set upstream by the otelhttp wrapper) flows through the
// SlogContextHandler and gets stamped with trace_id + span_id. That
// gives an operator a single grep-by-trace-id query that hits both
// log lines and OTLP traces.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		duration := time.Since(start)
		s.logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", wrapped.statusCode),
			slog.Int64("duration_ms", duration.Milliseconds()),
			slog.String("remote_addr", r.RemoteAddr),
			slog.String("user_agent", r.UserAgent()),
		)
	})
}

// authMiddleware validates JWT tokens or API keys and injects claims into context.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var claims *Claims
		var err error

		// Dev-auth mode: bypass authentication and stamp a default admin
		// identity. Gated behind an EXPLICIT --dev-auth flag (config.DevAuth)
		// so a production install that simply forgot to set an OIDC issuer
		// fails closed (401 below) instead of silently granting blanket
		// admin. NewServer logs a loud warning when this is active.
		if s.config.DevAuth {
			claims = &Claims{
				Sub:     "dev-user",
				Email:   "dev@agenttier.local",
				Name:    "Dev User",
				Groups:  []string{"agenttier-admins"},
				IsAdmin: true,
			}
			ctx := context.WithValue(r.Context(), ClaimsContextKey, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		// Try API key first (X-API-Key header)
		apiKey := r.Header.Get("X-API-Key")
		if apiKey != "" {
			claims, err = s.validateAPIKey(r.Context(), apiKey)
			if err != nil {
				http.Error(w, `{"error":"invalid_api_key"}`, http.StatusUnauthorized)
				return
			}
		} else {
			// Try Bearer token (OIDC JWT)
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				http.Error(w, `{"error":"missing_authorization"}`, http.StatusUnauthorized)
				return
			}

			token := strings.TrimPrefix(authHeader, "Bearer ")
			if token == authHeader {
				http.Error(w, `{"error":"invalid_authorization_format"}`, http.StatusUnauthorized)
				return
			}

			claims, err = s.validateJWT(r.Context(), token)
			if err != nil {
				http.Error(w, `{"error":"invalid_token","message":"`+err.Error()+`"}`, http.StatusUnauthorized)
				return
			}
		}

		// Inject claims into context
		ctx := context.WithValue(r.Context(), ClaimsContextKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// validateJWT validates an OIDC JWT bearer token against the configured
// issuer's JWKS and returns the mapped claims. Fails closed: if no OIDC
// validator is configured (issuer unset or its boot-time init failed), every
// token is rejected rather than allowed through.
func (s *Server) validateJWT(ctx context.Context, token string) (*Claims, error) {
	if s.oidcValidator == nil {
		return nil, fmt.Errorf("OIDC authentication is not configured on this server")
	}
	ac, err := s.oidcValidator.ValidateToken(ctx, token)
	if err != nil {
		return nil, err
	}
	return claimsFromAuth(ac), nil
}

// validateAPIKey validates an X-API-Key value against Secret-backed storage
// with an LRU cache, returning the mapped claims.
func (s *Server) validateAPIKey(ctx context.Context, key string) (*Claims, error) {
	if s.apiKeyValidator == nil {
		return nil, fmt.Errorf("API key authentication is not configured on this server")
	}
	ac, err := s.apiKeyValidator.ValidateKey(ctx, key)
	if err != nil {
		return nil, err
	}
	return claimsFromAuth(ac), nil
}

// claimsFromAuth converts the auth package's Claims (the validators' return
// type) into the router package's Claims (what handlers read from context).
// The two structs are intentionally separate so the auth package has no
// dependency on the router package.
func claimsFromAuth(ac *auth.Claims) *Claims {
	if ac == nil {
		return nil
	}
	return &Claims{
		Sub:          ac.Sub,
		Email:        ac.Email,
		Name:         ac.Name,
		Groups:       ac.Groups,
		IsAdmin:      ac.IsAdmin,
		SandboxID:    ac.SandboxID,
		ActionGroups: ac.ActionGroups,
	}
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Hijack implements http.Hijacker for WebSocket support.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hijacker, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return hijacker.Hijack()
	}
	return nil, nil, fmt.Errorf("upstream ResponseWriter does not implement http.Hijacker")
}

// Flush implements http.Flusher.
func (rw *responseWriter) Flush() {
	if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// generateRequestID creates a random hex request ID.
func generateRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// requireAdmin wraps a handler so only admin users may reach it. In dev mode
// (no OIDC configured), the authMiddleware already stamps every request with
// an admin identity, so this is a no-op there; in production it enforces the
// `IsAdmin` claim.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims := GetClaims(r.Context())
		if claims == nil || !claims.IsAdmin {
			http.Error(w, `{"error":"admin_required"}`, http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
