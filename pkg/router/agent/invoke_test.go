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

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agenttierv1alpha1 "github.com/agenttier/agenttier/api/v1alpha1"
)

// blockingBridge waits on a signal before returning so we can drive the
// concurrency / cancel flows deterministically.
type blockingBridge struct {
	mu      sync.Mutex
	calls   int
	release chan struct{} // closed to let in-flight execs return
	exit    int
	stdout  string
}

func newBlockingBridge() *blockingBridge {
	return &blockingBridge{release: make(chan struct{})}
}

func (b *blockingBridge) ExecCommandStream(ctx context.Context, _, _, _ string, _ []string, stdout, stderr io.Writer) (int, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()

	if b.stdout != "" {
		_, _ = stdout.Write([]byte(b.stdout))
	}

	select {
	case <-ctx.Done():
		return -1, ctx.Err()
	case <-b.release:
		return b.exit, nil
	}
}

// ExecCommandStreamWithStdin proxies through after draining stdin so the
// bridge interface is satisfied. Drain happens before block so the SPDY
// emulation completes input transfer like a real bridge would.
func (b *blockingBridge) ExecCommandStreamWithStdin(ctx context.Context, ns, pod, container string, command []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if stdin != nil {
		_, _ = io.Copy(io.Discard, stdin)
	}
	return b.ExecCommandStream(ctx, ns, pod, container, command, stdout, stderr)
}

func newConfiguredAgentSandbox() *agenttierv1alpha1.Sandbox {
	sb := newAgentSandbox()
	sb.Status.AgentConfigure = &agenttierv1alpha1.AgentConfigureStatus{
		LastConfiguredAt:   &metav1.Time{Time: time.Now()},
		InstallCommandHash: "abc",
		Entrypoint:         []string{"python", "/workspace/agent.py"},
	}
	return sb
}

func doInvoke(h *Handler, body string, query string) *httptest.ResponseRecorder {
	url := "/api/v1/sandboxes/sbx-agent/invoke"
	if query != "" {
		url += "?" + query
	}
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()
	h.RegisterRoutes(api)
	r.ServeHTTP(rec, req)
	return rec
}

