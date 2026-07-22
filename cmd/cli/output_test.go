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
	"strings"
	"testing"
)

func TestPrintJSON(t *testing.T) {
	out, _ := captureStdout(t, func() int {
		printJSON(map[string]string{"sandboxId": "demo"})
		return 0
	})
	var decoded map[string]string
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("output isn't valid JSON: %v\noutput: %s", err, out)
	}
	if decoded["sandboxId"] != "demo" {
		t.Errorf("decoded = %+v", decoded)
	}
}

func TestPrintTable_EmptyRows(t *testing.T) {
	out, _ := captureStdout(t, func() int {
		printTable([]string{"ID", "NAME"}, nil)
		return 0
	})
	if !strings.Contains(out, "(no results)") {
		t.Errorf("output = %q, want it to contain '(no results)'", out)
	}
}

func TestPrintTable_AlignsColumns(t *testing.T) {
	out, _ := captureStdout(t, func() int {
		printTable([]string{"ID", "NAME"}, [][]string{
			{"sbx-1", "demo"},
			{"sbx-longer-id", "x"},
		})
		return 0
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (header + 2 rows), got %d: %q", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "ID") {
		t.Errorf("header line = %q", lines[0])
	}
}

func TestDashIfEmpty(t *testing.T) {
	if got := dashIfEmpty(""); got != "-" {
		t.Errorf("dashIfEmpty(\"\") = %q, want -", got)
	}
	if got := dashIfEmpty("value"); got != "value" {
		t.Errorf("dashIfEmpty(value) = %q, want value", got)
	}
}

func TestErrExit_ReturnsOne(t *testing.T) {
	if code := errExit("op", errDummy{}); code != 1 {
		t.Errorf("errExit() = %d, want 1", code)
	}
}

type errDummy struct{}

func (errDummy) Error() string { return "dummy error" }
