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

func TestRunAPIKeysListCreateRevoke(t *testing.T) {
	var gotBody map[string]any
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{"id": "key1", "name": "ci"}}})
		case http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "key2", "key": "atk_shown-once"})
		case http.MethodDelete:
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "revoked"})
		}
	})

	out, code := captureStdout(t, func() int { return runAPIKeysList(baseArgs) })
	if code != 0 || !strings.Contains(out, "key1") {
		t.Fatalf("list: code=%d out=%q", code, out)
	}
	out, code = captureStdout(t, func() int {
		return runAPIKeysCreate(append(baseArgs, "--sandbox-id", "sbx-1", "--action-groups", "run-command,files:read"))
	})
	if code != 0 || !strings.Contains(out, "atk_shown-once") {
		t.Fatalf("create: code=%d out=%q", code, out)
	}
	if gotBody["sandboxId"] != "sbx-1" {
		t.Errorf("gotBody[sandboxId] = %v", gotBody["sandboxId"])
	}
	groups, _ := gotBody["actionGroups"].([]any)
	if len(groups) != 2 {
		t.Errorf("gotBody[actionGroups] = %v", gotBody["actionGroups"])
	}
	if code := runAPIKeysRevoke(append(baseArgs, "key1")); code != 0 {
		t.Fatalf("revoke: code=%d", code)
	}
}

func TestRunAPIKeysCreate_ActionGroupsRequireSandboxID(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called")
	})
	code := runAPIKeysCreate(append(baseArgs, "--action-groups", "run-command"))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
