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

// Package router implements the AgentTier Router — the HTTP/WebSocket server
// providing REST API, terminal access, port forwarding, and authentication.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/agenttier/agenttier/pkg/governance"
	agentotel "github.com/agenttier/agenttier/pkg/otel"
	"github.com/agenttier/agenttier/pkg/router/agent"
	"github.com/agenttier/agenttier/pkg/router/auth"
	"github.com/agenttier/agenttier/pkg/router/portforward"
	"github.com/agenttier/agenttier/pkg/router/terminal"
	"github.com/agenttier/agenttier/pkg/storage"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
)

// Config holds the Router server configuration.
type Config struct {
	ListenAddr       string
	MetricsAddr      string
	OIDCIssuerURL    string
	OIDCClientID     string
	AdminGroup       string
	GroupClaim       string
	PreviewDomain    string
	GatewayName      string
	IngressClassName string
	KubeConfig       string
	// InstallNamespace is where AgentTier itself runs (and therefore where
	// the warm pool ConfigMap, pool Pods, and pool PVCs live). Set from
	// POD_NAMESPACE in the router deployment.
	InstallNamespace string
	// SandboxNamespace is where Sandboxes (and therefore warm pool Pods)
	// live. Used when reporting warm pool status so the Router lists pool
	// Pods in the right namespace. Set from SANDBOX_NAMESPACE in the router
	// deployment; empty falls back to the warm pool's DefaultSandboxNamespace
	// ("default").
	SandboxNamespace string
	// CORSAllowedOrigins is the list of origins permitted to make
	// cross-origin requests. An incoming Origin header must exactly match
	// one of these values for the response to carry
	// Access-Control-Allow-Origin with that origin. An empty list disables
	// CORS entirely (no Access-Control-Allow-Origin header is emitted).
	// Never use "*" here — the authenticated API accepts Authorization and
	// X-API-Key headers, so wildcard CORS would strip the browser's
	// same-origin guard on response reading.
	// Set via --cors-allowed-origins (comma-separated) or
	// AGENTTIER_CORS_ALLOWED_ORIGINS env var; Helm value cors.allowedOrigins.
	CORSAllowedOrigins []string
	// DevAuth, when true, bypasses authentication and stamps every request
	// with a default admin identity. This is the local-development
	// convenience path. It is OFF by default so a misconfigured production
	// install fails closed (401) instead of open (blanket admin). The
	// router main only sets this from an explicit --dev-auth flag /
	// AGENTTIER_DEV_AUTH env var, and logs a loud warning at startup when
	// it's active.
	DevAuth bool
	// RateLimit configures per-IP and per-user request throttling. The
	// zero value disables rate limiting entirely (today's behavior); set
	// to DefaultRateLimitConfig() or a customized variant to enforce
	// limits.
	RateLimit RateLimitConfig
	// StorageBackend is the optional historical-records sink (SQL). Nil
	// (the default) means a no-op backend — Kubernetes Events stay the
	// source of truth. cmd/router constructs a SQLBackend here when an
	// operator configures a DSN.
	StorageBackend storage.Backend
}

// Server is the main Router HTTP server.
type Server struct {
	config          *Config
	router          *mux.Router
	httpServer      *http.Server
	logger          *slog.Logger
	k8sClient       client.Client
	bridge          *terminal.Bridge
	sessionManager  *terminal.Manager
	governanceStore governance.Store
	portForward     *portforward.Manager
	rateLimiter     *rateLimiter
	// oidcValidator validates OIDC JWT bearer tokens. Nil when no OIDC
	// issuer is configured (dev-auth mode) — authMiddleware checks for nil
	// and only reaches the validator on the authenticated path.
	oidcValidator *auth.OIDCValidator
	// apiKeyValidator validates X-API-Key headers against Secret-backed
	// storage with an LRU cache. Always constructed; the store is the
	// Kubernetes Secret store.
	apiKeyValidator *auth.APIKeyValidator
	// store is the optional historical-records backend (SQL) for audit
	// events, sandbox events, and cost snapshots. Defaults to a no-op so
	// the Kubernetes-native path is unchanged when no SQL is configured.
	store storage.Backend
}

