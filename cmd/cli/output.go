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
	"fmt"
	"os"
	"strings"
)

// printJSON pretty-prints data to stdout. Mirrors the Python CLI's
// _print_json (python-sdk/src/agenttier/cli.py).
func printJSON(data any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(data)
}

// printTable renders a simple aligned text table to stdout, mirroring the
// Python CLI's _print_table. headers and each row must have the same
// column count.
func printTable(headers []string, rows [][]string) {
	if len(rows) == 0 {
		fmt.Println(strings.Join(headers, "  ") + "\n(no results)")
		return
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	printRow := func(cells []string) {
		var sb strings.Builder
		for i, cell := range cells {
			if i > 0 {
				sb.WriteString("  ")
			}
			sb.WriteString(cell)
			if i < len(widths)-1 {
				sb.WriteString(strings.Repeat(" ", widths[i]-len(cell)))
			}
		}
		fmt.Println(strings.TrimRight(sb.String(), " "))
	}
	printRow(headers)
	for _, row := range rows {
		printRow(row)
	}
}

// errExit prints an error to stderr in the `agenttier: <op>: <err>` shape
// used throughout the legacy commands (cmd/cli/main.go's printErrorResponse)
// and returns the process exit code callers should use (always 1 — 2 is
// reserved for usage errors, matching the legacy commands' convention).
func errExit(op string, err error) int {
	fmt.Fprintf(os.Stderr, "agenttier: %s: %v\n", op, err)
	return 1
}

// dashIfEmpty returns "-" for an empty string, else s — used to keep table
// columns from collapsing to nothing when a field is unset.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
