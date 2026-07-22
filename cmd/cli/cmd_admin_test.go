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

func TestRunAdminSandboxesAndSharing(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sandboxes"):
			_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": []any{}})
		case strings.HasSuffix(r.URL.Path, "/sharing"):
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	})
	if code := runAdminSandboxes(baseArgs); code != 0 {
		t.Errorf("sandboxes: code=%d", code)
	}
	if code := runAdminSharing(baseArgs); code != 0 {
		t.Errorf("sharing: code=%d", code)
	}
}
