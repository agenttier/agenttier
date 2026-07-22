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

// runTemplate dispatches `agenttier template <subcommand> ...`, mirroring
// the Python CLI's `template` subparser family.
func runTemplate(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "template requires a subcommand: list|get")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runTemplateList(rest)
	case "get":
		return runTemplateGet(rest)
	default:
		fmt.Fprintf(os.Stderr, "template: unknown subcommand %q\n", sub)
		return 2
	}
}

func runTemplateList(args []string) int {
	fs := flag.NewFlagSet("template list", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("template list", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("template list", err)
	}
	templates, err := c.ListTemplates(context.Background())
	if err != nil {
		return errExit("template list", err)
	}
	if cfg.output == "json" {
		printJSON(templates)
		return 0
	}
	rows := make([][]string, 0, len(templates))
	for _, t := range templates {
		rows = append(rows, []string{t.Name, dashIfEmpty(t.Image), dashIfEmpty(t.Description)})
	}
	printTable([]string{"NAME", "IMAGE", "DESCRIPTION"}, rows)
	return 0
}

func runTemplateGet(args []string) int {
	fs := flag.NewFlagSet("template get", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "template get requires <name>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("template get", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("template get", err)
	}
	tmpl, err := c.GetTemplate(context.Background(), fs.Arg(0))
	if err != nil {
		return errExit("template get", err)
	}
	printJSON(tmpl)
	return 0
}
