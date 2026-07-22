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

func TestRunAnalyticsUsageAndCosts(t *testing.T) {
	baseArgs, _ := newFakeRouter(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/usage"):
			_ = json.NewEncoder(w).Encode(map[string]any{"total_sandboxes": 3})
		case strings.HasSuffix(r.URL.Path, "/costs"):
			_ = json.NewEncoder(w).Encode(map[string]any{"running_sandboxes": 2})
		}
	})
	out, code := captureStdout(t, func() int { return runAnalyticsUsage(baseArgs) })
	if code != 0 || !strings.Contains(out, "total_sandboxes") {
		t.Fatalf("usage: code=%d out=%q", code, out)
	}
	out, code = captureStdout(t, func() int { return runAnalyticsCosts(baseArgs) })
	if code != 0 || !strings.Contains(out, "running_sandboxes") {
		t.Fatalf("costs: code=%d out=%q", code, out)
	}
}

func TestRunAnalytics_UnknownSubcommand(t *testing.T) {
	code := runAnalytics([]string{"bogus"})
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}
