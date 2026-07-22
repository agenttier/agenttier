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
	"os"
)

// runUser dispatches `agenttier user <subcommand> ...` — FR1.5.
func runUser(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "user requires a subcommand: whoami|preferences-get|preferences-set")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "whoami":
		return runUserWhoami(rest)
	case "preferences-get":
		return runUserPreferencesGet(rest)
	case "preferences-set":
		return runUserPreferencesSet(rest)
	default:
		fmt.Fprintf(os.Stderr, "user: unknown subcommand %q\n", sub)
		return 2
	}
}

func runUserWhoami(args []string) int {
	fs := flag.NewFlagSet("user whoami", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("user whoami", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("user whoami", err)
	}
	who, err := c.GetCurrentUser(context.Background())
	if err != nil {
		return errExit("user whoami", err)
	}
	if cfg.output == "json" {
		printJSON(who)
		return 0
	}
	label := who.Email
	if label == "" {
		label = who.Name
	}
	if label == "" {
		label = who.Sub
	}
	if who.IsAdmin {
		label += " (admin)"
	}
	fmt.Println(label)
	return 0
}

func runUserPreferencesGet(args []string) int {
	fs := flag.NewFlagSet("user preferences-get", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("user preferences-get", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("user preferences-get", err)
	}
	prefs, err := c.GetPreferences(context.Background())
	if err != nil {
		return errExit("user preferences-get", err)
	}
	printJSON(prefs)
	return 0
}

func runUserPreferencesSet(args []string) int {
	fs := flag.NewFlagSet("user preferences-set", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var jsonData string
	fs.StringVar(&jsonData, "json", "", `Preferences as a JSON object, e.g. '{"theme":"dark"}' (required).`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if jsonData == "" {
		fmt.Fprintln(os.Stderr, "user preferences-set requires --json")
		return 2
	}
	var prefs map[string]any
	if err := json.Unmarshal([]byte(jsonData), &prefs); err != nil {
		fmt.Fprintf(os.Stderr, "agenttier: user preferences-set: invalid JSON: %v\n", err)
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("user preferences-set", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("user preferences-set", err)
	}
	updated, err := c.UpdatePreferences(context.Background(), prefs)
	if err != nil {
		return errExit("user preferences-set", err)
	}
	printJSON(updated)
	return 0
}