// NewServer creates a new Router server with all routes registered.
func NewServer(config *Config, k8sClient client.Client, bridge *terminal.Bridge) *Server {
	// Wrap the JSON handler in the OTel slog handler so trace_id and
	// span_id automatically appear in any log line written via
	// LogAttrs(ctx, ...). When the process boots without an OTLP
	// exporter the wrapper is a near-zero-cost noop, so this is safe
	// to keep on unconditionally.
	logger := slog.New(agentotel.NewSlogContextHandler(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	r := mux.NewRouter()
	s := &Server{
		config:          config,
		router:          r,
		logger:          logger,
		k8sClient:       k8sClient,
		bridge:          bridge,
		sessionManager:  terminal.NewManager(logger),
		governanceStore: governance.NewConfigMapStore(k8sClient),
		portForward: portforward.New(k8sClient, portforward.Options{
			PreviewDomain:    config.PreviewDomain,
			IngressClassName: config.IngressClassName,
		}),
	}
	// Historical-records backend: a SQL store when the operator configured
	// one (injected via Config), otherwise a no-op so the Kubernetes-native
	// behavior is unchanged.
	s.store = config.StorageBackend
	if s.store == nil {
		s.store = storage.NewNoopBackend()
	}
	// Rate limiter: zero-config = disabled (today's behavior). Operators
	// opt in by setting Helm values that populate config.RateLimit, which
	// the cmd/router main flag parser threads through.
	if config.RateLimit.PerIPRate > 0 || config.RateLimit.PerUserRate > 0 {
		s.rateLimiter = newRateLimiter(config.RateLimit)
	}

	// API-key validator: always constructed, backed by Kubernetes Secrets
	// in the install namespace. Cache 1024 keys for 5 minutes so a hot key
	// doesn't hit the apiserver on every request.
	s.apiKeyValidator = auth.NewAPIKeyValidator(
		newSecretAPIKeyStore(k8sClient, config.InstallNamespace),
		1024, 5*time.Minute,
	)

	// OIDC validator: constructed only when an issuer is configured. The
	// constructor does network I/O (discovery + initial JWKS fetch); if
	// that fails at boot we log and leave the validator nil. authMiddleware
	// treats a nil validator on the JWT path as a hard 401 (fail closed) —
	// a misconfigured or unreachable issuer must never silently fall back
	// to allowing requests.
	if config.OIDCIssuerURL != "" {
		v, err := auth.NewOIDCValidator(auth.OIDCConfig{
			IssuerURL:  config.OIDCIssuerURL,
			ClientID:   config.OIDCClientID,
			AdminGroup: config.AdminGroup,
			GroupClaim: config.GroupClaim,
		})
		if err != nil {
			logger.Error("OIDC validator initialization failed; JWT auth will reject all tokens until the issuer is reachable and the Router restarts",
				"issuer", config.OIDCIssuerURL, "error", err)
		} else {
			s.oidcValidator = v
			logger.Info("OIDC validator initialized", "issuer", config.OIDCIssuerURL)
		}
	}

	if config.DevAuth {
		// Intentionally loud: operators misconfiguring prod installs must
		// see this immediately in logs and understand the risk (M2).
		logger.Warn("AUTHENTICATION DISABLED — LOCAL DEV ONLY. All requests are granted admin identity; no credentials are verified. Never use --dev-auth or AGENTTIER_DEV_AUTH in production.")
	} else if config.OIDCIssuerURL == "" {
		logger.Warn("no OIDC issuer configured and --dev-auth is off — all API requests will be rejected with 401. Set auth.oidc.issuerUrl (prod) or --dev-auth (local dev).")
	}

	// Register middleware
	r.Use(s.requestIDMiddleware)
	r.Use(s.corsMiddleware)
	r.Use(s.loggingMiddleware)
	// Per-IP rate limiting runs before auth so anonymous abuse gets
	// throttled even without a valid token. No-op when rateLimiter is nil.
	r.Use(s.rateLimitMiddleware)

	// Global OPTIONS catch-all for CORS preflight. Gorilla/mux routes
	// method-mismatch requests to MethodNotAllowedHandler which bypasses
	// middleware, so we register an explicit OPTIONS route for all paths
	// here. The corsMiddleware installed above handles the actual preflight
	// response (204 for allowed origins, 403 for disallowed) and returns
	// before this handler body is ever executed.
	r.PathPrefix("/").Methods(http.MethodOptions).HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// corsMiddleware already wrote the response for OPTIONS requests
		// that carry an Origin header. This handler body is only reached
		// for OPTIONS requests without an Origin header, which are not
		// browser CORS preflights — respond 204 so tools like curl and
		// Postman still work.
		w.WriteHeader(http.StatusNoContent)
	})

	// Health and metrics (no auth required)
	r.HandleFunc("/healthz", s.handleHealthz).Methods("GET")
	r.HandleFunc("/readyz", s.handleReadyz).Methods("GET")
	r.HandleFunc("/metrics", promhttp.Handler().ServeHTTP).Methods("GET")

	// API routes (auth required)
	api := r.PathPrefix("/api/v1").Subrouter()
	api.Use(s.authMiddleware)
	// Per-user rate limit overlays the per-IP cap for authenticated
	// callers. Mounted after authMiddleware so claims are available.
	api.Use(s.rateLimitAuthenticatedMiddleware)
	// Stamps Deprecation/Sunset headers on any endpoint flagged in
	// deprecatedRoutes (empty today — no-op until an /api/v2 ships).
	api.Use(s.deprecationMiddleware)

	// FR6: enforce sandbox-scoped API key restrictions on every sandbox-{id}
	// route (and, by default-deny, every other route too — see
	// scopedkey_middleware.go). Mounted right after auth so Claims are
	// available; a non-scoped key (empty SandboxID) passes through
	// untouched, so this has no effect on the existing JWT/user-key paths.
	api.Use(s.requireSandboxScope)

	// Sandbox CRUD
	api.HandleFunc("/sandboxes", s.handleListSandboxes).Methods("GET")
	api.HandleFunc("/sandboxes", s.handleCreateSandbox).Methods("POST")
	api.HandleFunc("/sandboxes/{id}", s.handleGetSandbox).Methods("GET")
	api.HandleFunc("/sandboxes/{id}", s.handleDeleteSandbox).Methods("DELETE")
	api.HandleFunc("/sandboxes/{id}/stop", s.handleStopSandbox).Methods("POST")
	api.HandleFunc("/sandboxes/{id}/resume", s.handleResumeSandbox).Methods("POST")
	api.HandleFunc("/sandboxes/{id}/clone", s.handleCloneSandbox).Methods("POST")

	// FR2: live sandbox mutation (idleTimeout/resources/labels/annotations).
	api.HandleFunc("/sandboxes/{id}", s.handlePatchSandbox).Methods("PATCH")

	// FR4: bulk operations. Rejected outright for sandbox-scoped keys inside
	// the handlers themselves (DD3/NFR2); rate-limit cost scales with batch
	// size (DD5/NFR6) via the cost functions registered below.
	api.HandleFunc("/sandboxes/bulk", s.handleBulkCreate).Methods("POST")
	api.HandleFunc("/sandboxes/bulk-action", s.handleBulkAction).Methods("POST")

	// FR3: backup/restore, wrapping the existing VolumeSnapshot scheduler.
	api.HandleFunc("/sandboxes/{id}/backups", s.handleListBackups).Methods("GET")
	api.HandleFunc("/sandboxes/{id}/backups", s.handleCreateBackup).Methods("POST")
	api.HandleFunc("/sandboxes/{id}/backups/{snapshotName}/restore", s.handleRestoreBackup).Methods("POST")
	api.HandleFunc("/sandboxes/{id}/backups/{snapshotName}", s.handleDeleteBackup).Methods("DELETE")

	// Command execution
	api.HandleFunc("/sandboxes/{id}/exec", s.handleExecCommand).Methods("POST")

	// File transfer
	api.HandleFunc("/sandboxes/{id}/files/", s.handleListFiles).Methods("GET")
	api.HandleFunc("/sandboxes/{id}/files/{path:.*}", s.handleGetFile).Methods("GET")
	api.HandleFunc("/sandboxes/{id}/files/{path:.*}", s.handlePutFile).Methods("PUT")
	// Streaming archive download. /archive?path=/workspace[/subdir] returns
	// a real .zip with the subtree contents. Path is locked to /workspace.
	api.HandleFunc("/sandboxes/{id}/archive", s.handleArchive).Methods("GET")

	// Port forwarding
	api.HandleFunc("/sandboxes/{id}/ports", s.handleListPorts).Methods("GET")
	api.HandleFunc("/sandboxes/{id}/ports", s.handleForwardPort).Methods("POST")
	api.HandleFunc("/sandboxes/{id}/ports/{port}", s.handleRemovePort).Methods("DELETE")
	// Authenticated HTTP proxy into a forwarded port. Works even without a
	// public Ingress — useful for dev clusters and end-to-end testing.
	api.PathPrefix("/sandboxes/{id}/preview/{port}/").HandlerFunc(s.handlePortPreview)
	api.HandleFunc("/sandboxes/{id}/preview/{port}", s.handlePortPreview)

	// Sharing
	api.HandleFunc("/sandboxes/{id}/share", s.handleGetSharing).Methods("GET")
	api.HandleFunc("/sandboxes/{id}/share", s.handleShareSandbox).Methods("POST")
	api.HandleFunc("/sandboxes/{id}/share/{userId}", s.handleRevokeShare).Methods("DELETE")
	api.HandleFunc("/sandboxes/{id}/share-links", s.handleCreateShareLink).Methods("POST")

	// Templates. Read is open to any authenticated user; create/update/delete
	// are admin-gated because a template defines the container image, resource
	// limits, system prompt, harness config, and ServiceAccount/IRSA wiring
	// sandboxes run under, and is the unit governance allowlists reference —
	// letting a non-admin mutate one is a privilege-escalation + governance
	// bypass.
	api.HandleFunc("/templates", s.handleListTemplates).Methods("GET")
	api.Handle("/templates", s.requireAdmin(http.HandlerFunc(s.handleCreateTemplate))).Methods("POST")
	api.HandleFunc("/templates/{name}", s.handleGetTemplate).Methods("GET")
	api.Handle("/templates/{name}", s.requireAdmin(http.HandlerFunc(s.handleUpdateTemplate))).Methods("PUT")
	api.Handle("/templates/{name}", s.requireAdmin(http.HandlerFunc(s.handleDeleteTemplate))).Methods("DELETE")

	// FR5: webhook subscriptions. Not a sandbox-scoped operation (DD3) — the
	// handlers reject scoped keys the same way bulk does, and by
	// default-deny requireSandboxScope 403s any scoped key here regardless
	// (these routes have no /sandboxes/{id} shape at all).
	api.HandleFunc("/webhooks", s.handleCreateWebhook).Methods("POST")
	api.HandleFunc("/webhooks", s.handleListWebhooks).Methods("GET")
	api.HandleFunc("/webhooks/{id}", s.handleDeleteWebhook).Methods("DELETE")
	api.HandleFunc("/webhooks/{id}/deliveries", s.handleGetWebhookDeliveries).Methods("GET")

	// Governance
	api.HandleFunc("/governance/policies", s.handleListPolicies).Methods("GET")
	api.Handle("/governance/policies", s.requireAdmin(http.HandlerFunc(s.handleUpsertClusterPolicy))).Methods("PUT")
	api.HandleFunc("/governance/policies/{namespace}", s.handleGetPolicy).Methods("GET")
	api.Handle("/governance/policies/{namespace}", s.requireAdmin(http.HandlerFunc(s.handleSetPolicy))).Methods("PUT")
	api.Handle("/governance/policies/{namespace}", s.requireAdmin(http.HandlerFunc(s.handleDeletePolicy))).Methods("DELETE")
	api.HandleFunc("/governance/effective", s.handleGetEffectivePolicy).Methods("GET")

	// Audit — admin-only: lists Sandbox Events across all namespaces with no
	// per-owner filter, so it would otherwise leak every tenant's sandbox
	// names + event messages to any authenticated user.
	api.Handle("/audit/events", s.requireAdmin(http.HandlerFunc(s.handleListAuditEvents))).Methods("GET")

	// Analytics — admin-only: aggregates usage + cost across all tenants.
	api.Handle("/analytics/usage", s.requireAdmin(http.HandlerFunc(s.handleGetUsageAnalytics))).Methods("GET")
	api.Handle("/analytics/costs", s.requireAdmin(http.HandlerFunc(s.handleGetCostEstimates))).Methods("GET")

	// Admin — both routes are admin-gated (M1): they enumerate or aggregate
	// cross-tenant data, so a non-admin must never reach them.
	api.Handle("/admin/sandboxes", s.requireAdmin(http.HandlerFunc(s.handleAdminListSandboxes))).Methods("GET")
	api.Handle("/admin/sharing", s.requireAdmin(http.HandlerFunc(s.handleAdminListSharing))).Methods("GET")

	// User preferences
	api.HandleFunc("/user/me", s.handleGetMe).Methods("GET")
	api.HandleFunc("/user/preferences", s.handleGetPreferences).Methods("GET")
	api.HandleFunc("/user/preferences", s.handleUpdatePreferences).Methods("PUT")
	api.HandleFunc("/user/api-keys", s.handleListAPIKeys).Methods("GET")
	api.HandleFunc("/user/api-keys", s.handleCreateAPIKey).Methods("POST")
	api.HandleFunc("/user/api-keys/{keyId}", s.handleRevokeAPIKey).Methods("DELETE")

	// Warm pool. Status is readable by any authenticated user; setting the
	// desired count is a cost/capacity decision, so it's admin-gated to match
	// the cluster-headroom write below.
	api.HandleFunc("/warmpool/status", s.handleGetWarmPoolStatus).Methods("GET")
	api.Handle("/warmpool/config", s.requireAdmin(http.HandlerFunc(s.handleSetWarmPoolConfig))).Methods("PUT")

	// Cluster status — node + pod headcount for the Web UI's left-nav glance.
	// Auth via the same /api/v1 middleware; ClusterRole already grants the
	// node + pod read verbs we need.
	api.HandleFunc("/cluster/status", s.handleGetClusterStatus).Methods("GET")
	api.Handle("/cluster/nodes", s.requireAdmin(http.HandlerFunc(s.handleGetClusterNodes))).Methods("GET")

	// Cluster headroom (spare-capacity Deployment). Read-only for any
	// authenticated user, write-gated to admins because changing the
	// replica count is a cost decision and the cap is non-trivial.
	api.HandleFunc("/cluster/headroom", s.handleGetHeadroomConfig).Methods("GET")
	api.Handle("/cluster/headroom", s.requireAdmin(http.HandlerFunc(s.handleSetHeadroomConfig))).Methods("PUT")

	// WebSocket terminal (auth handled inside handler)
	r.HandleFunc("/ws/terminal/{sandboxId}", s.handleTerminalWebSocket)

	// Agent-mode endpoints (POST /sandboxes/{id}/configure today; /invoke
	// in the next milestone). Lives in pkg/router/agent so it can evolve
	// independently of the interactive code-mode surface.
	agentHandler := agent.New(agent.Options{
		K8sClient:    s.k8sClient,
		Bridge:       s.bridge,
		Logger:       s.logger,
		ClaimsLookup: s.agentClaims,
		SandboxOf:    s.agentSandboxOf,
		PolicyOf: func(ctx context.Context, namespace string) (governance.Policy, error) {
			if s.governanceStore == nil {
				return governance.Policy{}, nil
			}
			return governance.Resolve(ctx, s.governanceStore, namespace)
		},
		// HTTP-exec opt-in resolver. The agent /invoke handler asks for
		// a dispatcher per request; when the sandbox is opted in and
		// the runtime is reachable, /invoke streams via HTTP and cancel
		// flows through the runtime's registry — fixing the
		// cross-replica cancel bug. Falls back to SPDY transparently
		// when this returns (nil, false).
		HTTPExecOf: s.agentHTTPExec,
	})
	agentHandler.RegisterRoutes(api)

	// FR4.5/NFR6/DD5: bulk routes cost N tokens against the per-user rate
	// limit, where N is the batch size — never less than the sum of the
	// per-item equivalents. A no-op when rate limiting is disabled
	// (s.rateLimiter is nil in the zero-config default). The cost
	// functions decode only the field they need (items/ids length) and
	// deliberately ignore decode errors — a malformed body still costs at
	// least 1 (registerRouteCost's own floor), and the handler's own
	// json.Decode surfaces the real validation error to the caller.
	if s.rateLimiter != nil {
		s.rateLimiter.registerRouteCost("/api/v1/sandboxes/bulk", bulkCreateRouteCost)
		s.rateLimiter.registerRouteCost("/api/v1/sandboxes/bulk-action", bulkActionRouteCost)
	}

	// Wrap the mux with OTel HTTP instrumentation. otelhttp.NewHandler
	// extracts incoming W3C Trace Context headers, starts a server span
	// for each request, and propagates the resulting context down to
	// handlers via r.Context(). Healthz / readyz / metrics are excluded
	// to keep span volume sane — kubelet hits readyz every couple of
	// seconds and Prometheus hits metrics on its scrape cadence.
	//
	// SpanNameFormatter follows the steering rule for span naming
	// (`service.operation`): "router.GET" / "router.POST" etc. The mux
	// route template (e.g. /api/v1/sandboxes/{id}) shows up as a span
	// attribute via otelhttp's `http.route` semconv, so we don't
	// embed dynamic IDs in the span name.
	otelHandler := otelhttp.NewHandler(r, "router",
		otelhttp.WithFilter(func(req *http.Request) bool {
			return !isOTelExempt(req.URL.Path)
		}),
		otelhttp.WithSpanNameFormatter(func(_ string, req *http.Request) string {
			return "router." + req.Method
		}),
	)

	s.httpServer = &http.Server{
		Addr:    config.ListenAddr,
		Handler: otelHandler,
		// ReadTimeout: 0 is required so WebSocket upgrades can hold a
		// connection open beyond any HTTP read deadline. But a global
		// zero ReadTimeout exposes every endpoint (REST, SSE, file
		// PUT/GET) to slowloris — a client can dribble bytes out
		// indefinitely and pin a goroutine + fd.
		//
		// ReadHeaderTimeout bounds the request-line + headers phase
		// only and is allowed alongside ReadTimeout: 0. Five seconds
		// is enough for any legitimate client (curl over a slow
		// network finishes headers in well under a second) and short
		// enough that slowloris can't hold a connection waiting for
		// header data. WebSocket upgrades complete their handshake
		// within this window, then escape into the long-lived
		// hijacked path.
		ReadTimeout:       0,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	return s
}

// Start begins serving HTTP requests and blocks until shutdown.
//
// It uses signal.NotifyContext so that SIGTERM/SIGINT cancel the root
// context and trigger a graceful shutdown. The shutdown drain then runs on
// its own fresh 30 s timeout so in-flight requests keep their full drain
// budget even though the root context is already cancelled (M10).
func (s *Server) Start() error {
	// Root context cancelled on SIGTERM or SIGINT.
	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		s.logger.Info("router starting", "addr", s.config.ListenAddr)
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.logger.Error("server error", "error", err)
			os.Exit(1)
		}
	}()

	<-rootCtx.Done()
	s.logger.Info("shutting down gracefully")

	// Use a fresh background context (NOT the already-cancelled rootCtx) for
	// the drain, so in-flight requests get the full 30 s budget after the
	// signal fires. 30 s is generous; typical HTTP long-polling and WebSocket
	// connections are drained before that.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	return s.httpServer.Shutdown(ctx)
}

