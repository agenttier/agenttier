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
	"strings"

	"github.com/agenttier/agenttier/pkg/agenttierclient"
)

// runAPIKeys dispatches `agenttier apikeys <subcommand> ...` — FR1.5 + FR6
// (the --sandbox-id/--action-groups flags on create mint a sandbox-scoped
// key per design.md#FR6 instead of a full-access user-level one).
func runAPIKeys(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "apikeys requires a subcommand: list|create|revoke")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runAPIKeysList(rest)
	case "create":
		return runAPIKeysCreate(rest)
	case "revoke":
		return runAPIKeysRevoke(rest)
	default:
		fmt.Fprintf(os.Stderr, "apikeys: unknown subcommand %q\n", sub)
		return 2
	}
}

func runAPIKeysList(args []string) int {
	fs := flag.NewFlagSet("apikeys list", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("apikeys list", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("apikeys list", err)
	}
	keys, err := c.ListAPIKeys(context.Background())
	if err != nil {
		return errExit("apikeys list", err)
	}
	if cfg.output == "json" {
		printJSON(keys)
		return 0
	}
	rows := make([][]string, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, []string{k.ID, dashIfEmpty(k.Name), dashIfEmpty(k.SandboxID), dashIfEmpty(k.CreatedAt)})
	}
	printTable([]string{"ID", "NAME", "SANDBOX", "CREATED"}, rows)
	return 0
}

func runAPIKeysCreate(args []string) int {
	fs := flag.NewFlagSet("apikeys create", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var name, expiresIn, sandboxID, actionGroups string
	fs.StringVar(&name, "name", "", "Human-readable label for the key.")
	fs.StringVar(&expiresIn, "expires-in", "", `Key lifetime as a Go duration (e.g. "720h").`)
	fs.StringVar(&sandboxID, "sandbox-id", "", "Bind the key to this sandbox (FR6 scoped key). Omit for a full-access user-level key.")
	fs.StringVar(&actionGroups, "action-groups", "", `Comma-separated action groups for a scoped key (e.g. "run-command,files:read"). Requires --sandbox-id.`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if actionGroups != "" && sandboxID == "" {
		fmt.Fprintln(os.Stderr, "apikeys create: --action-groups requires --sandbox-id")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("apikeys create", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("apikeys create", err)
	}
	req := agenttierclient.CreateAPIKeyRequest{
		Name:      name,
		ExpiresIn: expiresIn,
		SandboxID: sandboxID,
	}
	if actionGroups != "" {
		req.ActionGroups = strings.Split(actionGroups, ",")
	}
	result, err := c.CreateAPIKey(context.Background(), req)
	if err != nil {
		return errExit("apikeys create", err)
	}
	if cfg.output == "json" {
		printJSON(result)
		return 0
	}
	fmt.Printf("key: %s\n", result.Key)
	fmt.Println("Store this key now — it cannot be retrieved again.")
	return 0
}

func runAPIKeysRevoke(args []string) int {
	fs := flag.NewFlagSet("apikeys revoke", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "apikeys revoke requires <key-id>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("apikeys revoke", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("apikeys revoke", err)
	}
	if err := c.RevokeAPIKey(context.Background(), fs.Arg(0)); err != nil {
		return errExit("apikeys revoke", err)
	}
	fmt.Printf("revoked %s\n", fs.Arg(0))
	return 0
}
