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
	"flag"
)

// runAudit dispatches `agenttier audit <subcommand> ...` — FR1.3 (admin-only).
func runAudit(args []string) int {
	fs := flag.NewFlagSet("audit list", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("audit list", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("audit list", err)
	}
	events, err := c.ListAuditEvents(context.Background())
	if err != nil {
		return errExit("audit list", err)
	}
	if cfg.output == "json" {
		printJSON(events)
		return 0
	}
	rows := make([][]string, 0, len(events))
	for _, e := range events {
		rows = append(rows, []string{e.Timestamp, e.EventType, e.SandboxID, e.Namespace})
	}
	printTable([]string{"TIMESTAMP", "EVENT", "SANDBOX", "NAMESPACE"}, rows)
	return 0
}
