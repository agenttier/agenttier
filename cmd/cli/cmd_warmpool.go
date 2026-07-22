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
	"fmt"
	"os"

	"github.com/agenttier/agenttier/pkg/agenttierclient"
)

// runWarmPool dispatches `agenttier warmpool <subcommand> ...` — FR1.6.
func runWarmPool(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "warmpool requires a subcommand: status|set-config")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "status":
		return runWarmPoolStatus(rest)
	case "set-config":
		return runWarmPoolSetConfig(rest)
	default:
		fmt.Fprintf(os.Stderr, "warmpool: unknown subcommand %q\n", sub)
		return 2
	}
}

func runWarmPoolStatus(args []string) int {
	fs := flag.NewFlagSet("warmpool status", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("warmpool status", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("warmpool status", err)
	}
	status, err := c.GetWarmPoolStatus(context.Background())
	if err != nil {
		return errExit("warmpool status", err)
	}
	if cfg.output == "json" {
		printJSON(status)
		return 0
	}
	rows := make([][]string, 0, len(status.Pools))
	for _, p := range status.Pools {
		rows = append(rows, []string{p.Template, fmt.Sprint(p.DesiredCount), fmt.Sprint(p.ReadyCount), fmt.Sprint(p.PendingCount)})
	}
	printTable([]string{"TEMPLATE", "DESIRED", "READY", "PENDING"}, rows)
	return 0
}

func runWarmPoolSetConfig(args []string) int {
	fs := flag.NewFlagSet("warmpool set-config", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var template string
	var desiredCount int
	fs.StringVar(&template, "template", "", "ClusterSandboxTemplate to warm (required).")
	fs.IntVar(&desiredCount, "desired-count", 0, "Idle pods to keep ready (0-10).")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if template == "" {
		fmt.Fprintln(os.Stderr, "warmpool set-config requires --template")
		return 2
	}
	if desiredCount < 0 || desiredCount > 10 {
		fmt.Fprintln(os.Stderr, "warmpool set-config: --desired-count must be 0-10")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("warmpool set-config", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("warmpool set-config", err)
	}
	result, err := c.SetWarmPoolConfig(context.Background(), agenttierclient.WarmPoolConfig{
		Pools: []agenttierclient.WarmPoolEntry{{Template: template, DesiredCount: desiredCount}},
	})
	if err != nil {
		return errExit("warmpool set-config", err)
	}
	printJSON(result)
	return 0
}
