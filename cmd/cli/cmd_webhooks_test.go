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

package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestRunWebhooksLifecycle(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/webhooks"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "wh1", "secret": "shown-once", "url": "https://example.com/hook"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/webhooks"):
			_ = json.NewEncoder(w).Encode(map[string]any{"webhooks": []map[string]any{{"id": "wh1", "url": "https://example.com/hook"}}})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/deliveries"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"deliveries": []map[string]any{{"eventType": "sandbox.running", "statusCode": 200, "success": true}},
			})
		}
	})

	out, code := captureStdout(t, func() int {
		return runWebhooksCreate(append(baseArgs, "--url", "https://example.com/hook", "--event-types", "sandbox.running,sandbox.stopped"))
	})
	if code != 0 || !strings.Contains(out, "shown-once") {
		t.Fatalf("create: code=%d out=%q", code, out)
	}
	out, code = captureStdout(t, func() int { return runWebhooksList(baseArgs) })
	if code != 0 || !strings.Contains(out, "wh1") {
		t.Fatalf("list: code=%d out=%q", code, out)
	}
	out, code = captureStdout(t, func() int { return runWebhooksDeliveries(append(baseArgs, "wh1")) })
	if code != 0 || !strings.Contains(out, "sandbox.running") {
		t.Fatalf("deliveries: code=%d out=%q", code, out)
	}
	if code := runWebhooksDelete(append(baseArgs, "wh1")); code != 0 {
		t.Fatalf("delete: code=%d", code)
	}
}

func TestRunWebhooksCreate_RequiresURLAndEventTypes(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	})
	if code := runWebhooksCreate(baseArgs); code != 2 {
		t.Errorf("missing both: code = %d, want 2", code)
	}
	if code := runWebhooksCreate(append(baseArgs, "--url", "https://example.com")); code != 2 {
		t.Errorf("missing event-types: code = %d, want 2", code)
	}
}

// TestRunWebhooksCreate_RejectsNonHTTPSLocally is the FR1.10 regression test:
// a non-https:// URL must be rejected before any network call, matching the
// Python CLI/SDK's local guard clause (sharing.py's _validate_create_args
// pattern) instead of round-tripping to the Router for the same 400.
func TestRunWebhooksCreate_RejectsNonHTTPSLocally(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for a locally-rejected non-https url")
	})
	code := runWebhooksCreate(append(baseArgs, "--url", "http://example.com/hook", "--event-types", "sandbox.running"))
	if code != 2 {
		t.Errorf("code = %d, want 2 (usage error)", code)
	}
}

// TestRunWebhooksCreate_RejectsUnknownEventTypeLocally is the FR1.10
// regression test for the event-type vocabulary side of the same gap.
func TestRunWebhooksCreate_RejectsUnknownEventTypeLocally(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for a locally-rejected unknown event type")
	})
	code := runWebhooksCreate(append(baseArgs, "--url", "https://example.com/hook", "--event-types", "sandbox.exploded"))
	if code != 2 {
		t.Errorf("code = %d, want 2 (usage error)", code)
	}
}

func TestValidateWebhookCreateArgs(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		eventTypes []string
		wantErr    bool
	}{
		{"valid", "https://example.com/hook", []string{"sandbox.running"}, false},
		{"empty url", "", []string{"sandbox.running"}, true},
		{"http not https", "http://example.com/hook", []string{"sandbox.running"}, true},
		{"empty event types", "https://example.com/hook", nil, true},
		{"unknown event type", "https://example.com/hook", []string{"sandbox.exploded"}, true},
		{"mixed known and unknown", "https://example.com/hook", []string{"sandbox.running", "bogus"}, true},
		{"all fixed vocabulary", "https://example.com/hook", []string{
			"sandbox.creating", "sandbox.running", "sandbox.stopped", "sandbox.error", "sandbox.deleting",
			"backup.created", "backup.pruned", "share.granted", "share.revoked",
			"agent.invoke.started", "agent.invoke.completed", "agent.invoke.failed",
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWebhookCreateArgs(tc.url, tc.eventTypes)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateWebhookCreateArgs(%q, %v) error = %v, wantErr %v", tc.url, tc.eventTypes, err, tc.wantErr)
			}
		})
	}
}