// --- Health endpoints ---

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	// Try a cheap, unprivileged read against the API server cache. If the
	// Router has lost its connection to the K8s API (network partition,
	// service-account token rotation glitch, kubelet churn) every real
	// endpoint will return 5xx; reporting healthy here would keep the
	// Service routing traffic to a broken pod.
	//
	// Limit=1 keeps the round-trip tiny. We use a short context timeout
	// so a hung apiserver can't pin this handler — Kubernetes fails the
	// probe and reroutes traffic.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	list := &agenttierv1alpha1.SandboxList{}
	if err := s.k8sClient.List(ctx, list, client.Limit(1)); err != nil {
		s.logger.Warn("readyz: K8s API unreachable", "error", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "not ready: %v", err)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "ok")
}

// agentClaims adapts the Router's request-scoped Claims into the minimal
// shape the agent subpackage consumes. Returns nil when no claims are set
// (handler returns 401).
func (s *Server) agentClaims(r *http.Request) *agent.Claims {
	c := GetClaims(r.Context())
	if c == nil {
		return nil
	}
	return &agent.Claims{Sub: c.Sub, Email: c.Email, Name: c.Name, IsAdmin: c.IsAdmin}
}

// agentSandboxOf reuses the existing ownership-aware sandbox lookup so the
// agent subpackage doesn't reimplement RBAC. The agent.Claims received here
// must round-trip through a router.Claims for the helper to apply admin /
// ownership rules consistently.
func (s *Server) agentSandboxOf(ctx context.Context, sandboxID string, ac *agent.Claims) (*agenttierv1alpha1.Sandbox, error) {
	if ac == nil {
		return nil, fmt.Errorf("authentication required")
	}
	rc := &Claims{Sub: ac.Sub, Email: ac.Email, Name: ac.Name, IsAdmin: ac.IsAdmin}
	return s.getSandboxWithAuthCheck(ctx, sandboxID, rc)
}

// isOTelExempt reports whether the path should NOT produce a server span.
// We skip kubelet probes, the Prometheus scrape, and WebSocket upgrades
// (long-lived connections that would generate a single multi-hour span
// containing every keystroke event — which is not what tracing is for).
func isOTelExempt(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/metrics":
		return true
	}
	return strings.HasPrefix(path, "/ws/")
}

// bulkCreateRouteCost is the RouteCostFunc for POST /sandboxes/bulk
// (FR4.5/NFR6/DD5): the token cost is the batch size (len(items)), so an
// N-item bulk create never costs less than N individual creates would.
func bulkCreateRouteCost(r *http.Request) int {
	var body struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return 1
	}
	return len(body.Items)
}

// bulkActionRouteCost is the RouteCostFunc for POST /sandboxes/bulk-action
// (FR4.5/NFR6/DD5): the token cost is the batch size (len(ids)).
func bulkActionRouteCost(r *http.Request) int {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return 1
	}
	return len(body.IDs)
}
