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

// runBackups dispatches `agenttier sandbox backups <subcommand> ...` — FR3.
func runBackups(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "backups requires a subcommand: list|create|restore|delete")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return runBackupsList(rest)
	case "create":
		return runBackupsCreate(rest)
	case "restore":
		return runBackupsRestore(rest)
	case "delete":
		return runBackupsDelete(rest)
	default:
		fmt.Fprintf(os.Stderr, "backups: unknown subcommand %q\n", sub)
		return 2
	}
}

func runBackupsList(args []string) int {
	fs := flag.NewFlagSet("backups list", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "backups list requires <sandbox-id>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("backups list", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("backups list", err)
	}
	backups, err := c.ListBackups(context.Background(), fs.Arg(0))
	if err != nil {
		return errExit("backups list", err)
	}
	if cfg.output == "json" {
		printJSON(backups)
		return 0
	}
	rows := make([][]string, 0, len(backups))
	for _, b := range backups {
		ready := "false"
		if b.ReadyToUse {
			ready = "true"
		}
		rows = append(rows, []string{b.Name, dashIfEmpty(b.Kind), ready, dashIfEmpty(b.CreatedAt)})
	}
	printTable([]string{"NAME", "KIND", "READY", "CREATED"}, rows)
	return 0
}

func runBackupsCreate(args []string) int {
	fs := flag.NewFlagSet("backups create", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var name, snapshotClass string
	fs.StringVar(&name, "name", "", "Snapshot name (default: auto-generated).")
	fs.StringVar(&snapshotClass, "snapshot-class", "", "Override the cluster's default VolumeSnapshotClass.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "backups create requires <sandbox-id>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("backups create", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("backups create", err)
	}
	backup, err := c.CreateBackup(context.Background(), fs.Arg(0), agenttierclient.CreateBackupRequest{
		Name:          name,
		SnapshotClass: snapshotClass,
	})
	if err != nil {
		return errExit("backups create", err)
	}
	if cfg.output == "json" {
		printJSON(backup)
		return 0
	}
	fmt.Printf("created backup %s\n", backup.Name)
	return 0
}

func runBackupsRestore(args []string) int {
	fs := flag.NewFlagSet("backups restore", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var name string
	fs.StringVar(&name, "name", "", "Name for the restored sandbox (default: auto-generated).")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "backups restore requires <sandbox-id> <snapshot-name>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("backups restore", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("backups restore", err)
	}
	result, err := c.RestoreBackup(context.Background(), fs.Arg(0), fs.Arg(1), agenttierclient.RestoreBackupRequest{Name: name})
	if err != nil {
		return errExit("backups restore", err)
	}
	if cfg.output == "json" {
		printJSON(result)
		return 0
	}
	fmt.Printf("restored %s from %s\n", result.Name, fs.Arg(1))
	fmt.Printf("poll: agenttier sandbox get %s\n", result.Name)
	return 0
}

func runBackupsDelete(args []string) int {
	fs := flag.NewFlagSet("backups delete", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "backups delete requires <sandbox-id> <snapshot-name>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("backups delete", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("backups delete", err)
	}
	if err := c.DeleteBackup(context.Background(), fs.Arg(0), fs.Arg(1)); err != nil {
		return errExit("backups delete", err)
	}
	fmt.Printf("deleted backup %s\n", fs.Arg(1))
	return 0
}
