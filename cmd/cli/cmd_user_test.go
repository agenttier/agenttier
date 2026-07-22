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

func TestRunUserWhoami(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": "user-1", "email": "user@example.com", "isAdmin": true})
	})
	out, code := captureStdout(t, func() int { return runUserWhoami(baseArgs) })
	if code != 0 || !strings.Contains(out, "user@example.com") || !strings.Contains(out, "admin") {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestRunUserPreferencesGetSet(t *testing.T) {
	var gotBody map[string]any
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"theme": "dark"})
		case http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(gotBody)
		}
	})
	out, code := captureStdout(t, func() int { return runUserPreferencesGet(baseArgs) })
	if code != 0 || !strings.Contains(out, "dark") {
		t.Fatalf("get: code=%d out=%q", code, out)
	}
	code = runUserPreferencesSet(append(baseArgs, "--json", `{"theme":"light"}`))
	if code != 0 {
		t.Fatalf("set: code=%d", code)
	}
	if gotBody["theme"] != "light" {
		t.Errorf("gotBody = %v", gotBody)
	}
}

func TestRunUserPreferencesSet_RejectsInvalidJSON(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called with invalid JSON")
	})
	code := runUserPreferencesSet(append(baseArgs, "--json", "{not json"))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
