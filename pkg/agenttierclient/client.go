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

// Package agenttierclient is the shared Go HTTP client for the AgentTier
// Router REST API (`/api/v1`). It centralizes base-URL resolution, auth
// header attachment (API key or bearer token), structured error decoding,
// and one-time deprecation-header warnings so the typed per-resource
// method files in this package (sandboxes.go, governance.go, cluster.go,
// webhooks.go, ...) and cmd/cli commands don't each reimplement them —
// mirroring the role python-sdk/src/agenttier/_http.py plays for the
// Python SDK.
package agenttierclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/agenttier/agenttier/pkg/version"
)

// apiPrefix is appended to Config.APIURL to form the base for every
// request. The REST surface is versioned at /api/v1 (see
// pkg/router/deprecation.go for the deprecation story when v2 ships).
const apiPrefix = "/api/v1"

// defaultUserAgentPrefix identifies this client in Router access logs,
// analogous to the Python SDK's "agenttier-python-sdk/<version>".
const defaultUserAgentPrefix = "agenttier-go-client"

// Config configures a Client. APIURL is the only required field.
type Config struct {
	// APIURL is the Router's base URL, e.g. "https://agenttier.example.com"
	// (no trailing "/api/v1" — Client appends it). Trailing slashes are
	// trimmed.
	APIURL string

	// APIKey, when set, is sent as the X-API-Key header on every request.
	// Takes precedence over BearerToken when both are set, matching the
	// Router's own auth precedence (pkg/router/middleware.go).
	APIKey string

	// BearerToken, when set, is sent as "Authorization: Bearer <token>"
	// (an OIDC JWT or similar). Ignored when APIKey is also set.
	BearerToken string

	// HTTPClient overrides the default *http.Client. Mainly for tests
	// (httptest.Server) and callers needing custom timeouts/transport.
	// Defaults to &http.Client{} (no timeout) when nil.
	HTTPClient *http.Client

	// Logger receives warnings (deprecation notices). Defaults to
	// slog.Default() when nil.
	Logger *slog.Logger
}

// Client is the shared Router HTTP client. Safe for concurrent use — the
// only mutable state is the deprecation-warning dedup set, which is
// guarded by a mutex.
type Client struct {
	baseURL     string
	apiKey      string
	bearerToken string
	httpClient  *http.Client
	logger      *slog.Logger
	userAgent   string

	warnedMu sync.Mutex
	warned   map[string]bool
}

// New returns a Client configured against cfg. Returns an error when
// APIURL is empty — every subsequent request would fail anyway, so this
// fails fast at construction rather than on first call.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.APIURL) == "" {
		return nil, fmt.Errorf("agenttierclient: APIURL must be a non-empty string")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		baseURL:     strings.TrimRight(cfg.APIURL, "/") + apiPrefix,
		apiKey:      cfg.APIKey,
		bearerToken: cfg.BearerToken,
		httpClient:  httpClient,
		logger:      logger,
		userAgent:   defaultUserAgentPrefix + "/" + version.Version,
		warned:      make(map[string]bool),
	}, nil
}

// APIError wraps a non-2xx Router response. Body is the raw decoded JSON
// body (when the response was JSON) so callers needing extension fields —
// e.g. the governance {"error":"policy_violation","violations":[...]}
// shape — can access them without a second decode.
type APIError struct {
	StatusCode int
	Message    string
	Body       map[string]any
}

// Error implements the error interface.
func (e *APIError) Error() string {
	return fmt.Sprintf("agenttierclient: HTTP %d: %s", e.StatusCode, e.Message)
}

// Get issues a GET request and decodes a successful JSON response into
// out (nil when the caller doesn't need the body).
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, nil, out)
}

// Post issues a POST request with body marshaled as JSON (nil for no
// body) and decodes a successful JSON response into out.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	return c.Do(ctx, http.MethodPost, path, body, out)
}

// Put issues a PUT request with body marshaled as JSON and decodes a
// successful JSON response into out.
func (c *Client) Put(ctx context.Context, path string, body, out any) error {
	return c.Do(ctx, http.MethodPut, path, body, out)
}

// Patch issues a PATCH request with body marshaled as JSON and decodes a
// successful JSON response into out.
func (c *Client) Patch(ctx context.Context, path string, body, out any) error {
	return c.Do(ctx, http.MethodPatch, path, body, out)
}

