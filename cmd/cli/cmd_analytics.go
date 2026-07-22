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
)

// runAnalytics dispatches `agenttier analytics <subcommand> ...` — FR1.3
// (admin-only).
func runAnalytics(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "analytics requires a subcommand: usage|costs")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "usage":
		return runAnalyticsUsage(rest)
	case "costs":
		return runAnalyticsCosts(rest)
	default:
		fmt.Fprintf(os.Stderr, "analytics: unknown subcommand %q\n", sub)
		return 2
	}
}

func runAnalyticsUsage(args []string) int {
	fs := flag.NewFlagSet("analytics usage", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("analytics usage", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("analytics usage", err)
	}
	usage, err := c.GetUsageAnalytics(context.Background())
	if err != nil {
		return errExit("analytics usage", err)
	}
	printJSON(usage)
	return 0
}

func runAnalyticsCosts(args []string) int {
	fs := flag.NewFlagSet("analytics costs", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("analytics costs", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("analytics costs", err)
	}
	costs, err := c.GetCostEstimates(context.Background())
	if err != nil {
		return errExit("analytics costs", err)
	}
	printJSON(costs)
	return 0
}
