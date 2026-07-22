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

func TestRunBackupsLifecycle(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"backups": []map[string]any{{"name": "snap-1", "readyToUse": true}}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/restore"):
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "restored", "clonedFrom": "demo"})
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "snap-2", "readyToUse": false})
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	})

	out, code := captureStdout(t, func() int { return runBackupsList(append(baseArgs, "demo")) })
	if code != 0 || !strings.Contains(out, "snap-1") {
		t.Fatalf("list: code=%d out=%q", code, out)
	}
	out, code = captureStdout(t, func() int { return runBackupsCreate(append(baseArgs, "demo")) })
	if code != 0 || !strings.Contains(out, "snap-2") {
		t.Fatalf("create: code=%d out=%q", code, out)
	}
	out, code = captureStdout(t, func() int { return runBackupsRestore(append(baseArgs, "demo", "snap-1")) })
	if code != 0 || !strings.Contains(out, "restored") {
		t.Fatalf("restore: code=%d out=%q", code, out)
	}
	if code := runBackupsDelete(append(baseArgs, "demo", "snap-1")); code != 0 {
		t.Fatalf("delete: code=%d", code)
	}
}

func TestRunBackupsRestore_PrunedSnapshot(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "snapshot not found"})
	})
	code := runBackupsRestore(append(baseArgs, "demo", "gone"))
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
}
