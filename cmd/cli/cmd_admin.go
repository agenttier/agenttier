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

// runAdmin dispatches `agenttier admin <subcommand> ...` — FR1.4 (admin-only).
func runAdmin(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "admin requires a subcommand: sandboxes|sharing")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "sandboxes":
		return runAdminSandboxes(rest)
	case "sharing":
		return runAdminSharing(rest)
	default:
		fmt.Fprintf(os.Stderr, "admin: unknown subcommand %q\n", sub)
		return 2
	}
}

func runAdminSandboxes(args []string) int {
	fs := flag.NewFlagSet("admin sandboxes", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("admin sandboxes", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("admin sandboxes", err)
	}
	out, err := c.AdminListSandboxes(context.Background())
	if err != nil {
		return errExit("admin sandboxes", err)
	}
	printJSON(out)
	return 0
}

func runAdminSharing(args []string) int {
	fs := flag.NewFlagSet("admin sharing", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("admin sharing", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("admin sharing", err)
	}
	out, err := c.AdminListSharing(context.Background())
	if err != nil {
		return errExit("admin sharing", err)
	}
	printJSON(out)
	return 0
}