// Delete issues a DELETE request and decodes a successful JSON response
// into out (nil for the common 204 No Content case).
func (c *Client) Delete(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodDelete, path, nil, out)
}

// DoRawUpload issues a request with a raw (non-JSON) request body — used by
// endpoints like PUT .../files/{path} that accept arbitrary bytes rather
// than a JSON envelope. Decodes a successful JSON response into out.
func (c *Client) DoRawUpload(ctx context.Context, method, path, contentType string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("agenttierclient: build request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	c.applyAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("agenttierclient: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	c.warnIfDeprecated(resp)

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("agenttierclient: read response body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return decodeAPIError(resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("agenttierclient: decode response body: %w", err)
		}
	}
	return nil
}

// DoRawDownload issues a GET request and returns the raw response body
// bytes without attempting JSON decoding — used by endpoints like GET
// .../files/{path} that return application/octet-stream. Returns
// *APIError for any non-2xx response.
func (c *Client) DoRawDownload(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("agenttierclient: build request: %w", err)
	}
	req.Header.Set("User-Agent", c.userAgent)
	c.applyAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agenttierclient: GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	c.warnIfDeprecated(resp)

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("agenttierclient: read response body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, decodeAPIError(resp.StatusCode, raw)
	}
	return raw, nil
}

// Do issues an HTTP request against path (relative to the /api/v1 prefix,
// e.g. "/sandboxes/sbx-1") and decodes a successful JSON response into
// out. body, when non-nil, is marshaled as the JSON request body; out,
// when non-nil, receives the decoded JSON response body. Returns
// *APIError for any non-2xx response.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("agenttierclient: marshal request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("agenttierclient: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	c.applyAuth(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("agenttierclient: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	c.warnIfDeprecated(resp)

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("agenttierclient: read response body: %w", err)
	}

	if resp.StatusCode/100 != 2 {
		return decodeAPIError(resp.StatusCode, raw)
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("agenttierclient: decode response body: %w", err)
		}
	}
	return nil
}

// applyAuth attaches whichever credential Config supplied. API key takes
// precedence over bearer token (see Config.APIKey doc comment).
func (c *Client) applyAuth(req *http.Request) {
	switch {
	case c.apiKey != "":
		req.Header.Set("X-API-Key", c.apiKey)
	case c.bearerToken != "":
		req.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}
}

// decodeAPIError builds an *APIError from a non-2xx response body,
// preferring the Router's {"error": "..."} / {"message": "..."} shape and
// falling back to the raw body text when it isn't JSON.
func decodeAPIError(status int, raw []byte) *APIError {
	apiErr := &APIError{StatusCode: status, Message: http.StatusText(status)}
	if len(raw) == 0 {
		return apiErr
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		apiErr.Message = strings.TrimSpace(string(raw))
		return apiErr
	}
	apiErr.Body = decoded
	if msg, ok := decoded["error"].(string); ok && msg != "" {
		apiErr.Message = msg
	} else if msg, ok := decoded["message"].(string); ok && msg != "" {
		apiErr.Message = msg
	}
	return apiErr
}

// warnIfDeprecated logs a one-time warning when the Router flags an
// endpoint as deprecated (Deprecation: true), mirroring the Python SDK's
// warn_if_deprecated (python-sdk/src/agenttier/_http.py) and the legacy
// CLI's warnIfDeprecated (cmd/cli/main.go). Silenced by
// AGENTTIER_DEPRECATION_WARNINGS=off.
func (c *Client) warnIfDeprecated(resp *http.Response) {
	if !strings.EqualFold(resp.Header.Get("Deprecation"), "true") {
		return
	}
	if strings.EqualFold(os.Getenv("AGENTTIER_DEPRECATION_WARNINGS"), "off") {
		return
	}
	if resp.Request == nil {
		return
	}
	key := resp.Request.Method + " " + resp.Request.URL.Path

	c.warnedMu.Lock()
	seen := c.warned[key]
	if !seen {
		c.warned[key] = true
	}
	c.warnedMu.Unlock()
	if seen {
		return
	}

	msg := "agenttier API endpoint " + key + " is deprecated"
	if sunset := resp.Header.Get("Sunset"); sunset != "" {
		msg += " (sunset " + sunset + ")"
	}
	if link := resp.Header.Get("Link"); strings.Contains(link, "successor-version") {
		msg += "; see " + link
	}
	c.logger.Warn(msg)
}