func TestInvoke_RequiresConfigure(t *testing.T) {
	sb := newAgentSandbox()
	// no AgentConfigure set
	bridge := newBlockingBridge()
	close(bridge.release) // bridge would return immediately, but we shouldn't reach it
	h, _ := buildHandler(t, sb, &stubBridge{})

	rec := doInvoke(h, "", "")
	if rec.Code != http.StatusFailedDependency {
		t.Fatalf("expected 424 FailedDependency, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "configure") {
		t.Errorf("expected error to mention /configure, got %q", rec.Body.String())
	}
}

func TestInvoke_RejectsCodeMode(t *testing.T) {
	sb := newConfiguredAgentSandbox()
	sb.Spec.Mode = agenttierv1alpha1.SandboxModeCode
	h, _ := buildHandler(t, sb, &stubBridge{})

	rec := doInvoke(h, "", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for code-mode sandbox, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestInvoke_StreamsStartAndExitEvents(t *testing.T) {
	sb := newConfiguredAgentSandbox()
	bridge := &stubBridge{stdout: []byte("hello world\n"), exit: 0}
	h, _ := buildHandler(t, sb, bridge)

	rec := doInvoke(h, "input bytes", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK SSE, got %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: start") {
		t.Errorf("expected start event, got %q", body)
	}
	if !strings.Contains(body, `"invokeId":"inv-`) {
		t.Errorf("expected invokeId in start event, got %q", body)
	}
	if !strings.Contains(body, "hello world") {
		t.Errorf("expected stdout streamed, got %q", body)
	}
	if !strings.Contains(body, "event: exit") {
		t.Errorf("expected exit event, got %q", body)
	}
	if !strings.Contains(body, `"exitCode":0`) {
		t.Errorf("expected exit code 0, got %q", body)
	}
	if !strings.Contains(body, `"reason":"completed"`) {
		t.Errorf("expected reason completed, got %q", body)
	}
}

// invokeEventReasons returns the Reason of every Sandbox-kind Event
// recorded against sb, in list order (the fake client does not guarantee
// creation order, so callers that care about ordering should assert
// presence/absence rather than sequence).
func invokeEventReasons(t *testing.T, c client.Client, sb *agenttierv1alpha1.Sandbox) []string {
	t.Helper()
	list := &corev1.EventList{}
	if err := c.List(context.Background(), list, client.InNamespace(sb.Namespace)); err != nil {
		t.Fatalf("list events: %v", err)
	}
	var reasons []string
	for _, evt := range list.Items {
		if evt.InvolvedObject.Kind == "Sandbox" && evt.InvolvedObject.Name == sb.Name {
			reasons = append(reasons, evt.Reason)
		}
	}
	return reasons
}

func containsReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

// TestInvoke_EmitsStartedEvent verifies FR5.2's agent.invoke.started webhook
// event source: an AgentInvokeStarted K8s Event fires as soon as the
// invoke's SSE stream begins, independent of how the invoke eventually ends.
func TestInvoke_EmitsStartedEvent(t *testing.T) {
	sb := newConfiguredAgentSandbox()
	bridge := &stubBridge{stdout: []byte("hello\n"), exit: 0}
	h, c := buildHandler(t, sb, bridge)

	rec := doInvoke(h, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK SSE, got %d: %s", rec.Code, rec.Body.String())
	}

	reasons := invokeEventReasons(t, c, sb)
	if !containsReason(reasons, "AgentInvokeStarted") {
		t.Errorf("expected an AgentInvokeStarted event, got reasons %v", reasons)
	}
}

// TestInvoke_EmitsCompletedEventOnSuccess verifies the successful-exit path
// (exitReason=="completed" && exitCode==0) records AgentInvokeCompleted, not
// the old undifferentiated "AgentInvoked" reason.
func TestInvoke_EmitsCompletedEventOnSuccess(t *testing.T) {
	sb := newConfiguredAgentSandbox()
	bridge := &stubBridge{stdout: []byte("hello\n"), exit: 0}
	h, c := buildHandler(t, sb, bridge)

	rec := doInvoke(h, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK SSE, got %d: %s", rec.Code, rec.Body.String())
	}

	reasons := invokeEventReasons(t, c, sb)
	if !containsReason(reasons, "AgentInvokeCompleted") {
		t.Errorf("expected an AgentInvokeCompleted event, got reasons %v", reasons)
	}
	if containsReason(reasons, "AgentInvokeFailed") {
		t.Errorf("did not expect an AgentInvokeFailed event on a clean exit, got reasons %v", reasons)
	}
	if containsReason(reasons, "AgentInvoked") {
		t.Errorf("expected the old undifferentiated AgentInvoked reason to be gone, got reasons %v", reasons)
	}
}

// TestInvoke_EmitsFailedEventOnNonZeroExit verifies the exitReason=="completed"
// but exitCode!=0 branch — a clean shell return of a non-zero code is still a
// FAILED invoke for FR5.2's webhook purposes, matching the pre-existing
// auditType==Warning logic this task must not change the semantics of.
func TestInvoke_EmitsFailedEventOnNonZeroExit(t *testing.T) {
	sb := newConfiguredAgentSandbox()
	bridge := &stubBridge{stdout: []byte("boom\n"), exit: 1}
	h, c := buildHandler(t, sb, bridge)

	rec := doInvoke(h, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK SSE, got %d: %s", rec.Code, rec.Body.String())
	}

	reasons := invokeEventReasons(t, c, sb)
	if !containsReason(reasons, "AgentInvokeFailed") {
		t.Errorf("expected an AgentInvokeFailed event for a non-zero exit, got reasons %v", reasons)
	}
	if containsReason(reasons, "AgentInvokeCompleted") {
		t.Errorf("did not expect an AgentInvokeCompleted event for a non-zero exit, got reasons %v", reasons)
	}
}

func TestInvoke_CancelTerminatesInFlight(t *testing.T) {
	sb := newConfiguredAgentSandbox()
	bridge := newBlockingBridge()
	h, _ := buildHandler(t, sb, bridge)

	// Kick off the invoke in a goroutine; it will block in the bridge
	// until we cancel.
	type result struct {
		rec  *httptest.ResponseRecorder
		body string
	}
	done := make(chan result, 1)
	go func() {
		rec := doInvoke(h, "", "")
		done <- result{rec: rec, body: rec.Body.String()}
	}()

	// Wait until the invoke is registered, then look up its ID.
	var invokeID string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var found string
		h.invokes.Range(func(k, _ any) bool {
			found = k.(string)
			return false
		})
		if found != "" {
			invokeID = found
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if invokeID == "" {
		t.Fatal("invoke never registered")
	}

	// Cancel.
	cancelBody, _ := json.Marshal(CancelRequest{InvokeID: invokeID})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/sbx-agent/invoke/cancel", bytes.NewReader(cancelBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()
	h.RegisterRoutes(api)
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case res := <-done:
		if !strings.Contains(res.body, `"reason":"canceled"`) {
			t.Errorf("expected reason canceled, got %q", res.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("invoke did not terminate after cancel")
	}
}

func TestInvoke_CancelOfUnknownIDReturns404(t *testing.T) {
	sb := newConfiguredAgentSandbox()
	h, _ := buildHandler(t, sb, &stubBridge{})

	cancelBody, _ := json.Marshal(CancelRequest{InvokeID: "inv-doesnotexist"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes/sbx-agent/invoke/cancel", bytes.NewReader(cancelBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r := mux.NewRouter()
	api := r.PathPrefix("/api/v1").Subrouter()
	h.RegisterRoutes(api)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown invoke, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestShellQuote_Roundtrip(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"abc", "abc"},
		{"hello world", "'hello world'"},
		{"it's", `'it'"'"'s'`},
		{"$VAR", "'$VAR'"},
	}
	for _, c := range cases {
		if got := shellQuote(c.in); got != c.want {
			t.Errorf("shellQuote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestInvoke_RespectsConcurrencyCap(t *testing.T) {
	sb := newConfiguredAgentSandbox()
	// Only 1 concurrent invoke allowed.
	sb.Status.AgentConfigure.MaxConcurrentInvokes = 1
	bridge := newBlockingBridge()
	h, _ := buildHandler(t, sb, bridge)

	// Kick off the first invoke; it'll block in the bridge.
	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- doInvoke(h, "", "") }()

	// Wait for it to register so the second call sees the in-flight count.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		count := 0
		h.invokes.Range(func(_, _ any) bool { count++; return true })
		if count == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Second invoke should hit the cap.
	rec := doInvoke(h, "", "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 on second invoke, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("expected Retry-After header on 429")
	}
	if !strings.Contains(rec.Body.String(), "concurrency_exceeded") {
		t.Errorf("expected concurrency_exceeded error code, got %q", rec.Body.String())
	}

	// Let the first invoke complete.
	close(bridge.release)
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("first invoke never returned")
	}
}
