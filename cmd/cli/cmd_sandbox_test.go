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

func TestRunSandboxList(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sandboxes" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sandboxes": []map[string]any{{"sandboxId": "demo", "name": "demo", "status": "Running", "namespace": "default"}},
		})
	})
	out, code := captureStdout(t, func() int {
		return runSandboxList(baseArgs)
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out, "demo") || !strings.Contains(out, "Running") {
		t.Errorf("output = %q, want it to mention demo/Running", out)
	}
}

func TestRunSandboxList_JSONOutput(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sandboxes": []map[string]any{{"sandboxId": "demo", "status": "Running", "namespace": "default"}},
		})
	})
	args := append(baseArgs, "--output", "json")
	out, code := captureStdout(t, func() int {
		return runSandboxList(args)
	})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	var decoded []map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output isn't valid JSON: %v\n%s", err, out)
	}
	if len(decoded) != 1 || decoded[0]["sandboxId"] != "demo" {
		t.Errorf("decoded = %+v", decoded)
	}
}

func TestRunSandboxGet_RequiresArg(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called when the required arg is missing")
	})
	code := runSandboxGet(baseArgs)
	if code != 2 {
		t.Errorf("exit code = %d, want 2 (usage error)", code)
	}
}

func TestRunSandboxCreate(t *testing.T) {
	var gotBody map[string]any
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxId": "demo", "status": "Creating"})
	})
	args := append(baseArgs, "--template", "general-coding", "demo")
	out, code := captureStdout(t, func() int {
		return runSandboxCreate(args)
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0, output=%s", code, out)
	}
	if gotBody["name"] != "demo" {
		t.Errorf("gotBody[name] = %v, want demo", gotBody["name"])
	}
	templateRef, _ := gotBody["templateRef"].(map[string]any)
	if templateRef["name"] != "general-coding" {
		t.Errorf("templateRef = %v", templateRef)
	}
	// Regression guard: the controller defaults an empty templateRef.kind to
	// the NAMESPACED "SandboxTemplate", but every built-in template ships as
	// a ClusterSandboxTemplate — an unset Kind here 404s on every real
	// cluster (confirmed live via task #32's kind-cluster validation). The
	// request must always carry an explicit Kind, matching the Python SDK's
	// create_sandbox precedent.
	if templateRef["kind"] != "ClusterSandboxTemplate" {
		t.Errorf("templateRef[kind] = %v, want ClusterSandboxTemplate", templateRef["kind"])
	}
	if !strings.Contains(out, "created demo") {
		t.Errorf("output = %q", out)
	}
}

func TestRunSandboxCreate_RequiresTemplate(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called without --template")
	})
	args := append(baseArgs, "demo")
	code := runSandboxCreate(args)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunSandboxStopResumeDelete(t *testing.T) {
	var requests []string
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})
	for _, sub := range []string{"stop", "resume", "delete"} {
		args := append([]string{sub}, append(baseArgs, "demo")...)
		if code := runSandbox(args); code != 0 {
			t.Errorf("sandbox %s exit code = %d, want 0", sub, code)
		}
	}
	wantPaths := []string{
		"POST /api/v1/sandboxes/demo/stop",
		"POST /api/v1/sandboxes/demo/resume",
		"DELETE /api/v1/sandboxes/demo",
	}
	for i, want := range wantPaths {
		if i >= len(requests) || requests[i] != want {
			t.Errorf("requests[%d] = %v, want %s", i, requests, want)
		}
	}
}

func TestRunSandboxPatch(t *testing.T) {
	var gotBody map[string]any
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("method = %s, want PATCH", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sandboxId":       "demo",
			"applied":         map[string]string{"idleTimeout": "immediately"},
			"restartRequired": false,
		})
	})
	args := append(baseArgs, "--idle-timeout", "30m", "--label", "team=x", "demo")
	code := runSandboxPatch(args)
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if gotBody["idleTimeout"] != "30m" {
		t.Errorf("gotBody[idleTimeout] = %v", gotBody["idleTimeout"])
	}
	labels, _ := gotBody["labels"].(map[string]any)
	if labels["team"] != "x" {
		t.Errorf("labels = %v", labels)
	}
}

func TestRunSandboxPatch_RequiresAtLeastOneField(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called with no patch fields")
	})
	code := runSandboxPatch(append(baseArgs, "demo"))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunSandboxExec(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["command"] != "echo hi" {
			t.Errorf("command = %v, want 'echo hi'", body["command"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"stdout": "hi\n", "stderr": "", "exitCode": 0})
	})
	args := append(baseArgs, "demo", "echo", "hi")
	out, code := captureStdout(t, func() int {
		return runSandboxExec(args)
	})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if out != "hi\n" {
		t.Errorf("out = %q, want 'hi\\n'", out)
	}
}

func TestRunSandboxExec_NonZeroExitPropagates(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"stdout": "", "stderr": "boom\n", "exitCode": 7})
	})
	args := append(baseArgs, "demo", "false")
	_, code := captureStdout(t, func() int {
		return runSandboxExec(args)
	})
	if code != 7 {
		t.Errorf("exit code = %d, want 7 (propagated from command result)", code)
	}
}

func TestStringMapFlag_ParsesKeyValue(t *testing.T) {
	var m stringMapFlag
	if err := m.Set("team=platform"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if m["team"] != "platform" {
		t.Errorf("m = %+v", m)
	}
	if err := m.Set("noequals"); err == nil {
		t.Error("expected error for missing '='")
	}
}

func TestParsePortArg(t *testing.T) {
	if _, err := parsePortArg("0"); err == nil {
		t.Error("expected error for port 0")
	}
	if _, err := parsePortArg("-1"); err == nil {
		t.Error("expected error for negative port")
	}
	if _, err := parsePortArg("notanumber"); err == nil {
		t.Error("expected error for non-numeric port")
	}
	p, err := parsePortArg("8080")
	if err != nil || p != 8080 {
		t.Errorf("parsePortArg(8080) = %d, %v", p, err)
	}
}
