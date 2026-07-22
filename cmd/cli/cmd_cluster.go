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

// runCluster dispatches `agenttier cluster <subcommand> ...` — FR1.7.
func runCluster(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "cluster requires a subcommand: status|nodes|headroom-get|headroom-set")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "status":
		return runClusterStatus(rest)
	case "nodes":
		return runClusterNodes(rest)
	case "headroom-get":
		return runClusterHeadroomGet(rest)
	case "headroom-set":
		return runClusterHeadroomSet(rest)
	default:
		fmt.Fprintf(os.Stderr, "cluster: unknown subcommand %q\n", sub)
		return 2
	}
}

func runClusterStatus(args []string) int {
	fs := flag.NewFlagSet("cluster status", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("cluster status", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("cluster status", err)
	}
	status, err := c.GetClusterStatus(context.Background())
	if err != nil {
		return errExit("cluster status", err)
	}
	printJSON(status)
	return 0
}

func runClusterNodes(args []string) int {
	fs := flag.NewFlagSet("cluster nodes", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("cluster nodes", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("cluster nodes", err)
	}
	out, err := c.GetClusterNodes(context.Background())
	if err != nil {
		return errExit("cluster nodes", err)
	}
	if cfg.output == "json" {
		printJSON(out)
		return 0
	}
	rows := make([][]string, 0, len(out.Nodes))
	for _, n := range out.Nodes {
		ready := "false"
		if n.Ready {
			ready = "true"
		}
		rows = append(rows, []string{n.Name, ready, dashIfEmpty(n.InstanceType), dashIfEmpty(n.NodeGroup)})
	}
	printTable([]string{"NAME", "READY", "INSTANCE-TYPE", "NODE-GROUP"}, rows)
	return 0
}

func runClusterHeadroomGet(args []string) int {
	fs := flag.NewFlagSet("cluster headroom-get", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("cluster headroom-get", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("cluster headroom-get", err)
	}
	cfgOut, err := c.GetHeadroomConfig(context.Background())
	if err != nil {
		return errExit("cluster headroom-get", err)
	}
	printJSON(cfgOut)
	return 0
}

func runClusterHeadroomSet(args []string) int {
	fs := flag.NewFlagSet("cluster headroom-set", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var replicas int
	var cpu, memory string
	fs.IntVar(&replicas, "replicas", -1, "Spare-node pause-Pod replica count (0-50, required).")
	fs.StringVar(&cpu, "cpu", "", `Per-replica CPU request/limit (e.g. "500m").`)
	fs.StringVar(&memory, "memory", "", `Per-replica memory request/limit (e.g. "1Gi").`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if replicas < 0 || replicas > 50 {
		fmt.Fprintln(os.Stderr, "cluster headroom-set requires --replicas in [0, 50]")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("cluster headroom-set", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("cluster headroom-set", err)
	}
	result, err := c.SetHeadroomConfig(context.Background(), agenttierclient.HeadroomConfig{
		Replicas: replicas,
		CPU:      cpu,
		Memory:   memory,
	})
	if err != nil {
		return errExit("cluster headroom-set", err)
	}
	printJSON(result)
	return 0
}
