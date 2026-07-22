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
	"io"
	"os"
	"strconv"
)

// runFiles dispatches `agenttier sandbox files <subcommand> ...`, mirroring
// the Python CLI's `sandbox files` subparser family.
func runFiles(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "files requires a subcommand: ls|cat|upload|download|write")
		return 2
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls":
		return runFilesLs(rest)
	case "cat":
		return runFilesCat(rest)
	case "upload":
		return runFilesUpload(rest)
	case "download":
		return runFilesDownload(rest)
	case "write":
		return runFilesWrite(rest)
	default:
		fmt.Fprintf(os.Stderr, "files: unknown subcommand %q\n", sub)
		return 2
	}
}

func runFilesLs(args []string) int {
	fs := flag.NewFlagSet("files ls", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var path string
	fs.StringVar(&path, "path", "/workspace", "Sandbox directory to list.")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "files ls requires <sandbox-id>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("files ls", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("files ls", err)
	}
	entries, err := c.ListFiles(context.Background(), fs.Arg(0), path)
	if err != nil {
		return errExit("files ls", err)
	}
	if cfg.output == "json" {
		printJSON(entries)
		return 0
	}
	rows := make([][]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name
		typ := "file"
		if e.IsDir {
			name += "/"
			typ = "dir"
		}
		modified := ""
		if e.ModifiedAt != 0 {
			modified = strconv.FormatInt(e.ModifiedAt, 10)
		}
		rows = append(rows, []string{name, typ, strconv.FormatInt(e.Size, 10), modified})
	}
	printTable([]string{"NAME", "TYPE", "SIZE", "MODIFIED"}, rows)
	return 0
}

func runFilesCat(args []string) int {
	fs := flag.NewFlagSet("files cat", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "files cat requires <sandbox-id> <path>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("files cat", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("files cat", err)
	}
	data, err := c.GetFile(context.Background(), fs.Arg(0), fs.Arg(1))
	if err != nil {
		return errExit("files cat", err)
	}
	_, _ = os.Stdout.Write(data)
	return 0
}

func runFilesUpload(args []string) int {
	fs := flag.NewFlagSet("files upload", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 3 {
		fmt.Fprintln(os.Stderr, "files upload requires <sandbox-id> <local-path> <remote-path>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("files upload", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("files upload", err)
	}
	id, local, remote := fs.Arg(0), fs.Arg(1), fs.Arg(2)
	data, err := os.ReadFile(local) // #nosec G304 -- local is a CLI-argument path the caller explicitly asked to upload
	if err != nil {
		return errExit("files upload", err)
	}
	result, err := c.PutFile(context.Background(), id, remote, data)
	if err != nil {
		return errExit("files upload", err)
	}
	fmt.Printf("uploaded %d bytes to %s\n", result.Bytes, remote)
	return 0
}

func runFilesDownload(args []string) int {
	fs := flag.NewFlagSet("files download", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 3 {
		fmt.Fprintln(os.Stderr, "files download requires <sandbox-id> <remote-path> <local-path>")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("files download", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("files download", err)
	}
	id, remote, local := fs.Arg(0), fs.Arg(1), fs.Arg(2)
	data, err := c.GetFile(context.Background(), id, remote)
	if err != nil {
		return errExit("files download", err)
	}
	if err := os.WriteFile(local, data, 0o600); err != nil {
		return errExit("files download", err)
	}
	fmt.Printf("downloaded %d bytes to %s\n", len(data), local)
	return 0
}

func runFilesWrite(args []string) int {
	fs := flag.NewFlagSet("files write", flag.ContinueOnError)
	cfg := registerCLIFlags(fs)
	var data, file string
	fs.StringVar(&data, "data", "", "Inline content (utf-8).")
	fs.StringVar(&file, "file", "", `Local file path; "-" reads stdin.`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fmt.Fprintln(os.Stderr, "files write requires <sandbox-id> <remote-path>")
		return 2
	}
	var payload []byte
	switch {
	case data != "":
		payload = []byte(data)
	case file == "-":
		read, err := io.ReadAll(os.Stdin)
		if err != nil {
			return errExit("files write", err)
		}
		payload = read
	case file != "":
		read, err := os.ReadFile(file) // #nosec G304 -- file is the --file flag the caller explicitly asked to read
		if err != nil {
			return errExit("files write", err)
		}
		payload = read
	default:
		fmt.Fprintln(os.Stderr, "files write requires --data or --file")
		return 2
	}
	if err := cfg.resolve(); err != nil {
		return errExit("files write", err)
	}
	c, err := cfg.client()
	if err != nil {
		return errExit("files write", err)
	}
	id, remote := fs.Arg(0), fs.Arg(1)
	if _, err := c.PutFile(context.Background(), id, remote, payload); err != nil {
		return errExit("files write", err)
	}
	fmt.Printf("wrote %s\n", remote)
	return 0
}
