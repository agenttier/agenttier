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

func TestRunAudit(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"events": []map[string]any{{"eventType": "Created", "sandboxId": "demo"}},
		})
	})
	out, code := captureStdout(t, func() int { return runAudit(baseArgs) })
	if code != 0 || !strings.Contains(out, "Created") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestRunAudit_AdminRequired(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "admin required"})
	})
	code := runAudit(baseArgs)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}
