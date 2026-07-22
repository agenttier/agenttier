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
	"strings"
	"time"

	"github.com/agenttier/agenttier/pkg/agenttierclient"
)

// runSandbox dispatches `agenttier sandbox <subcommand> ...`, mirroring the
// Python CLI's `sandbox` subparser family (python-sdk/src/agenttier/cli.py).
func runSandbox(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "sandbox requires a subcommand: list|get|create|stop|resume|delete|clone|exec|wait|patch|bulk-create|bulk-action|files|ports|sharing|backups")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runSandboxList(rest)
	case "get":
		return runSandboxGet(rest)
	case "create":
		return runSandboxCreate(rest)
	case "stop":
		return runSandboxSimpleAction(rest, "stop", func(c *agenttierclient.Client, id string) error {
			return c.StopSandbox(context.Background(), id)
		})
	case "resume":
		return runSandboxSimpleAction(rest, "resume", func(c *agenttierclient.Client, id string) error {
			return c.ResumeSandbox(context.Background(), id)
		})
	case "delete":
		return runSandboxSimpleAction(rest, "delete", func(c *agenttierclient.Client, id string) error {
			return c.DeleteSandbox(context.Background(), id)
		})
	case "clone":
		return runSandboxClone(rest)
	case "exec":
		return runSandboxExec(rest)
	case "wait":
		return runSandboxWait(rest)
	case "patch":
		return runSandboxPatch(rest)
	case "bulk-create":
		return runSandboxBulkCreate(rest)
	case "bulk-action":
		return runSandboxBulkAction(rest)
	case "files":
		return runFiles(rest)
	case "ports":
		return runPorts(rest)
	case "sharing":
		return runSharing(rest)
	case "backups":
		return runBackups(rest)
	default:
		fmt.Fprintf(os.Stderr, "sandbox: unknown subcommand %q\n", sub)
		return 2
	}
}

