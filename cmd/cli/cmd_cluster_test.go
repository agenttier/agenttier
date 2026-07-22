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

func TestRunClusterStatusNodesHeadroom(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			_ = json.NewEncoder(w).Encode(map[string]any{"nodes": 5, "nodesReady": 5})
		case strings.HasSuffix(r.URL.Path, "/nodes"):
			_ = json.NewEncoder(w).Encode(map[string]any{"nodes": []map[string]any{{"name": "node-1", "ready": true}}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/headroom"):
			_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "replicas": 2})
		case r.Method == http.MethodPut:
			_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "replicas": 5})
		}
	})
	out, code := captureStdout(t, func() int { return runClusterStatus(baseArgs) })
	if code != 0 || !strings.Contains(out, "nodesReady") {
		t.Fatalf("status: code=%d out=%q", code, out)
	}
	out, code = captureStdout(t, func() int { return runClusterNodes(baseArgs) })
	if code != 0 || !strings.Contains(out, "node-1") {
		t.Fatalf("nodes: code=%d out=%q", code, out)
	}
	if code := runClusterHeadroomGet(baseArgs); code != 0 {
		t.Fatalf("headroom-get: code=%d", code)
	}
	if code := runClusterHeadroomSet(append(baseArgs, "--replicas", "5")); code != 0 {
		t.Fatalf("headroom-set: code=%d", code)
	}
}

func TestRunClusterHeadroomSet_ValidatesReplicas(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called with invalid replicas")
	})
	code := runClusterHeadroomSet(append(baseArgs, "--replicas", "51"))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
