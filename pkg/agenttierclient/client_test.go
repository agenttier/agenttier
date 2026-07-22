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

package agenttierclient

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNew_RequiresAPIURL(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error for empty APIURL, got nil")
	}
}

func TestNew_TrimsTrailingSlash(t *testing.T) {
	c, err := New(Config{APIURL: "https://example.com/"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if c.baseURL != "https://example.com/api/v1" {
		t.Fatalf("baseURL = %q, want %q", c.baseURL, "https://example.com/api/v1")
	}
}

func TestDo_GetSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/v1/sandboxes/sbx-1" {
			t.Errorf("path = %s, want /api/v1/sandboxes/sbx-1", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sandboxId": "sbx-1", "status": "Running"})
	}))
	defer srv.Close()

	c, err := New(Config{APIURL: srv.URL})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var out struct {
		SandboxID string `json:"sandboxId"`
		Status    string `json:"status"`
	}
	if err := c.Get(context.Background(), "/sandboxes/sbx-1", &out); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if out.SandboxID != "sbx-1" || out.Status != "Running" {
		t.Fatalf("out = %+v, want sbx-1/Running", out)
	}
}

func TestDo_PostSendsBodyAndAuth(t *testing.T) {
	var gotAPIKey, gotContentType string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-API-Key")
		gotContentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"sandboxId": "sbx-2"})
	}))
	defer srv.Close()

	c, err := New(Config{APIURL: srv.URL, APIKey: "secret-key"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var out map[string]string
	body := map[string]string{"name": "demo"}
	if err := c.Post(context.Background(), "/sandboxes", body, &out); err != nil {
		t.Fatalf("Post() error = %v", err)
	}
	if gotAPIKey != "secret-key" {
		t.Errorf("X-API-Key = %q, want secret-key", gotAPIKey)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotBody["name"] != "demo" {
		t.Errorf("request body name = %q, want demo", gotBody["name"])
	}
	if out["sandboxId"] != "sbx-2" {
		t.Errorf("response sandboxId = %q, want sbx-2", out["sandboxId"])
	}
}

func TestDo_BearerTokenUsedWhenNoAPIKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, err := New(Config{APIURL: srv.URL, BearerToken: "jwt-token"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := c.Get(context.Background(), "/user/me", nil); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if gotAuth != "Bearer jwt-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer jwt-token")
	}
}

func TestDo_APIKeyTakesPrecedenceOverBearer(t *testing.T) {
	var gotAPIKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("X-API-Key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, err := New(Config{APIURL: srv.URL, APIKey: "the-key", BearerToken: "the-token"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := c.Get(context.Background(), "/user/me", nil); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if gotAPIKey != "the-key" {
		t.Errorf("X-API-Key = %q, want the-key", gotAPIKey)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty (API key should win)", gotAuth)
	}
}

func TestDo_ErrorDecodesStructuredBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":      "policy_violation",
			"violations": []string{"maxCpu exceeded"},
		})
	}))
	defer srv.Close()

	c, err := New(Config{APIURL: srv.URL})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	err = c.Get(context.Background(), "/sandboxes/sbx-3", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d, want 403", apiErr.StatusCode)
	}
	if apiErr.Message != "policy_violation" {
		t.Errorf("Message = %q, want policy_violation", apiErr.Message)
	}
	if apiErr.Body["error"] != "policy_violation" {
		t.Errorf("Body[error] = %v, want policy_violation", apiErr.Body["error"])
	}
}

func TestDo_ErrorFallsBackToRawTextWhenNotJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal error, try again"))
	}))
	defer srv.Close()

	c, err := New(Config{APIURL: srv.URL})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	err = c.Get(context.Background(), "/sandboxes/sbx-4", nil)
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Message != "internal error, try again" {
		t.Errorf("Message = %q, want raw body text", apiErr.Message)
	}
}

func TestDo_DeprecationWarningLoggedOncePerEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Deprecation", "true")
		w.Header().Set("Sunset", "Fri, 01 Jan 2027 00:00:00 GMT")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var buf syncBuffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	c, err := New(Config{APIURL: srv.URL, Logger: logger})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := c.Get(context.Background(), "/sandboxes", nil); err != nil {
			t.Fatalf("Get() error = %v", err)
		}
	}
	out := buf.String()
	count := 0
	for _, line := range splitLines(out) {
		if line != "" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 deprecation warning log line, got %d: %s", count, out)
	}
}

func TestDo_PatchAndDelete(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, err := New(Config{APIURL: srv.URL})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := c.Patch(context.Background(), "/sandboxes/sbx-5", map[string]string{"idleTimeout": "30m"}, nil); err != nil {
		t.Fatalf("Patch() error = %v", err)
	}
	if err := c.Delete(context.Background(), "/sandboxes/sbx-5", nil); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if len(methods) != 2 || methods[0] != http.MethodPatch || methods[1] != http.MethodDelete {
		t.Fatalf("methods = %v, want [PATCH DELETE]", methods)
	}
}

// syncBuffer is a minimal io.Writer collecting output for log assertions;
// tests run sequentially against one client so no locking is needed.
type syncBuffer struct {
	data []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	return string(b.data)
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}
