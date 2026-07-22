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

// Package main is the entrypoint for the AgentTier CLI.
//
// Phase 10 ships two subcommands wired against the agent-mode endpoints:
//
//	agenttier configure <sandbox> [--file path=local-path]... [--install "..."] [--entrypoint "..."]
//	agenttier invoke <sandbox> [--prompt "..."] [--input @body.json] [--timeout 5m] [--cancel <invoke-id>]
//
// Wider CLI work — sandbox list/get/create/stop/delete/exec — is tracked
// under task 5.7 in todo.md and not part of Phase 10's scope.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/agenttier/agenttier/pkg/version"
)

const usageBanner = `agenttier — control AgentTier sandboxes from the command line.

Usage:
  agenttier login --api-url <url> [--api-key <key>|--token <jwt>]
  agenttier sandbox <list|get|create|stop|resume|delete|clone|exec|wait|patch|bulk-create|bulk-action|files|ports|sharing|backups> [flags]
  agenttier template <list|get> [flags]
  agenttier governance <list|get|set|delete|effective> [flags]
  agenttier audit [flags]
  agenttier analytics <usage|costs> [flags]
  agenttier admin <sandboxes|sharing> [flags]
  agenttier user <whoami|preferences-get|preferences-set> [flags]
  agenttier apikeys <list|create|revoke> [flags]
  agenttier warmpool <status|set-config> [flags]
  agenttier cluster <status|nodes|headroom-get|headroom-set> [flags]
  agenttier webhooks <create|list|delete|deliveries> [flags]
  agenttier configure <sandbox-id> [flags]
  agenttier invoke <sandbox-id> [flags]
  agenttier version

Use "agenttier <command> -h" for command-specific help.

Authentication:
  --api-url   AgentTier router base URL (env: AGENTTIER_API_URL).
  --api-key   API key (env: AGENTTIER_API_KEY). When unset and the router
              has OIDC disabled (dev mode), the CLI sends no auth header.
  --token     Bearer token / OIDC JWT (env: AGENTTIER_TOKEN).
  --output    "text" (default) or "json".

Connection settings are also read from ~/.config/agenttier/config.json
(or $AGENTTIER_CONFIG), written by "agenttier login". Precedence:
flag > env var > saved config file.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageBanner)
		os.Exit(2)
	}

	cmd, rest := os.Args[1], os.Args[2:]
	switch cmd {
	case "configure":
		os.Exit(runConfigure(rest))
	case "invoke":
		os.Exit(runInvoke(rest))
	case "login":
		os.Exit(runLogin(rest))
	case "sandbox":
		os.Exit(runSandbox(rest))
	case "template":
		os.Exit(runTemplate(rest))
	case "governance":
		os.Exit(runGovernance(rest))
	case "audit":
		os.Exit(runAudit(rest))
	case "analytics":
		os.Exit(runAnalytics(rest))
	case "admin":
		os.Exit(runAdmin(rest))
	case "user":
		os.Exit(runUser(rest))
	case "apikeys":
		os.Exit(runAPIKeys(rest))
	case "warmpool":
		os.Exit(runWarmPool(rest))
	case "cluster":
		os.Exit(runCluster(rest))
	case "webhooks":
		os.Exit(runWebhooks(rest))
	case "version", "--version", "-v":
		fmt.Printf("agenttier %s (%s)\n", version.Version, version.GitCommit)
	case "-h", "--help", "help":
		fmt.Print(usageBanner)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usageBanner)
		os.Exit(2)
	}
}

// --- common args ---------------------------------------------------------

type globalFlags struct {
	apiURL string
	apiKey string
}

func registerGlobalFlags(fs *flag.FlagSet) *globalFlags {
	g := &globalFlags{
		apiURL: os.Getenv("AGENTTIER_API_URL"),
		apiKey: os.Getenv("AGENTTIER_API_KEY"),
	}
	fs.StringVar(&g.apiURL, "api-url", g.apiURL,
		"Router base URL (default: $AGENTTIER_API_URL).")
	fs.StringVar(&g.apiKey, "api-key", g.apiKey,
		"API key (default: $AGENTTIER_API_KEY).")
	return g
}

func (g *globalFlags) validate() error {
	if g.apiURL == "" {
		return errors.New("--api-url is required (or set AGENTTIER_API_URL)")
	}
	return nil
}

func (g *globalFlags) request(method, path string, body io.Reader) (*http.Request, error) {
	full := strings.TrimRight(g.apiURL, "/") + "/api/v1" + path
	req, err := http.NewRequest(method, full, body)
	if err != nil {
		return nil, err
	}
	if g.apiKey != "" {
		req.Header.Set("X-API-Key", g.apiKey)
	}
	return req, nil
}

// --- `agenttier configure` -----------------------------------------------

type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ",") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

func runConfigure(args []string) int {
	fs := flag.NewFlagSet("configure", flag.ContinueOnError)
	g := registerGlobalFlags(fs)
	var (
		install    string
		entrypoint string
		fileFlags  stringSliceFlag
	)
	fs.StringVar(&install, "install", "", `Install command, run once at /configure (e.g. "pip install -r req.txt").`)
	fs.StringVar(&entrypoint, "entrypoint", "", `Entrypoint command for /invoke (e.g. "python /workspace/agent.py").`)
	fs.Var(&fileFlags, "file",
		`Upload a file: "path=local-path". Repeatable. Example: --file /workspace/agent.py=./agent.py`)

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "configure requires <sandbox-id>")
		fs.Usage()
		return 2
	}
	sandboxID := fs.Arg(0)
	if err := g.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	body, err := buildConfigurePayload(install, entrypoint, fileFlags)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure: %v\n", err)
		return 1
	}

	req, err := g.request(http.MethodPost,
		"/sandboxes/"+url.PathEscape(sandboxID)+"/configure",
		bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	// 15-min HTTP timeout matches the Router's soft install timeout. The
	// Router will surface its own error if pip is genuinely stuck.
	client := &http.Client{Timeout: 15 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	warnIfDeprecated(resp)
	if resp.StatusCode/100 != 2 {
		return printErrorResponse("configure", resp)
	}

	exitCode := 0
	stderr := os.Stderr
	stdout := os.Stdout
	for evt := range parseSSE(resp.Body) {
		switch evt.event {
		case "log":
			stream := evt.dataField("stream", "stdout")
			line := evt.dataField("data", "")
			if stream == "stderr" {
				fmt.Fprintln(stderr, line)
			} else {
				fmt.Fprintln(stdout, line)
			}
		case "result":
			// Print a one-line summary so scripts can grep for it.
			ec := evt.intField("installExitCode")
			if ec != 0 {
				exitCode = 1
			}
			fmt.Fprintf(stdout, "configure: install exit %d, hash=%s, skipped=%v\n",
				ec, evt.stringField("installCommandHash"), evt.boolField("skipped"))
		case "error":
			fmt.Fprintf(stderr, "configure error during %s: %s\n",
				evt.stringField("phase"), evt.stringField("message"))
			exitCode = 1
		}
	}
	return exitCode
}

func buildConfigurePayload(install, entrypoint string, files []string) ([]byte, error) {
	body := map[string]any{}
	if install != "" {
		body["installCommand"] = splitArgv(install)
	}
	if entrypoint != "" {
		body["entrypoint"] = splitArgv(entrypoint)
	}
	if len(files) > 0 {
		out := make([]map[string]any, 0, len(files))
		for _, raw := range files {
			parts := strings.SplitN(raw, "=", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil, fmt.Errorf("--file %q must be path=local-path", raw)
			}
			contents, err := os.ReadFile(parts[1]) // #nosec G304 -- parts[1] is the local-path half of a --file path=local-path flag the caller explicitly asked to read
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", parts[1], err)
			}
			// Send as text when valid UTF-8; base64 otherwise. Matches the
			// SDK's encoding.
			entry := map[string]any{"path": parts[0]}
			if isValidUTF8(contents) {
				entry["content"] = string(contents)
			} else {
				entry["contentBase64"] = base64Encode(contents)
			}
			out = append(out, entry)
		}
		body["files"] = out
	}
	if len(body) == 0 {
		return nil, errors.New("nothing to do; pass at least one of --file, --install, --entrypoint")
	}
	return json.Marshal(body)
}

// splitArgv splits a shell-ish command string by whitespace. Good enough for
// our purposes — users who need quoting can run the SDK or hand-craft
// /configure with curl.
func splitArgv(s string) []string {
	return strings.Fields(s)
}

func isValidUTF8(b []byte) bool {
	for i := 0; i < len(b); {
		r, size := decodeRune(b[i:])
		if r == 0xFFFD && size == 1 {
			return false
		}
		i += size
	}
	return true
}

// decodeRune is a tiny wrapper so we don't pull in unicode/utf8 just for one
// call site. Returns the standard replacement rune on a malformed sequence.
func decodeRune(b []byte) (rune, int) {
	if len(b) == 0 {
		return 0xFFFD, 0
	}
	c := b[0]
	switch {
	case c < 0x80:
		return rune(c), 1
	case c&0xE0 == 0xC0 && len(b) >= 2:
		return rune(c&0x1F)<<6 | rune(b[1]&0x3F), 2
	case c&0xF0 == 0xE0 && len(b) >= 3:
		return rune(c&0x0F)<<12 | rune(b[1]&0x3F)<<6 | rune(b[2]&0x3F), 3
	case c&0xF8 == 0xF0 && len(b) >= 4:
		return rune(c&0x07)<<18 | rune(b[1]&0x3F)<<12 | rune(b[2]&0x3F)<<6 | rune(b[3]&0x3F), 4
	}
	return 0xFFFD, 1
}

func base64Encode(b []byte) string {
	const tbl = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var sb strings.Builder
	for i := 0; i < len(b); i += 3 {
		var v uint32
		switch {
		case i+2 < len(b):
			v = uint32(b[i])<<16 | uint32(b[i+1])<<8 | uint32(b[i+2])
		case i+1 < len(b):
			v = uint32(b[i])<<16 | uint32(b[i+1])<<8
		default:
			v = uint32(b[i]) << 16
		}
		out := []byte{
			tbl[(v>>18)&0x3F],
			tbl[(v>>12)&0x3F],
			tbl[(v>>6)&0x3F],
			tbl[v&0x3F],
		}
		switch len(b) - i {
		case 1:
			out[2] = '='
			out[3] = '='
		case 2:
			out[3] = '='
		}
		sb.Write(out)
	}
	return sb.String()
}

// --- `agenttier invoke` --------------------------------------------------

func runInvoke(args []string) int {
	fs := flag.NewFlagSet("invoke", flag.ContinueOnError)
	g := registerGlobalFlags(fs)
	var (
		prompt     string
		input      string
		timeoutStr string
		cancelID   string
	)
	fs.StringVar(&prompt, "prompt", "", `Convenience flag — appended to argv as --prompt=<value> and fed to stdin if --input is empty.`)
	fs.StringVar(&input, "input", "",
		`Body forwarded to the entrypoint on stdin. Inline string, "@/path/to/file" to read from disk, or "-" for stdin.`)
	fs.StringVar(&timeoutStr, "timeout", "",
		`Per-invoke server-side timeout (e.g. "5m"). Defaults to the template's defaultInvokeTimeout (30m if unset).`)
	fs.StringVar(&cancelID, "cancel", "",
		`Cancel an in-flight invoke by ID instead of starting a new one.`)

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "invoke requires <sandbox-id>")
		fs.Usage()
		return 2
	}
	sandboxID := fs.Arg(0)
	if err := g.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	if cancelID != "" {
		return runInvokeCancel(g, sandboxID, cancelID)
	}

	body, err := readInvokeBody(input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invoke: %v\n", err)
		return 1
	}

	q := url.Values{}
	if prompt != "" {
		q.Set("prompt", prompt)
	}
	if timeoutStr != "" {
		q.Set("timeout", timeoutStr)
	}
	path := "/sandboxes/" + url.PathEscape(sandboxID) + "/invoke"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	req, err := g.request(http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invoke: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Accept", "text/event-stream")

	// We use 0 (no client-side timeout) because the user controls the
	// server-side cap via --timeout, and the Router's keepalive comments
	// keep the connection live. The CLI exits when the server emits the
	// exit event.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invoke: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	warnIfDeprecated(resp)
	if resp.StatusCode/100 != 2 {
		return printErrorResponse("invoke", resp)
	}

	exitCode := 0
	for evt := range parseSSE(resp.Body) {
		switch evt.event {
		case "start":
			fmt.Fprintf(os.Stderr, "invoke started: %s\n", evt.stringField("invokeId"))
		case "log":
			stream := evt.dataField("stream", "stdout")
			line := evt.dataField("data", "")
			if stream == "stderr" {
				fmt.Fprintln(os.Stderr, line)
			} else {
				fmt.Fprintln(os.Stdout, line)
			}
		case "exit":
			exitCode = evt.intField("exitCode")
			reason := evt.stringField("reason")
			fmt.Fprintf(os.Stderr, "invoke %s: exit %d (duration %dms)\n",
				reason, exitCode, evt.intField("durationMs"))
		case "error":
			fmt.Fprintf(os.Stderr, "invoke error: %s\n", evt.stringField("message"))
			exitCode = 1
		}
	}
	return exitCode
}

