# CLI

The `agenttier` CLI manages sandboxes and templates from the terminal. Two distributions, now at **full command-family parity** (as of 0.9.x) — pick based on runtime, not capability:

- **`pip install agenttier`** — pure-Python CLI installed alongside the SDK. Works on any platform with Python 3.10+.
- **GitHub Releases** — native Go binaries for Linux, macOS, and Windows on amd64 and arm64. No Python runtime required.

Both cover the same command families: `sandbox` (list/get/create/stop/resume/delete/clone/exec/wait/patch/bulk-create/bulk-action/files/ports/sharing/backups), `template`, `governance`, `audit`, `analytics`, `admin`, `user`, `apikeys`, `warmpool`, `cluster`, `webhooks`, plus the original `configure`/`invoke`/`version`. Both support `--output text|json`, `--api-url`/`--api-key`/`--token`, and a saved config file written by `agenttier login`. The Go CLI additionally has `~/.config/agenttier/config.json` support and a shared `pkg/agenttierclient` HTTP client under the hood — see [`pkg/agenttierclient`](https://github.com/agenttier/agenttier/tree/main/pkg/agenttierclient) if you're embedding it in Go tooling of your own.

Sources: [`cmd/cli/`](https://github.com/agenttier/agenttier/tree/main/cmd/cli) (Go), [`python-sdk/src/agenttier/cli.py`](https://github.com/agenttier/agenttier/tree/main/python-sdk/src/agenttier/cli.py) (Python).
Full command reference: [CLI command reference](cli-reference.md).

## Install

### Via `pip`

```bash
pip install agenttier
agenttier --version
```

This installs the SDK and the CLI together. The Python CLI exposes the full SDK surface (lifecycle, exec, files, port forwards, sharing, backups, bulk operations, governance, audit, analytics, admin, user preferences, API keys, warm pool, cluster status, webhooks, templates, agent-mode configure/invoke).

### Via GitHub Releases (Go binary)

Pick the binary for your OS/arch and drop it on your `PATH`.

### macOS / Linux

```bash
VERSION=v0.8.1  # replace with the latest release tag
OS=$(uname -s | tr A-Z a-z)
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
curl -L -o agenttier \
  "https://github.com/agenttier/agenttier/releases/download/${VERSION}/agenttier-${VERSION}-${OS}-${ARCH}"

# Verify the SHA256 (recommended)
curl -L -o agenttier.sha256 \
  "https://github.com/agenttier/agenttier/releases/download/${VERSION}/agenttier-${VERSION}-${OS}-${ARCH}.sha256"
sha256sum -c agenttier.sha256 || shasum -a 256 -c agenttier.sha256

chmod +x agenttier
sudo mv agenttier /usr/local/bin/
agenttier --version
```

### Windows

Download `agenttier-vX.Y.Z-windows-amd64.exe` from the release page and place
it somewhere on your `PATH`. PowerShell one-liner:

```powershell
$version = "v0.8.1"  # replace with the latest release tag
Invoke-WebRequest `
  -Uri "https://github.com/agenttier/agenttier/releases/download/$version/agenttier-$version-windows-amd64.exe" `
  -OutFile "$env:USERPROFILE\bin\agenttier.exe"
```

### Homebrew

A tap is planned; in the meantime use the curl flow above.

## Configuration

The CLI talks to the Router's REST API. Configure the endpoint and credentials
with environment variables (same as the SDK):

| Variable | Effect | CLI |
| --- | --- | --- |
| `AGENTTIER_API_URL` | Router base URL (`https://agenttier.company.com`). | both |
| `AGENTTIER_API_KEY` | Preferred auth; sent as `X-API-Key`. | both |
| `AGENTTIER_TOKEN` | OIDC bearer token; used if no API key. | both |
| `AGENTTIER_DEPRECATION_WARNINGS` | Set to `off` to silence one-time deprecation notices. | both |
| `AGENTTIER_CONFIG` | Override the saved config file path (default `~/.config/agenttier/config.json`, written by `agenttier login`). | both |

Example (Python CLI):

```bash
export AGENTTIER_API_URL=https://agenttier.company.com
export AGENTTIER_API_KEY=sk_live_...
agenttier sandbox list
```

## Usage

```bash
agenttier --help
agenttier --version
```

Both distributions cover the full command surface — see
[CLI command reference](cli-reference.md) for every subcommand and flag. One
difference remains: `agenttier sandbox files archive` (download the whole
`/workspace` tree as a `.zip`) is Python-CLI-only for now — the Go binary has
`ls`/`cat`/`upload`/`download`/`write` but no `archive` subcommand yet. For
either binary, `kubectl` on the CRDs directly (`sandboxes`, `templates`,
`clustersandboxtemplates`) remains available as a GitOps-friendly fallback.

## Agent mode (Phase 10)

Two commands drive `mode: agent` sandboxes from the shell:

### `agenttier configure <sandbox-id>`

Uploads files into the sandbox PVC, runs an install command, and records
the entrypoint. Idempotent: re-running with the same files + install command
short-circuits.

```bash
agenttier configure my-agent \
  --file /workspace/agent.py=./agent.py \
  --file /workspace/requirements.txt=./requirements.txt \
  --install "pip install -r /workspace/requirements.txt" \
  --entrypoint "python /workspace/agent.py"
```

| Flag | Meaning |
| --- | --- |
| `--file path=local-path` | Upload a file. Repeatable. Binary files auto-base64. |
| `--install "..."` | Argv string for the install command. Whitespace-split. |
| `--entrypoint "..."` | Argv string for the agent entrypoint. Whitespace-split. |

Install logs stream live to stdout / stderr. Exits 0 on success, 1 if the
install exited non-zero.

### `agenttier invoke <sandbox-id>`

Runs the configured entrypoint and streams its output. The CLI exits with
the same exit code as the entrypoint, so you can compose it in shell
pipelines.

```bash
# Inline prompt
agenttier invoke my-agent --prompt "what's the weather?"

# JSON body fed to the entrypoint on stdin
agenttier invoke my-agent --input '{"messages":[{"role":"user","content":"hi"}]}'

# Body from a file
agenttier invoke my-agent --input @./request.json

# Body from this shell's stdin
echo "from a pipe" | agenttier invoke my-agent --input -

# Cap the per-invoke timeout (overrides the template's defaultInvokeTimeout)
agenttier invoke my-agent --prompt "..." --timeout 5m

# Cancel an in-flight invoke (use the invokeId printed at the top of the stream)
agenttier invoke my-agent --cancel inv-abc123
```

| Flag | Meaning |
| --- | --- |
| `--prompt "..."` | Convenience: appended as `--prompt=<value>` to argv and fed to stdin if `--input` is empty. |
| `--input STR` | Body forwarded to the entrypoint on stdin. Inline string, `@/path` for a file, `-` for the CLI's own stdin. |
| `--timeout DURATION` | Server-side per-invoke timeout (Go duration string like `5m`). |
| `--cancel ID` | Cancel an in-flight invoke instead of starting a new one. |

Stdout from the entrypoint goes to your stdout, stderr to your stderr, and a
small status line ("`invoke started: inv-...`", "`invoke completed: exit 0`")
to stderr — so `agenttier invoke ... > out.txt 2>/dev/null` cleanly captures
just the agent's output.

## Why use the CLI vs `kubectl` or the SDK?

| Tool | Best for |
| --- | --- |
| `agenttier` CLI | Quick one-off commands, shell scripting, CI jobs. |
| `kubectl` on CRDs | GitOps, bulk operations, combining with other Kubernetes tooling. |
| Python SDK | Building tools, orchestrating multiple sandboxes, embedding into an agent framework. |
| Web UI | Humans exploring and debugging sandboxes. |

All four drive the same Router API and CRDs, so they are interchangeable at
any point — pick the right shape for the task.