func runSandboxList(args []string) int {
	fs := flag.NewFlagSet("sandbox list", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var namespace, status string
	fs.StringVar(&namespace, "namespace", "", "Filter by namespace.")
	fs.StringVar(&status, "status", "", "Filter by status (Running, Stopped, ...).")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("sandbox list", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("sandbox list", err)
	}
	sandboxes, err := c.ListSandboxes(context.Background(), agenttierclient.ListSandboxesOptions{Namespace: namespace, Status: status})
	if err != nil {
		return errExit("sandbox list", err)
	}
	if cfg.output == "json" {
		printJSON(sandboxes)
		return 0
	}
	rows := make([][]string, 0, len(sandboxes))
	for _, s := range sandboxes {
		rows = append(rows, []string{s.SandboxID, s.Name, s.Status, dashIfEmpty(s.TemplateRef), s.Namespace})
	}
	printTable([]string{"ID", "NAME", "STATUS", "TEMPLATE", "NAMESPACE"}, rows)
	return 0
}

func runSandboxGet(args []string) int {
	fs := flag.NewFlagSet("sandbox get", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "sandbox get requires <sandbox-id>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("sandbox get", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("sandbox get", err)
	}
	sb, err := c.GetSandbox(context.Background(), fs.Arg(0))
	if err != nil {
		return errExit("sandbox get", err)
	}
	if cfg.output == "json" {
		printJSON(sb)
		return 0
	}
	fmt.Printf("id:        %s\n", sb.SandboxID)
	fmt.Printf("name:      %s\n", sb.Name)
	fmt.Printf("status:    %s\n", sb.Status)
	fmt.Printf("template:  %s\n", dashIfEmpty(sb.TemplateRef))
	fmt.Printf("namespace: %s\n", sb.Namespace)
	if sb.PodName != "" {
		fmt.Printf("pod:       %s\n", sb.PodName)
	}
	return 0
}

func runSandboxCreate(args []string) int {
	fs := flag.NewFlagSet("sandbox create", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var template, namespace, timeout, idleTimeout, storageSize string
	fs.StringVar(&template, "template", "", "ClusterSandboxTemplate name (required).")
	fs.StringVar(&namespace, "namespace", "default", "Namespace to create in.")
	fs.StringVar(&timeout, "timeout", "", `Max-runtime duration (e.g. "8h").`)
	fs.StringVar(&idleTimeout, "idle-timeout", "", `Idle timeout (e.g. "30m").`)
	fs.StringVar(&storageSize, "storage-size", "", `PVC size (e.g. "10Gi").`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "sandbox create requires <name>")
		return 2
	}
	if template == "" {
		fmt.Fprintln(os.Stderr, "sandbox create requires --template")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("sandbox create", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("sandbox create", err)
	}
	req := agenttierclient.CreateSandboxRequest{
		Name:      fs.Arg(0),
		Namespace: namespace,
		// Kind defaults to "ClusterSandboxTemplate" — the controller's
		// template_resolver.go defaults an empty Kind to the NAMESPACED
		// "SandboxTemplate" instead, but every built-in template ships as
		// a ClusterSandboxTemplate, so an unset Kind here always 404s on
		// "sandbox create --template <builtin>". Matches the Python SDK's
		// create_sandbox, which hardcodes the same default with no
		// override — there is no --template-kind flag on either CLI.
		TemplateRef: &agenttierclient.TemplateRef{Name: template, Kind: "ClusterSandboxTemplate"},
		Timeout:     timeout,
		IdleTimeout: idleTimeout,
	}
	if storageSize != "" {
		req.Storage = &agenttierclient.StorageSpec{Size: storageSize}
	}
	sb, err := c.CreateSandbox(context.Background(), req)
	if err != nil {
		return errExit("sandbox create", err)
	}
	if cfg.output == "json" {
		printJSON(sb)
		return 0
	}
	fmt.Printf("created %s (status: %s)\n", sb.SandboxID, sb.Status)
	return 0
}

// runSandboxSimpleAction covers stop/resume/delete — one positional
// sandbox-id argument, no request body, no response body.
func runSandboxSimpleAction(args []string, verb string, do func(*agenttierclient.Client, string) error) int {
	fs := flag.NewFlagSet("sandbox "+verb, flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "sandbox %s requires <sandbox-id>\n", verb)
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("sandbox "+verb, err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("sandbox "+verb, err)
	}
	id := fs.Arg(0)
	if err := do(c, id); err != nil {
		return errExit("sandbox "+verb, err)
	}
	pastTense := map[string]string{"stop": "stopped", "resume": "resumed", "delete": "deleted"}[verb]
	fmt.Printf("%s %s\n", pastTense, id)
	return 0
}

func runSandboxClone(args []string) int {
	fs := flag.NewFlagSet("sandbox clone", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var name, snapshotClass string
	fs.StringVar(&name, "name", "", "Name for the cloned sandbox (default: <source>-clone-<ts>).")
	fs.StringVar(&snapshotClass, "snapshot-class", "", "Override the cluster's default VolumeSnapshotClass (advanced).")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "sandbox clone requires <sandbox-id>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("sandbox clone", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("sandbox clone", err)
	}
	sourceID := fs.Arg(0)
	result, err := c.CloneSandbox(context.Background(), sourceID, agenttierclient.CloneSandboxRequest{
		Name:          name,
		SnapshotClass: snapshotClass,
	})
	if err != nil {
		return errExit("sandbox clone", err)
	}
	if cfg.output == "json" {
		printJSON(result)
		return 0
	}
	fmt.Printf("clone %s created from %s\n", result.Name, sourceID)
	fmt.Printf("poll: agenttier sandbox get %s\n", result.Name)
	return 0
}

func runSandboxExec(args []string) int {
	fs := flag.NewFlagSet("sandbox exec", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var timeout int
	fs.IntVar(&timeout, "timeout", 30, "Command timeout (seconds).")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "sandbox exec requires <sandbox-id> <command...>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("sandbox exec", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("sandbox exec", err)
	}
	id := fs.Arg(0)
	command := strings.Join(fs.Args()[1:], " ")
	result, err := c.ExecCommand(context.Background(), id, agenttierclient.ExecRequest{Command: command, Timeout: timeout})
	if err != nil {
		return errExit("sandbox exec", err)
	}
	if cfg.output == "json" {
		printJSON(result)
		return result.ExitCode
	}
	fmt.Print(result.Stdout)
	if result.Stdout != "" && !strings.HasSuffix(result.Stdout, "\n") {
		fmt.Println()
	}
	if result.Stderr != "" {
		fmt.Fprint(os.Stderr, result.Stderr)
		if !strings.HasSuffix(result.Stderr, "\n") {
			fmt.Fprintln(os.Stderr)
		}
	}
	return result.ExitCode
}

// sandboxWaitPollInterval is how often `sandbox wait` re-polls GET
// /sandboxes/{id} while waiting for Running. Short enough to feel
// responsive, long enough not to hammer the Router.
const sandboxWaitPollInterval = 2 * time.Second

func runSandboxWait(args []string) int {
	fs := flag.NewFlagSet("sandbox wait", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var timeoutSeconds int
	fs.IntVar(&timeoutSeconds, "timeout", 180, "Wait timeout in seconds.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "sandbox wait requires <sandbox-id>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("sandbox wait", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("sandbox wait", err)
	}
	id := fs.Arg(0)
	deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	for {
		sb, err := c.GetSandbox(context.Background(), id)
		if err != nil {
			return errExit("sandbox wait", err)
		}
		if sb.Status == "Running" {
			fmt.Printf("%s is running\n", id)
			return 0
		}
		if sb.Status == "Error" {
			return errExit("sandbox wait", fmt.Errorf("sandbox entered Error phase: %s", sb.Message))
		}
		if time.Now().After(deadline) {
			return errExit("sandbox wait", fmt.Errorf("timed out after %ds waiting for Running (current: %s)", timeoutSeconds, sb.Status))
		}
		time.Sleep(sandboxWaitPollInterval)
	}
}

func runSandboxPatch(args []string) int {
	fs := flag.NewFlagSet("sandbox patch", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var idleTimeout, cpuRequest, memRequest, cpuLimit, memLimit string
	var labels, annotations stringMapFlag
	fs.StringVar(&idleTimeout, "idle-timeout", "", `New idle timeout (e.g. "30m").`)
	fs.StringVar(&cpuRequest, "cpu-request", "", "CPU request (e.g. \"1\").")
	fs.StringVar(&memRequest, "memory-request", "", "Memory request (e.g. \"2Gi\").")
	fs.StringVar(&cpuLimit, "cpu-limit", "", "CPU limit (e.g. \"2\").")
	fs.StringVar(&memLimit, "memory-limit", "", "Memory limit (e.g. \"4Gi\").")
	fs.Var(&labels, "label", `Label to set: "key=value". Repeatable.`)
	fs.Var(&annotations, "annotation", `Annotation to set: "key=value". Repeatable.`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "sandbox patch requires <sandbox-id>")
		return 2
	}
	req := agenttierclient.PatchSandboxRequest{
		IdleTimeout: idleTimeout,
		Labels:      map[string]string(labels),
		Annotations: map[string]string(annotations),
	}
	if cpuRequest != "" || memRequest != "" || cpuLimit != "" || memLimit != "" {
		req.Resources = &agenttierclient.ResourceRequirements{}
		if cpuRequest != "" || memRequest != "" {
			req.Resources.Requests = map[string]string{}
			if cpuRequest != "" {
				req.Resources.Requests["cpu"] = cpuRequest
			}
			if memRequest != "" {
				req.Resources.Requests["memory"] = memRequest
			}
		}
		if cpuLimit != "" || memLimit != "" {
			req.Resources.Limits = map[string]string{}
			if cpuLimit != "" {
				req.Resources.Limits["cpu"] = cpuLimit
			}
			if memLimit != "" {
				req.Resources.Limits["memory"] = memLimit
			}
		}
	}
	if req.IdleTimeout == "" && req.Resources == nil && len(req.Labels) == 0 && len(req.Annotations) == 0 {
		fmt.Fprintln(os.Stderr, "sandbox patch requires at least one of --idle-timeout, --cpu-request, --memory-request, --cpu-limit, --memory-limit, --label, --annotation")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("sandbox patch", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("sandbox patch", err)
	}
	result, err := c.PatchSandbox(context.Background(), fs.Arg(0), req)
	if err != nil {
		return errExit("sandbox patch", err)
	}
	if cfg.output == "json" {
		printJSON(result)
		return 0
	}
	for field, applied := range result.Applied {
		fmt.Printf("%s: %s\n", field, applied)
	}
	if result.RestartRequired {
		fmt.Println(result.Message)
	}
	return 0
}

// stringMapFlag implements flag.Value for repeatable "key=value" flags
// (--label / --annotation).
type stringMapFlag map[string]string

func (m *stringMapFlag) String() string {
	if *m == nil {
		return ""
	}
	parts := make([]string, 0, len(*m))
	for k, v := range *m {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

func (m *stringMapFlag) Set(v string) error {
	parts := strings.SplitN(v, "=", 2)
	if len(parts) != 2 || parts[0] == "" {
		return fmt.Errorf("expected key=value, got %q", v)
	}
	if *m == nil {
		*m = map[string]string{}
	}
	(*m)[parts[0]] = parts[1]
	return nil
}

// parsePortArg parses a port number, rejecting non-positive values the
// Router would reject anyway (fail fast locally, FR1.10).
func parsePortArg(raw string) (int32, error) {
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid port %q: must be a positive integer", raw)
	}
	return int32(n), nil
}