func runInvokeCancel(g *globalFlags, sandboxID, invokeID string) int {
	body, _ := json.Marshal(map[string]string{"invokeId": invokeID})
	req, err := g.request(http.MethodPost,
		"/sandboxes/"+url.PathEscape(sandboxID)+"/invoke/cancel",
		bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "cancel: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cancel: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	warnIfDeprecated(resp)
	if resp.StatusCode == http.StatusNoContent {
		fmt.Fprintf(os.Stderr, "canceled %s\n", invokeID)
		return 0
	}
	return printErrorResponse("cancel", resp)
}

// readInvokeBody resolves the --input flag into a byte slice. Supports inline
// strings, "@/path" for files, and "-" for stdin.
func readInvokeBody(in string) ([]byte, error) {
	switch {
	case in == "":
		return nil, nil
	case in == "-":
		return io.ReadAll(os.Stdin)
	case strings.HasPrefix(in, "@"):
		return os.ReadFile(in[1:]) // #nosec G304 -- in[1:] is the path from --input @<path>, a flag the caller explicitly asked to read
	default:
		return []byte(in), nil
	}
}

// --- SSE parser ----------------------------------------------------------

type sseEvent struct {
	event string
	data  map[string]any
}

func (e sseEvent) stringField(name string) string {
	if e.data == nil {
		return ""
	}
	if v, ok := e.data[name].(string); ok {
		return v
	}
	return ""
}

func (e sseEvent) dataField(name, fallback string) string {
	v := e.stringField(name)
	if v == "" {
		return fallback
	}
	return v
}

func (e sseEvent) intField(name string) int {
	if e.data == nil {
		return 0
	}
	switch v := e.data[name].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

func (e sseEvent) boolField(name string) bool {
	if e.data == nil {
		return false
	}
	if v, ok := e.data[name].(bool); ok {
		return v
	}
	return false
}

// parseSSE returns a channel of events parsed from an SSE response body.
// Channel closes when the body is exhausted.
func parseSSE(r io.Reader) <-chan sseEvent {
	out := make(chan sseEvent, 8)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(r)
		// SSE messages can contain large data lines (file output), so bump
		// the per-token buffer well above the default 64 KiB.
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		event := "message"
		var dataLines []string
		flush := func() {
			if len(dataLines) == 0 {
				return
			}
			joined := strings.Join(dataLines, "\n")
			payload := map[string]any{}
			if err := json.Unmarshal([]byte(joined), &payload); err != nil {
				payload = map[string]any{"data": joined}
			}
			out <- sseEvent{event: event, data: payload}
			event = "message"
			dataLines = nil
		}
		for sc.Scan() {
			line := strings.TrimRight(sc.Text(), "\r")
			if line == "" {
				flush()
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(line[len("event:"):])
			} else if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			}
		}
		flush()
	}()
	return out
}

