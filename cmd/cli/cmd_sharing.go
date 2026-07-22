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

// runSharing dispatches `agenttier sandbox sharing <subcommand> ...` — FR1.1.
func runSharing(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "sharing requires a subcommand: list|grant|revoke|create-link")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runSharingList(rest)
	case "grant":
		return runSharingGrant(rest)
	case "revoke":
		return runSharingRevoke(rest)
	case "create-link":
		return runSharingCreateLink(rest)
	default:
		fmt.Fprintf(os.Stderr, "sharing: unknown subcommand %q\n", sub)
		return 2
	}
}

func runSharingList(args []string) int {
	fs := flag.NewFlagSet("sharing list", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "sharing list requires <sandbox-id>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("sharing list", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("sharing list", err)
	}
	info, err := c.GetSharing(context.Background(), fs.Arg(0))
	if err != nil {
		return errExit("sharing list", err)
	}
	if cfg.output == "json" {
		printJSON(info)
		return 0
	}
	rows := make([][]string, 0, len(info.Users)+len(info.Groups))
	for _, u := range info.Users {
		rows = append(rows, []string{u.Identity, "user", u.Level})
	}
	for _, g := range info.Groups {
		rows = append(rows, []string{g.Identity, "group", g.Level})
	}
	printTable([]string{"IDENTITY", "KIND", "LEVEL"}, rows)
	return 0
}

func runSharingGrant(args []string) int {
	fs := flag.NewFlagSet("sharing grant", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var level, kind string
	fs.StringVar(&level, "level", "viewer", `Access level: "viewer" or "collaborator".`)
	fs.StringVar(&kind, "kind", "user", `Identity kind: "user" or "group".`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "sharing grant requires <sandbox-id> <identity>")
		return 2
	}
	if level != "viewer" && level != "collaborator" {
		fmt.Fprintln(os.Stderr, `sharing grant: --level must be "viewer" or "collaborator"`)
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("sharing grant", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("sharing grant", err)
	}
	info, err := c.ShareSandbox(context.Background(), fs.Arg(0), agenttierclient.ShareSandboxRequest{
		Identity: fs.Arg(1),
		Level:    level,
		Kind:     kind,
	})
	if err != nil {
		return errExit("sharing grant", err)
	}
	if cfg.output == "json" {
		printJSON(info)
		return 0
	}
	fmt.Printf("granted %s access to %s\n", level, fs.Arg(1))
	return 0
}

func runSharingRevoke(args []string) int {
	fs := flag.NewFlagSet("sharing revoke", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "sharing revoke requires <sandbox-id> <identity>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("sharing revoke", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("sharing revoke", err)
	}
	if err := c.RevokeShare(context.Background(), fs.Arg(0), fs.Arg(1)); err != nil {
		return errExit("sharing revoke", err)
	}
	fmt.Printf("revoked access for %s\n", fs.Arg(1))
	return 0
}

func runSharingCreateLink(args []string) int {
	fs := flag.NewFlagSet("sharing create-link", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var level, expiresIn string
	var maxUses int
	fs.StringVar(&level, "level", "viewer", `Access level: "viewer" or "collaborator".`)
	fs.StringVar(&expiresIn, "expires-in", "", `Link lifetime as a Go duration (e.g. "24h").`)
	fs.IntVar(&maxUses, "max-uses", 0, "Max redemptions (0 = unlimited).")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "sharing create-link requires <sandbox-id>")
		return 2
	}
	if maxUses < 0 {
		fmt.Fprintln(os.Stderr, "sharing create-link: --max-uses must be >= 0")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("sharing create-link", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("sharing create-link", err)
	}
	link, err := c.CreateShareLink(context.Background(), fs.Arg(0), agenttierclient.CreateShareLinkRequest{
		Level:     level,
		ExpiresIn: expiresIn,
		MaxUses:   maxUses,
	})
	if err != nil {
		return errExit("sharing create-link", err)
	}
	if cfg.output == "json" {
		printJSON(link)
		return 0
	}
	fmt.Printf("share link created: %s\n", link.Token)
	fmt.Println("Store this token now — it is shown only once.")
	return 0
}
