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

func TestRunSharingListGrantRevoke(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"users": []any{}, "groups": []any{}, "shareLinks": []any{}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/share"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"users": []map[string]any{{"identity": "alice@example.com", "level": "viewer"}},
			})
		case r.Method == http.MethodDelete:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "revoked"})
		}
	})

	if code := runSharingList(append(baseArgs, "demo")); code != 0 {
		t.Fatalf("list code=%d", code)
	}
	out, code := captureStdout(t, func() int {
		return runSharingGrant(append(baseArgs, "demo", "alice@example.com"))
	})
	if code != 0 || !strings.Contains(out, "granted") {
		t.Fatalf("grant: code=%d out=%q", code, out)
	}
	if code := runSharingRevoke(append(baseArgs, "demo", "alice@example.com")); code != 0 {
		t.Fatalf("revoke code=%d", code)
	}
}

func TestRunSharingGrant_RejectsBadLevel(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called with an invalid level")
	})
	code := runSharingGrant(append(baseArgs, "--level", "owner", "demo", "alice@example.com"))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunSharingCreateLink(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "link1", "token": "raw-token", "level": "viewer"})
	})
	out, code := captureStdout(t, func() int {
		return runSharingCreateLink(append(baseArgs, "demo"))
	})
	if code != 0 || !strings.Contains(out, "raw-token") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}
