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

// runWebhooks dispatches `agenttier webhooks <subcommand> ...` — FR5.
func runWebhooks(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "webhooks requires a subcommand: create|list|delete|deliveries")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "create":
		return runWebhooksCreate(rest)
	case "list":
		return runWebhooksList(rest)
	case "delete":
		return runWebhooksDelete(rest)
	case "deliveries":
		return runWebhooksDeliveries(rest)
	default:
		fmt.Fprintf(os.Stderr, "webhooks: unknown subcommand %q\n", sub)
		return 2
	}
}

// webhookAllowedEventTypes is the fixed FR5.2 vocabulary, mirrored from the
// Router's authoritative copy (pkg/router/webhook_store.go) and the Python
// SDK's webhooks.py so a bad event type is rejected locally before the
// network round-trip (FR1.10), instead of round-tripping to the server only
// to get the same 400 back.
var webhookAllowedEventTypes = map[string]bool{
	"sandbox.creating": true, "sandbox.running": true, "sandbox.stopped": true,
	"sandbox.error": true, "sandbox.deleting": true,
	"backup.created": true, "backup.pruned": true,
	"share.granted": true, "share.revoked": true,
	"agent.invoke.started": true, "agent.invoke.completed": true, "agent.invoke.failed": true,
}

// validateWebhookCreateArgs runs the same local guard clauses the Python
// CLI/SDK apply before POSTing (sharing.py's _validate_create_args pattern):
// non-empty url, https:// scheme, non-empty event types, every event type in
// the known vocabulary. The Router re-validates all of this authoritatively
// server-side (webhook_store.go) — this is strictly a fail-fast convenience,
// not the security boundary.
func validateWebhookCreateArgs(url string, eventTypes []string) error {
	if url == "" {
		return fmt.Errorf("url must be a non-empty string")
	}
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("url must use https:// (webhook deliveries are signed but not encrypted otherwise)")
	}
	if len(eventTypes) == 0 {
		return fmt.Errorf("event-types must be a non-empty list")
	}
	var unknown []string
	for _, t := range eventTypes {
		if !webhookAllowedEventTypes[t] {
			unknown = append(unknown, t)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("unknown event type(s): %s", strings.Join(unknown, ", "))
	}
	return nil
}

func runWebhooksCreate(args []string) int {
	fs := flag.NewFlagSet("webhooks create", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var url, eventTypes, sandboxID, namespace string
	fs.StringVar(&url, "url", "", "Receiver URL, must be https:// (required).")
	fs.StringVar(&eventTypes, "event-types", "", "Comma-separated event types to subscribe to (required).")
	fs.StringVar(&sandboxID, "sandbox-id", "", "Scope the subscription to one sandbox.")
	fs.StringVar(&namespace, "namespace", "", "Scope the subscription to one namespace.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if url == "" {
		fmt.Fprintln(os.Stderr, "webhooks create requires --url")
		return 2
	}
	if eventTypes == "" {
		fmt.Fprintln(os.Stderr, "webhooks create requires --event-types")
		return 2
	}
	parsedEventTypes := strings.Split(eventTypes, ",")
	if err := validateWebhookCreateArgs(url, parsedEventTypes); err != nil {
		fmt.Fprintf(os.Stderr, "webhooks create: %v\n", err)
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("webhooks create", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("webhooks create", err)
	}
	webhook, err := c.CreateWebhook(context.Background(), agenttierclient.CreateWebhookRequest{
		URL:        url,
		EventTypes: parsedEventTypes,
		SandboxID:  sandboxID,
		Namespace:  namespace,
	})
	if err != nil {
		return errExit("webhooks create", err)
	}
	if cfg.output == "json" {
		printJSON(webhook)
		return 0
	}
	fmt.Printf("created webhook %s\n", webhook.ID)
	fmt.Printf("secret: %s\n", webhook.Secret)
	fmt.Println("Store this secret now — it is shown only once.")
	return 0
}

func runWebhooksList(args []string) int {
	fs := flag.NewFlagSet("webhooks list", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("webhooks list", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("webhooks list", err)
	}
	webhooks, err := c.ListWebhooks(context.Background())
	if err != nil {
		return errExit("webhooks list", err)
	}
	if cfg.output == "json" {
		printJSON(webhooks)
		return 0
	}
	rows := make([][]string, 0, len(webhooks))
	for _, w := range webhooks {
		rows = append(rows, []string{w.ID, w.URL, strings.Join(w.EventTypes, ",")})
	}
	printTable([]string{"ID", "URL", "EVENT-TYPES"}, rows)
	return 0
}

func runWebhooksDelete(args []string) int {
	fs := flag.NewFlagSet("webhooks delete", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "webhooks delete requires <id>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("webhooks delete", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("webhooks delete", err)
	}
	if err := c.DeleteWebhook(context.Background(), fs.Arg(0)); err != nil {
		return errExit("webhooks delete", err)
	}
	fmt.Printf("deleted %s\n", fs.Arg(0))
	return 0
}

func runWebhooksDeliveries(args []string) int {
	fs := flag.NewFlagSet("webhooks deliveries", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "webhooks deliveries requires <id>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("webhooks deliveries", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("webhooks deliveries", err)
	}
	deliveries, err := c.GetWebhookDeliveries(context.Background(), fs.Arg(0))
	if err != nil {
		return errExit("webhooks deliveries", err)
	}
	if cfg.output == "json" {
		printJSON(deliveries)
		return 0
	}
	rows := make([][]string, 0, len(deliveries))
	for _, d := range deliveries {
		success := "false"
		if d.Success {
			success = "true"
		}
		rows = append(rows, []string{d.EventType, d.Timestamp, fmt.Sprint(d.StatusCode), fmt.Sprint(d.Attempt), success})
	}
	printTable([]string{"EVENT", "TIMESTAMP", "STATUS", "ATTEMPT", "SUCCESS"}, rows)
	return 0
}
