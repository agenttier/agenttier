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

func TestRunTemplateListGet(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/templates"):
			_ = json.NewEncoder(w).Encode(map[string]any{"templates": []map[string]any{{"name": "t1", "image": "img"}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "t1", "description": "desc"})
		}
	})
	out, code := captureStdout(t, func() int { return runTemplateList(baseArgs) })
	if code != 0 || !strings.Contains(out, "t1") {
		t.Fatalf("list: code=%d out=%q", code, out)
	}
	out, code = captureStdout(t, func() int { return runTemplateGet(append(baseArgs, "t1")) })
	if code != 0 || !strings.Contains(out, "desc") {
		t.Fatalf("get: code=%d out=%q", code, out)
	}
}
