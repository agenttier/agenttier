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
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/agenttier/agenttier/pkg/agenttierclient"
)

// runSandboxBulkCreate implements `agenttier sandbox bulk-create` (FR4),
// mirroring the Python CLI's cmd_sandbox_bulk_create: reads a JSON array of
// create specs from --file ("-" for stdin) and posts them in one call.
func runSandboxBulkCreate(args []string) int {
	fs := flag.NewFlagSet("sandbox bulk-create", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var file string
	fs.StringVar(&file, "file", "", `JSON array of create specs; "-" reads stdin (required).`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if file == "" {
		fmt.Fprintln(os.Stderr, "sandbox bulk-create requires --file")
		return 2
	}
	raw, err := readBulkCreateInput(file)
	if err != nil {
		return errExit("sandbox bulk-create", err)
	}
	var items []agenttierclient.CreateSandboxRequest
	if err := json.Unmarshal(raw, &items); err != nil {
		fmt.Fprintln(os.Stderr, "sandbox bulk-create: input must be a JSON array of sandbox specs")
		return 2
	}
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr, "sandbox bulk-create: input must be a non-empty JSON array of sandbox specs")
		return 2
	}
	// Same default as runSandboxCreate/the Python SDK's create_bulk: an item
	// whose templateRef omits "kind" (the natural thing for an unaware
	// caller to write) would otherwise hit the controller's default of the
	// NAMESPACED SandboxTemplate kind and 404, since every built-in
	// template ships as a ClusterSandboxTemplate only. Only fills the gap
	// when a caller's JSON left it empty — an explicit "kind" in the input
	// (e.g. a deliberate SandboxTemplate reference) is never overridden.
	for i := range items {
		if items[i].TemplateRef != nil && items[i].TemplateRef.Kind == "" {
			items[i].TemplateRef.Kind = "ClusterSandboxTemplate"
		}
	}
	if err := cfg.resolve(); err != nil {
		return errExit("sandbox bulk-create", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("sandbox bulk-create", err)
	}
	result, err := c.BulkCreateSandboxes(context.Background(), items)
	if err != nil {
		return errExit("sandbox bulk-create", err)
	}
	if cfg.output == "json" {
		printJSON(result.Results)
		return 0
	}
	rows := make([][]string, 0, len(result.Results))
	for _, r := range result.Results {
		rows = append(rows, []string{fmt.Sprint(r.Index), r.Status, r.SandboxID, r.Error})
	}
	printTable([]string{"INDEX", "STATUS", "SANDBOX_ID", "ERROR"}, rows)
	return 0
}

// readBulkCreateInput reads the --file argument's contents: "-" means stdin,
// anything else is a local path.
func readBulkCreateInput(file string) ([]byte, error) {
	if file == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(file) // #nosec G304 -- file is the --file flag the caller explicitly asked to read
}

// bulkActionValidChoices mirrors handleBulkAction's own validation
// (pkg/router/bulk_handlers.go) so a bad --action is rejected locally
// before the network round-trip (FR1.10).
var bulkActionValidChoices = map[string]bool{"stop": true, "resume": true, "delete": true}

// runSandboxBulkAction implements `agenttier sandbox bulk-action` (FR4):
// apply stop/resume/delete to a list of sandbox IDs in one call.
func runSandboxBulkAction(args []string) int {
	fs := flag.NewFlagSet("sandbox bulk-action", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var action string
	fs.StringVar(&action, "action", "", `Action to apply: "stop", "resume", or "delete" (required).`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if action == "" {
		fmt.Fprintln(os.Stderr, "sandbox bulk-action requires --action")
		return 2
	}
	if !bulkActionValidChoices[action] {
		fmt.Fprintln(os.Stderr, `sandbox bulk-action: --action must be one of "stop", "resume", "delete"`)
		return 2
	}
	ids := fs.Args()
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "sandbox bulk-action requires one or more <sandbox-id> arguments")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("sandbox bulk-action", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("sandbox bulk-action", err)
	}
	result, err := c.BulkAction(context.Background(), action, ids)
	if err != nil {
		return errExit("sandbox bulk-action", err)
	}
	if cfg.output == "json" {
		printJSON(result.Results)
		return 0
	}
	rows := make([][]string, 0, len(result.Results))
	for _, r := range result.Results {
		rows = append(rows, []string{r.ID, r.Status, r.Error})
	}
	printTable([]string{"ID", "STATUS", "ERROR"}, rows)
	return 0
}
