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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTempBulkCreateFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "specs.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

func TestRunSandboxBulkCreate(t *testing.T) {
	var gotBody map[string]any
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/sandboxes/bulk") {
			t.Errorf("method/path = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 0, "status": "created", "sandboxId": "a"},
				{"index": 1, "status": "error", "error": "template not found"},
			},
		})
	})
	file := writeTempBulkCreateFile(t, `[{"name":"a","templateRef":{"name":"general-coding"}},{"name":"b","templateRef":{"name":"bogus"}}]`)
	out, code := captureStdout(t, func() int {
		return runSandboxBulkCreate(append(baseArgs, "--file", file))
	})
	if code != 0 {
		t.Fatalf("exit code = %d, output=%s", code, out)
	}
	items, _ := gotBody["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("gotBody[items] = %v, want 2 entries", gotBody["items"])
	}
	if !strings.Contains(out, "created") || !strings.Contains(out, "a") {
		t.Errorf("output = %q, want it to mention created/a", out)
	}
	if !strings.Contains(out, "error") || !strings.Contains(out, "template not found") {
		t.Errorf("output = %q, want it to mention the per-item error", out)
	}
}

// TestRunSandboxBulkCreate_DefaultsMissingTemplateKind regresses the same
// class of bug as runSandboxCreate's: a caller's JSON item that omits
// templateRef.kind (the natural thing to write) must not 404 against the
// controller's default of the namespaced SandboxTemplate — every built-in
// template ships as a ClusterSandboxTemplate only.
func TestRunSandboxBulkCreate_DefaultsMissingTemplateKind(t *testing.T) {
	var gotBody map[string]any
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"index": 0, "status": "created", "sandboxId": "a"}},
		})
	})
	file := writeTempBulkCreateFile(t, `[{"name":"a","templateRef":{"name":"general-coding"}}]`)
	_, code := captureStdout(t, func() int {
		return runSandboxBulkCreate(append(baseArgs, "--file", file))
	})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	items, _ := gotBody["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("gotBody[items] = %v, want 1 entry", gotBody["items"])
	}
	item, _ := items[0].(map[string]any)
	templateRef, _ := item["templateRef"].(map[string]any)
	if templateRef["kind"] != "ClusterSandboxTemplate" {
		t.Errorf("templateRef[kind] = %v, want ClusterSandboxTemplate to be defaulted in", templateRef["kind"])
	}
}

// TestRunSandboxBulkCreate_PreservesExplicitTemplateKind confirms the
// defaulting above never clobbers a caller's deliberate choice — an item
// that explicitly asks for the namespaced SandboxTemplate kind must reach
// the Router unchanged.
func TestRunSandboxBulkCreate_PreservesExplicitTemplateKind(t *testing.T) {
	var gotBody map[string]any
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"index": 0, "status": "created", "sandboxId": "a"}},
		})
	})
	file := writeTempBulkCreateFile(t, `[{"name":"a","templateRef":{"name":"custom","kind":"SandboxTemplate"}}]`)
	_, code := captureStdout(t, func() int {
		return runSandboxBulkCreate(append(baseArgs, "--file", file))
	})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	items, _ := gotBody["items"].([]any)
	item, _ := items[0].(map[string]any)
	templateRef, _ := item["templateRef"].(map[string]any)
	if templateRef["kind"] != "SandboxTemplate" {
		t.Errorf("templateRef[kind] = %v, want the explicit SandboxTemplate preserved, not overridden", templateRef["kind"])
	}
}

func TestRunSandboxBulkCreate_JSONOutput(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"index": 0, "status": "created", "sandboxId": "a"}},
		})
	})
	file := writeTempBulkCreateFile(t, `[{"name":"a","templateRef":{"name":"general-coding"}}]`)
	out, code := captureStdout(t, func() int {
		return runSandboxBulkCreate(append(baseArgs, "--file", file, "--output", "json"))
	})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	var decoded []map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output isn't valid JSON: %v\n%s", err, out)
	}
	if len(decoded) != 1 || decoded[0]["sandboxId"] != "a" {
		t.Errorf("decoded = %+v", decoded)
	}
}

func TestRunSandboxBulkCreate_RequiresFile(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called without --file")
	})
	code := runSandboxBulkCreate(baseArgs)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunSandboxBulkCreate_RejectsNonArrayJSON(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for malformed input")
	})
	file := writeTempBulkCreateFile(t, `{"name":"a","templateRef":{"name":"t"}}`)
	code := runSandboxBulkCreate(append(baseArgs, "--file", file))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunSandboxBulkCreate_RejectsEmptyArray(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for an empty array")
	})
	file := writeTempBulkCreateFile(t, `[]`)
	code := runSandboxBulkCreate(append(baseArgs, "--file", file))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunSandboxBulkAction(t *testing.T) {
	var gotBody map[string]any
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/sandboxes/bulk-action") {
			t.Errorf("method/path = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"id": "a", "status": "stopped"},
				{"id": "missing", "status": "error", "error": "sandbox not found"},
			},
		})
	})
	out, code := captureStdout(t, func() int {
		return runSandboxBulkAction(append(baseArgs, "--action", "stop", "a", "missing"))
	})
	if code != 0 {
		t.Fatalf("exit code = %d, output=%s", code, out)
	}
	if gotBody["action"] != "stop" {
		t.Errorf("gotBody[action] = %v", gotBody["action"])
	}
	ids, _ := gotBody["ids"].([]any)
	if len(ids) != 2 {
		t.Errorf("gotBody[ids] = %v, want 2 entries", gotBody["ids"])
	}
	if !strings.Contains(out, "stopped") || !strings.Contains(out, "sandbox not found") {
		t.Errorf("output = %q", out)
	}
}

func TestRunSandboxBulkAction_RequiresAction(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called without --action")
	})
	code := runSandboxBulkAction(append(baseArgs, "a"))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunSandboxBulkAction_RejectsInvalidAction(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called for an invalid --action")
	})
	code := runSandboxBulkAction(append(baseArgs, "--action", "reboot", "a"))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunSandboxBulkAction_RequiresIDs(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be called without sandbox IDs")
	})
	code := runSandboxBulkAction(append(baseArgs, "--action", "stop"))
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

// TestRunSandbox_DispatchesBulkSubcommands confirms `sandbox bulk-create`/
// `sandbox bulk-action` are wired into the top-level dispatcher, not just
// callable directly — this is the actual FR1.9 parity gap that was found
// (the functions existed nowhere, so this dispatch didn't exist either).
func TestRunSandbox_DispatchesBulkSubcommands(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sandboxes/bulk"):
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{{"index": 0, "status": "created", "sandboxId": "a"}}})
		case strings.HasSuffix(r.URL.Path, "/sandboxes/bulk-action"):
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{{"id": "a", "status": "stopped"}}})
		}
	})
	file := writeTempBulkCreateFile(t, `[{"name":"a","templateRef":{"name":"t"}}]`)
	if code := runSandbox(append([]string{"bulk-create", "--file", file}, baseArgs...)); code != 0 {
		t.Errorf("sandbox bulk-create via dispatch: code = %d", code)
	}
	if code := runSandbox(append([]string{"bulk-action", "--action", "stop"}, append(baseArgs, "a")...)); code != 0 {
		t.Errorf("sandbox bulk-action via dispatch: code = %d", code)
	}
}
