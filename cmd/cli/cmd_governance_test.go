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

func TestRunGovernanceListGetSetDeleteEffective(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/policies"):
			_ = json.NewEncoder(w).Encode(map[string]any{"cluster": map[string]any{"maxSandboxesTotal": 100}, "namespaces": []any{}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/effective"):
			_ = json.NewEncoder(w).Encode(map[string]any{"namespace": "default", "policy": map[string]any{"maxCpu": "4"}})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"namespace": "team-a", "policy": map[string]any{"maxCpu": "4"}})
		case r.Method == http.MethodPut:
			_ = json.NewEncoder(w).Encode(map[string]any{"namespace": "team-a", "policy": map[string]any{"maxCpu": "8"}})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	})

	out, code := captureStdout(t, func() int { return runGovernanceList(baseArgs) })
	if code != 0 || !strings.Contains(out, "maxSandboxesTotal") {
		t.Fatalf("list: code=%d out=%q", code, out)
	}
	out, code = captureStdout(t, func() int { return runGovernanceGet(append(baseArgs, "team-a")) })
	if code != 0 || !strings.Contains(out, "maxCpu") {
		t.Fatalf("get: code=%d out=%q", code, out)
	}
	out, code = captureStdout(t, func() int {
		return runGovernanceSet(append(baseArgs, "--namespace", "team-a", "--max-cpu", "8"))
	})
	if code != 0 || !strings.Contains(out, "8") {
		t.Fatalf("set: code=%d out=%q", code, out)
	}
	if code := runGovernanceDelete(append(baseArgs, "team-a")); code != 0 {
		t.Fatalf("delete: code=%d", code)
	}
	out, code = captureStdout(t, func() int { return runGovernanceEffective(baseArgs) })
	if code != 0 || !strings.Contains(out, "maxCpu") {
		t.Fatalf("effective: code=%d out=%q", code, out)
	}
}
