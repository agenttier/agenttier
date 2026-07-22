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
	"strconv"
)

// runPorts dispatches `agenttier sandbox ports <subcommand> ...`, mirroring
// the Python CLI's `sandbox ports` subparser family.
func runPorts(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "ports requires a subcommand: list|forward|remove")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runPortsList(rest)
	case "forward":
		return runPortsForward(rest)
	case "remove":
		return runPortsRemove(rest)
	default:
		fmt.Fprintf(os.Stderr, "ports: unknown subcommand %q\n", sub)
		return 2
	}
}

func runPortsList(args []string) int {
	fs := flag.NewFlagSet("ports list", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "ports list requires <sandbox-id>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("ports list", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("ports list", err)
	}
	ports, err := c.ListPorts(context.Background(), fs.Arg(0))
	if err != nil {
		return errExit("ports list", err)
	}
	if cfg.output == "json" {
		printJSON(ports)
		return 0
	}
	rows := make([][]string, 0, len(ports))
	for _, p := range ports {
		url := p.PreviewURL
		if url == "" {
			url = p.InternalURL
		}
		protocol := p.Protocol
		if protocol == "" {
			protocol = "http"
		}
		rows = append(rows, []string{strconv.Itoa(int(p.Port)), protocol, url})
	}
	printTable([]string{"PORT", "PROTOCOL", "URL"}, rows)
	return 0
}

func runPortsForward(args []string) int {
	fs := flag.NewFlagSet("ports forward", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var port int
	var protocol string
	fs.IntVar(&port, "port", 0, "Container port to forward (required).")
	fs.StringVar(&protocol, "protocol", "http", `Protocol: "http" or "tcp".`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "ports forward requires <sandbox-id>")
		return 2
	}
	if port <= 0 {
		fmt.Fprintln(os.Stderr, "ports forward requires --port > 0")
		return 2
	}
	if protocol != "http" && protocol != "tcp" {
		fmt.Fprintln(os.Stderr, `ports forward: --protocol must be "http" or "tcp"`)
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("ports forward", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("ports forward", err)
	}
	fp, err := c.ForwardPort(context.Background(), fs.Arg(0), int32(port), protocol)
	if err != nil {
		return errExit("ports forward", err)
	}
	if cfg.output == "json" {
		printJSON(fp)
		return 0
	}
	target := fp.PreviewURL
	if target == "" {
		target = fp.InternalURL
	}
	if target == "" {
		target = "(no URL)"
	}
	fmt.Printf("forwarded port %d -> %s\n", fp.Port, target)
	return 0
}

func runPortsRemove(args []string) int {
	fs := flag.NewFlagSet("ports remove", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var port int
	fs.IntVar(&port, "port", 0, "Forwarded port to remove (required).")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "ports remove requires <sandbox-id>")
		return 2
	}
	if port <= 0 {
		fmt.Fprintln(os.Stderr, "ports remove requires --port > 0")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("ports remove", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("ports remove", err)
	}
	if err := c.RemovePort(context.Background(), fs.Arg(0), int32(port)); err != nil {
		return errExit("ports remove", err)
	}
	fmt.Printf("removed forward for port %d\n", port)
	return 0
}
