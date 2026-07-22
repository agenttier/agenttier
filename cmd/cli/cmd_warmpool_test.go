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

func TestRunWarmPoolStatusAndSetConfig(t *testing.T) {
	var gotBody map[string]any
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"pools": []map[string]any{{"template": "t1", "desiredCount": 2, "readyCount": 2}},
			})
		case http.MethodPut:
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "updated", "pools": gotBody["pools"]})
		}
	})
	out, code := captureStdout(t, func() int { return runWarmPoolStatus(baseArgs) })
	if code != 0 || !strings.Contains(out, "t1") {
		t.Fatalf("status: code=%d out=%q", code, out)
	}
	code = runWarmPoolSetConfig(append(baseArgs, "--template", "t1", "--desired-count", "3"))
	if code != 0 {
		t.Fatalf("set-config: code=%d", code)
	}
}

func TestRunWarmPoolSetConfig_ValidatesDesiredCount(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called with an invalid desired-count")
	})
	code := runWarmPoolSetConfig(append(baseArgs, "--template", "t1", "--desired-count", "99"))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
