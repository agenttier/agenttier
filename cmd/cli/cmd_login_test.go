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
	"path/filepath"
	"strings"
	"testing"
)

func TestRunLogin_SavesConfigAndVerifies(t *testing.T) {
	t.Setenv("AGENTTIER_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": "user-1", "email": "user@example.com"})
	})
	args := append(baseArgs, "--api-key", "test-key")
	out, code := captureStdout(t, func() int { return runLogin(args) })
	if code != 0 {
		t.Fatalf("exit code = %d, out=%q", code, out)
	}
	if !strings.Contains(out, "Saved config") || !strings.Contains(out, "user@example.com") {
		t.Errorf("output = %q", out)
	}
	loaded := loadSavedConfig()
	if loaded.APIKey != "test-key" {
		t.Errorf("loaded.APIKey = %q, want test-key", loaded.APIKey)
	}
}

func TestRunLogin_RequiresAPIURL(t *testing.T) {
	t.Setenv("AGENTTIER_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	code := runLogin([]string{})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
