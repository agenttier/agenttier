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

// runGovernance dispatches `agenttier governance <subcommand> ...` — FR1.2.
func runGovernance(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "governance requires a subcommand: list|get|set|delete|effective")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runGovernanceList(rest)
	case "get":
		return runGovernanceGet(rest)
	case "set":
		return runGovernanceSet(rest)
	case "delete":
		return runGovernanceDelete(rest)
	case "effective":
		return runGovernanceEffective(rest)
	default:
		fmt.Fprintf(os.Stderr, "governance: unknown subcommand %q\n", sub)
		return 2
	}
}

func runGovernanceList(args []string) int {
	fs := flag.NewFlagSet("governance list", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("governance list", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("governance list", err)
	}
	result, err := c.ListPolicies(context.Background())
	if err != nil {
		return errExit("governance list", err)
	}
	printJSON(result)
	return 0
}

func runGovernanceGet(args []string) int {
	fs := flag.NewFlagSet("governance get", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "governance get requires <namespace>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("governance get", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("governance get", err)
	}
	policy, err := c.GetPolicy(context.Background(), fs.Arg(0))
	if err != nil {
		return errExit("governance get", err)
	}
	printJSON(policy)
	return 0
}

func runGovernanceSet(args []string) int {
	fs := flag.NewFlagSet("governance set", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var namespace string
	var maxSandboxesPerUser, maxSandboxesTotal, maxAgentSandboxes int
	var maxCPU, maxMemory, maxStorage, maxTimeout, maxIdleTimeout string
	fs.StringVar(&namespace, "namespace", "", "Namespace to set the policy for (omit for the cluster-wide default).")
	fs.IntVar(&maxSandboxesPerUser, "max-sandboxes-per-user", 0, "0 = unlimited.")
	fs.IntVar(&maxSandboxesTotal, "max-sandboxes-total", 0, "0 = unlimited.")
	fs.IntVar(&maxAgentSandboxes, "max-agent-sandboxes", 0, "0 = no agent-specific cap.")
	fs.StringVar(&maxCPU, "max-cpu", "", `CPU limit per sandbox (e.g. "4").`)
	fs.StringVar(&maxMemory, "max-memory", "", `Memory limit per sandbox (e.g. "8Gi").`)
	fs.StringVar(&maxStorage, "max-storage", "", `PVC size limit per sandbox (e.g. "50Gi").`)
	fs.StringVar(&maxTimeout, "max-timeout", "", `Max spec.timeout (e.g. "24h").`)
	fs.StringVar(&maxIdleTimeout, "max-idle-timeout", "", `Max spec.idleTimeout (e.g. "1h").`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("governance set", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("governance set", err)
	}
	policy := agenttierclient.Policy{
		MaxSandboxesPerUser: maxSandboxesPerUser,
		MaxSandboxesTotal:   maxSandboxesTotal,
		MaxAgentSandboxes:   maxAgentSandboxes,
		MaxCPU:              maxCPU,
		MaxMemory:           maxMemory,
		MaxStorage:          maxStorage,
		MaxTimeout:          maxTimeout,
		MaxIdleTimeout:      maxIdleTimeout,
	}
	var result *agenttierclient.Policy
	if namespace == "" {
		result, err = c.SetClusterPolicy(context.Background(), policy)
	} else {
		result, err = c.SetPolicy(context.Background(), namespace, policy)
	}
	if err != nil {
		return errExit("governance set", err)
	}
	printJSON(result)
	return 0
}

func runGovernanceDelete(args []string) int {
	fs := flag.NewFlagSet("governance delete", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "governance delete requires <namespace>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("governance delete", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("governance delete", err)
	}
	if err := c.DeletePolicy(context.Background(), fs.Arg(0)); err != nil {
		return errExit("governance delete", err)
	}
	fmt.Printf("deleted policy for namespace %s\n", fs.Arg(0))
	return 0
}

func runGovernanceEffective(args []string) int {
	fs := flag.NewFlagSet("governance effective", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var namespace string
	fs.StringVar(&namespace, "namespace", "", "Namespace to resolve (default: the Router's configured default).")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("governance effective", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("governance effective", err)
	}
	policy, err := c.GetEffectivePolicy(context.Background(), namespace)
	if err != nil {
		return errExit("governance effective", err)
	}
	printJSON(policy)
	return 0
}