// printErrorResponse drains a non-2xx HTTP response, prints a useful error to
// stderr, and returns a process exit code.
// deprecationWarned tracks endpoints we've already warned about so a CLI
// deprecation notice fires at most once per process per endpoint.
var deprecationWarned sync.Map

// warnIfDeprecated prints a one-time stderr notice when the Router flags the
// endpoint as deprecated (Deprecation: true). Silence with
// AGENTTIER_DEPRECATION_WARNINGS=off.
func warnIfDeprecated(resp *http.Response) {
	if resp == nil || resp.Request == nil {
		return
	}
	if !strings.EqualFold(resp.Header.Get("Deprecation"), "true") {
		return
	}
	if strings.EqualFold(os.Getenv("AGENTTIER_DEPRECATION_WARNINGS"), "off") {
		return
	}
	key := resp.Request.Method + " " + resp.Request.URL.Path
	if _, seen := deprecationWarned.LoadOrStore(key, struct{}{}); seen {
		return
	}
	msg := "agenttier: API endpoint " + key + " is deprecated"
	if s := resp.Header.Get("Sunset"); s != "" {
		msg += " (sunset " + s + ")"
	}
	fmt.Fprintln(os.Stderr, msg)
}

func printErrorResponse(op string, resp *http.Response) int {
	body, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err == nil {
		if msg, ok := decoded["error"].(string); ok && msg != "" {
			fmt.Fprintf(os.Stderr, "%s: HTTP %d: %s\n", op, resp.StatusCode, msg)
			return 1
		}
	}
	fmt.Fprintf(os.Stderr, "%s: HTTP %d: %s\n", op, resp.StatusCode, strings.TrimSpace(string(body)))
	return 1
}
