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

func TestRunPortsListForwardRemove(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"ports": []map[string]any{{"port": 8080, "protocol": "http"}}})
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"port": 9090, "protocol": "http", "previewUrl": "https://preview.example.com"})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	})

	out, code := captureStdout(t, func() int { return runPortsList(append(baseArgs, "demo")) })
	if code != 0 || !strings.Contains(out, "8080") {
		t.Fatalf("list: code=%d out=%q", code, out)
	}

	out, code = captureStdout(t, func() int {
		return runPortsForward(append(baseArgs, "--port", "9090", "demo"))
	})
	if code != 0 || !strings.Contains(out, "preview.example.com") {
		t.Fatalf("forward: code=%d out=%q", code, out)
	}

	if code := runPortsRemove(append(baseArgs, "--port", "9090", "demo")); code != 0 {
		t.Fatalf("remove: code=%d", code)
	}
}

func TestRunPortsForward_RequiresPositivePort(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called with an invalid port")
	})
	code := runPortsForward(append(baseArgs, "--port", "0", "demo"))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
