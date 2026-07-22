# CLI reference

Full command reference for the `agenttier` CLI. As of 0.9.x, both
distributions — the **Python CLI** (`pip install agenttier`, installed
alongside the SDK; any platform with Python 3.10+) and the **Go binary**
(native builds from GitHub Releases, `linux/darwin/windows × amd64/arm64`, no
Python runtime required) — cover the same command families: `sandbox`
(including `files`, `ports`, `sharing`, `backups`, `patch`, `bulk-create`,
`bulk-action`), `template`, `governance`, `audit`, `analytics`, `admin`,
`user`, `apikeys`, `warmpool`, `cluster`, `webhooks`, `configure`, `invoke`.

The two exceptions, called out inline below: `agenttier sandbox files
archive` (whole-workspace `.zip` download) exists only in the Python CLI, and
flag/output styling differs slightly (the Go binary's flag parser is
`flag.FlagSet`-based rather than `argparse`-based, though flag names match).
See [CLI](cli.md) for the two-distribution overview and
[`cmd/cli/`](https://github.com/agenttier/agenttier/tree/main/cmd/cli) for the
Go binary's source, or
[`python-sdk/src/agenttier/cli.py`](https://github.com/agenttier/agenttier/tree/main/python-sdk/src/agenttier/cli.py)
for the Python source.

For tutorials, see [Web UI walkthrough](tutorials/web-ui.md), [Python SDK walkthrough](tutorials/python-sdk.md), and [Agent mode in depth](tutorials/agent-mode-tutorial.md).

## Synopsis

```
agenttier <command> [<subcommand>] [flags] [arguments]
```

## Global flags

Available on every command in this reference, on **both** distributions —
the Go binary reached full flag/command parity with the Python CLI in
0.9.x (previously it only recognized `--api-url`/`--api-key` on `configure`/
`invoke`; see [CLI](cli.md) for the small remaining differences).

| Flag | Env var | Description |
| --- | --- | --- |
| `--api-url <URL>` | `AGENTTIER_API_URL` | Router base URL. Required unless saved by `agenttier login`. |
| `--api-key <KEY>` | `AGENTTIER_API_KEY` | Static API key. Sent as `X-API-Key`. |
| `--token <JWT>` | `AGENTTIER_TOKEN` | Bearer token / OIDC JWT. Sent as `Authorization: Bearer ...`. |
| `--output text\|json` | — | Output format. `text` is human-friendly tables; `json` is machine-readable. Default: `text`. |
| `-h`, `--help` | — | Show help for the current command. |
| `--version` | — | Print CLI version. |

Configuration precedence: CLI flag → environment variable → `~/.config/agenttier/config.json` (set by `agenttier login`).

## Commands

### `agenttier version`

Print the CLI version string.

```
agenttier version
```

### `agenttier login`

Save endpoint and credentials to `~/.config/agenttier/config.json` (mode 0600). Verifies the configuration by calling `/user/me`.

```
agenttier login --api-url https://agenttier.example.com [--api-key <KEY> | --token <JWT>]
```

Flags: global only. The path is overridable via `AGENTTIER_CONFIG`.

### `agenttier whoami`

Print the server's view of the authenticated identity. Python CLI only — the
Go CLI exposes the same call as [`agenttier user whoami`](#agenttier-user)
instead.

```
agenttier whoami [--output text|json]
```

Output: `email (admin)` for admin users, `email` otherwise. JSON mode returns the full `CurrentUser` model.

### `agenttier sandbox`

Manage sandboxes. Most operations work on the sandbox ID (DNS-friendly name).

#### `agenttier sandbox list`

List sandboxes visible to the caller.

```
agenttier sandbox list [--namespace <NS>] [--status Running|Stopped|Error|...]
```

Output columns: ID, NAME, STATUS, TEMPLATE, NAMESPACE.

#### `agenttier sandbox get`

Show one sandbox.

```
agenttier sandbox get <sandbox-id>
```

#### `agenttier sandbox create`

Create a sandbox from a `ClusterSandboxTemplate`.

```
agenttier sandbox create <name> --template <name> \
  [--namespace <NS>] \
  [--timeout <DUR>] [--idle-timeout <DUR>] \
  [--storage-size <SIZE>] \
  [--wait] [--wait-timeout <SECONDS>]
```

| Flag | Description |
| --- | --- |
| `<name>` | Sandbox name (DNS-friendly: lowercase alphanumeric and hyphens). |
| `--template` | Required. Cluster-scoped template name (e.g. `general-coding`). |
| `--namespace` | Default: `default`. |
| `--timeout` | Max-runtime duration (Go format, e.g. `8h`, `24h`). |
| `--idle-timeout` | Auto-stop after this much inactivity (e.g. `30m`). |
| `--storage-size` | PVC size (e.g. `10Gi`, `50Gi`). |
| `--wait` | Block until sandbox reaches `Running`. Python CLI only — the Go CLI has no `--wait`/`--wait-timeout` on `create`; use the separate `agenttier sandbox wait` subcommand below instead. |
| `--wait-timeout` | Wait timeout in seconds. Default: `180`. Python CLI only (see `--wait`). |

#### `agenttier sandbox stop`

Stop a sandbox. The Pod is deleted; the PVC is preserved.

```
agenttier sandbox stop <sandbox-id>
```

#### `agenttier sandbox resume`

Resume a stopped sandbox. The PVC is re-attached to a fresh Pod.

```
agenttier sandbox resume <sandbox-id>
```

#### `agenttier sandbox delete`

Permanently delete a sandbox and its PVC.

```
agenttier sandbox delete <sandbox-id>
```

#### `agenttier sandbox exec`

Run a one-shot command and stream output. The CLI exits with the command's exit code.

```
agenttier sandbox exec <sandbox-id> [--timeout <SECONDS>] -- <command> [args...]
```

Examples:

```bash
agenttier sandbox exec demo -- bash -lc 'echo $USER'
agenttier sandbox exec demo --timeout 60 -- pytest tests/
```

#### `agenttier sandbox wait`

Block until the sandbox reaches `Running`.

```
agenttier sandbox wait <sandbox-id> [--timeout <SECONDS>]
```

#### `agenttier sandbox files`

File operations. Each operation goes through the Router with a 32 MiB per-call cap.

| Subcommand | Synopsis |
| --- | --- |
| `ls` | `agenttier sandbox files ls <id> [--path /workspace]` |
| `cat` | `agenttier sandbox files cat <id> <path>` |
| `upload` | `agenttier sandbox files upload <id> <local> <remote>` |
| `download` | `agenttier sandbox files download <id> <remote> <local>` |
| `write` | `agenttier sandbox files write <id> <path> --data "..."` or `--file <local>` (use `-` for stdin) |

Examples:

```bash
agenttier sandbox files ls demo --path /workspace
agenttier sandbox files upload demo ./script.py /workspace/script.py
agenttier sandbox files cat demo /workspace/output.txt > local.txt
echo "hello" | agenttier sandbox files write demo /workspace/note.txt --file -
```

#### `agenttier sandbox ports`

Manage port forwards. The Router proxies the in-pod port at `/api/v1/sandboxes/<id>/preview/<port>/`. When `networking.previewDomain` is set in Helm, an Ingress preview URL is also returned.

| Subcommand | Synopsis |
| --- | --- |
| `list` | `agenttier sandbox ports list <id>` |
| `forward` | `agenttier sandbox ports forward <id> --port <N> [--protocol http\|tcp]` |
| `remove` | `agenttier sandbox ports remove <id> --port <N>` |

#### `agenttier sandbox patch`

Live-mutate a running sandbox — see [PATCH /api/v1/sandboxes/{id}](api/new-endpoints.md#patch-apiv1sandboxesid-live-mutation) for the full contract.

```
agenttier sandbox patch <sandbox-id> \
  [--idle-timeout <DUR>] \
  [--cpu-request <CPU>] [--memory-request <MEM>] [--cpu-limit <CPU>] [--memory-limit <MEM>] \
  [--label <key>=<value>]... [--annotation <key>=<value>]...
```

At least one flag is required. `--label`/`--annotation` are repeatable and merge into (not replace) the sandbox's existing labels/annotations. Prints each field's `applied` status (`immediately` or `on-restart`); if any field is `on-restart`, also prints the accompanying message.

```bash
agenttier sandbox patch demo --idle-timeout 1h --label team=platform
agenttier sandbox patch demo --cpu-limit 2 --memory-limit 4Gi   # resources: on-restart
```

#### `agenttier sandbox bulk-create`

Create multiple sandboxes from one JSON array in a single call — see [Bulk operations](api/new-endpoints.md#bulk-operations-apiv1sandboxesbulk-and-bulk-action).

```
agenttier sandbox bulk-create --file <path-or-'-'>
```

`--file` takes a JSON array of create specs (same shape as `sandbox create`'s
fields); `-` reads from stdin. Prints a per-item `INDEX STATUS SANDBOX_ID ERROR` table (or the raw JSON with `--output json`).

```bash
echo '[{"name":"w1","templateRef":{"name":"general-coding","kind":"ClusterSandboxTemplate"}},
       {"name":"w2","templateRef":{"name":"general-coding","kind":"ClusterSandboxTemplate"}}]' \
  | agenttier sandbox bulk-create --file -
```

#### `agenttier sandbox bulk-action`

Apply `stop`, `resume`, or `delete` to multiple sandbox IDs in one call.

```
agenttier sandbox bulk-action --action <stop|resume|delete> <sandbox-id>...
```

Per-item results are independent — an unknown ID or another user's sandbox reports as a per-item error, not a whole-batch abort.

```bash
agenttier sandbox bulk-action --action stop worker-1 worker-2 worker-3
```

#### `agenttier sandbox sharing`

Manage sandbox sharing grants and share links. See [Sharing](sdk.md#sandbox-handle) for the SDK equivalent.

| Subcommand | Synopsis |
| --- | --- |
| `list` | `agenttier sandbox sharing list <id>` |
| `grant` | `agenttier sandbox sharing grant <id> <identity> [--level viewer\|collaborator] [--kind user\|group]` |
| `revoke` | `agenttier sandbox sharing revoke <id> <identity>` |
| `create-link` | `agenttier sandbox sharing create-link <id> [--level viewer\|collaborator] [--expires-in <DUR>] [--max-uses <N>]` |

`create-link` prints the share token exactly once — it cannot be retrieved again.

#### `agenttier sandbox backups`

Trigger, list, restore, and delete backup snapshots — see [Backups](api/new-endpoints.md#backups-apiv1sandboxesidbackups) for the full contract and [Backup and restore](backup.md) for the underlying mechanism.

| Subcommand | Synopsis |
| --- | --- |
| `list` | `agenttier sandbox backups list <id>` |
| `create` | `agenttier sandbox backups create <id> [--snapshot-class <class>]` (Go CLI also accepts `--name`, which the Router ignores for create — it only applies to `restore`) |
| `restore` | `agenttier sandbox backups restore <id> <snapshot-name> [--name <new-sandbox-name>]` |
| `delete` | `agenttier sandbox backups delete <id> <snapshot-name>` |

```bash
agenttier sandbox backups create demo
agenttier sandbox backups list demo
agenttier sandbox backups restore demo demo-pvc-backup-1721234567 --name demo-restored
```

### `agenttier template`

Inspect cluster-scoped templates.

| Subcommand | Synopsis |
| --- | --- |
| `list` | `agenttier template list` — outputs NAME, IMAGE, DESCRIPTION |
| `get` | `agenttier template get <name>` — full template spec as JSON |

### `agenttier governance`

Manage governance policies. `set`/`delete` are admin-only on the Router side. See [Governance](governance.md) for the full policy field list and violation codes.

| Subcommand | Synopsis |
| --- | --- |
| `list` | `agenttier governance list` — cluster default + every namespace override |
| `get` | `agenttier governance get <namespace>` |
| `set` | `agenttier governance set [--namespace <ns>] --max-sandboxes-per-user <N> --max-sandboxes-total <N> --max-agent-sandboxes <N> --max-cpu <CPU> --max-memory <MEM> --max-storage <SIZE> --max-timeout <DUR> --max-idle-timeout <DUR>` (Go CLI; Python CLI takes `--file <path\|->` with a JSON policy body instead) — omit `--namespace` to set the cluster-wide default |
| `delete` | `agenttier governance delete <namespace>` — removes the override, falls back to cluster default |
| `effective` | `agenttier governance effective [--namespace <ns>]` — the resolved policy actually enforced for that namespace |

### `agenttier audit`

Admin-only. Lists Kubernetes-Event-backed audit events across all namespaces.

```
agenttier audit [--output text|json]
```

### `agenttier analytics`

Admin-only fleet-wide rollups.

| Subcommand | Synopsis |
| --- | --- |
| `usage` | `agenttier analytics usage` |
| `costs` | `agenttier analytics costs` |

### `agenttier admin`

Admin-only cross-tenant views.

| Subcommand | Synopsis |
| --- | --- |
| `sandboxes` | `agenttier admin sandboxes` — every sandbox across every namespace |
| `sharing` | `agenttier admin sharing` — every sharing grant across every namespace |

### `agenttier user`

| Subcommand | Synopsis |
| --- | --- |
| `whoami` | `agenttier user whoami [--output text\|json]` — Go CLI only; the Python CLI exposes the same call as the top-level [`agenttier whoami`](#agenttier-whoami) instead, with no `user whoami` alias |
| `preferences-get` | `agenttier user preferences-get` |
| `preferences-set` | `agenttier user preferences-set --file <path\|->` — JSON object of preferences (Python CLI; Go CLI takes `--json <inline-string>` instead of `--file`) |

### `agenttier apikeys`

Manage your own API keys, including [sandbox-scoped keys](api/new-endpoints.md#sandbox-scoped-api-keys).

| Subcommand | Synopsis |
| --- | --- |
| `list` | `agenttier apikeys list` — metadata only, never the plaintext or hash |
| `create` | `agenttier apikeys create [--name <label>] [--expires-in <DUR>] [--sandbox-id <id>] [--action-groups <g1,g2,...>]` |
| `revoke` | `agenttier apikeys revoke <key-id>` |

Omit `--sandbox-id` for a full-access user-level key. `--action-groups` requires `--sandbox-id` and defaults to `run-command,files:read,files:write,ports,agent:invoke,agent:configure,resume,stop` when omitted; `delete` is never a valid action group. The plaintext key is printed exactly once at creation — it cannot be retrieved again.

```bash
agenttier apikeys create --name "ci-bot" --expires-in 720h
agenttier apikeys create --sandbox-id my-sandbox --action-groups run-command,files:read
```

### `agenttier warmpool`

| Subcommand | Synopsis |
| --- | --- |
| `status` | `agenttier warmpool status` |
| `set-config` | `agenttier warmpool set-config --file <path\|->` (admin-only) — JSON array of `{"template":"...","desiredCount":N}` |

### `agenttier cluster`

| Subcommand | Synopsis |
| --- | --- |
| `status` | `agenttier cluster status` — node/pod headcount |
| `nodes` | `agenttier cluster nodes` (admin-only) — per-node capacity + fleet-wide saturation |
| `headroom-get` | `agenttier cluster headroom-get` |
| `headroom-set` | `agenttier cluster headroom-set --replicas <N> [--cpu <CPU>] [--memory <MEM>]` (admin-only) |

### `agenttier webhooks`

Manage webhook subscriptions — see [Webhooks](api/new-endpoints.md#webhooks-apiv1webhooks) for event types, delivery/retry mechanics, and the HMAC signature format.

| Subcommand | Synopsis |
| --- | --- |
| `create` | `agenttier webhooks create --url <https-url> --event-types <t1,t2,...> [--sandbox-id <id>] [--namespace <ns>]` |
| `list` | `agenttier webhooks list` — your own subscriptions only |
| `delete` | `agenttier webhooks delete <id>` |
| `deliveries` | `agenttier webhooks deliveries <id>` — recent delivery attempts, for debugging |

`--url` must be `https://`; `--event-types` is validated locally against the fixed vocabulary before the network round-trip. The signing secret is printed exactly once at creation.

```bash
agenttier webhooks create --url https://example.com/hooks/agenttier --event-types sandbox.running,sandbox.error
```

### `agenttier configure`

Configure an agent-mode sandbox. Uploads files, runs an install command, and registers the entrypoint. Streams install logs as Server-Sent Events while the command runs.

```
agenttier configure <sandbox-id> \
  [--file <remote>=<local>]... \
  [--install "<command>"] \
  [--entrypoint "<command>"]
```

| Flag | Description |
| --- | --- |
| `--file` | Upload one local file. Repeatable. Format: `remote-path=local-path`. |
| `--install` | Install command, run once. Idempotent — re-runs with the same files + command short-circuit. |
| `--entrypoint` | Command run on every `/invoke`. Persisted in `Sandbox.status.agentConfigure`. |

At least one of `--file`, `--install`, `--entrypoint` is required. The sandbox must be `mode: agent` (rejected with HTTP 400 otherwise).

Example:

```bash
agenttier configure my-agent \
  --file /workspace/agent.py=./agent.py \
  --file /workspace/requirements.txt=./requirements.txt \
  --install "pip install -r /workspace/requirements.txt" \
  --entrypoint "python /workspace/agent.py"
```

### `agenttier invoke`

Run the configured entrypoint and stream stdout / stderr / exit as Server-Sent Events. The CLI exits with the entrypoint's exit code.

```
agenttier invoke <sandbox-id> \
  [--prompt "<text>"] \
  [--input "<inline string>" | "@/path/to/file" | "-"] \
  [--timeout <DUR>] \
  [--cancel <invoke-id>]
```

| Flag | Description |
| --- | --- |
| `--prompt` | Convenience flag — sent as `?prompt=<value>` query param. The entrypoint can read this from argv as `--prompt=<value>`. |
| `--input` | Body forwarded to the entrypoint on stdin. Inline string, `@/path/to/file` to read from disk, or `-` for stdin. |
| `--timeout` | Server-side per-invoke wall-clock cap (Go duration format, e.g. `5m`, `30s`). Defaults to the template's `defaultInvokeTimeout` (30 min if unset). |
| `--cancel <id>` | Cancel an in-flight invoke instead of starting a new one. The invoke ID is printed at the start of every invoke. |

Stdin is accepted in two ways: `--input "literal"` or piping into the CLI with `--input -`. JSON bodies are passed through unchanged so frameworks that expect typed dicts (LangGraph, Strands) can decode directly.

Examples:

```bash
# Plain prompt
agenttier invoke my-agent --prompt "summarize this PR"

# JSON body via inline string
agenttier invoke my-agent --input '{"prompt":"hello","temperature":0.7}'

# JSON body from file
agenttier invoke my-agent --input @./body.json

# Pipe stdin
echo '{"prompt":"hello"}' | agenttier invoke my-agent --input -

# Cancel an in-flight invoke
agenttier invoke my-agent --cancel inv-7ca109acefc16b71e8c99c03

# Cap server-side runtime to 5 minutes
agenttier invoke my-agent --prompt "long task" --timeout 5m
```

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Success. |
| `1` | Operational error (HTTP 4xx / 5xx, sandbox not found, file I/O, etc.). |
| `2` | Usage error (bad flag, missing required argument). |
| `130` | Interrupted by Ctrl+C. |
| _other_ | For `sandbox exec` and `invoke`: the underlying command's exit code. |

## Output formats

Every command supports `--output json` for scriptable use:

```bash
agenttier sandbox list --output json | jq '.[] | select(.status == "Running") | .name'
agenttier template get general-coding --output json | jq .spec.image
```

In `json` mode, fields are serialized in snake_case from the SDK's pydantic models. The Router responds in camelCase; the SDK normalizes both directions.

## Configuration file

`agenttier login` writes `~/.config/agenttier/config.json` (mode 0600):

```json
{
  "api_url": "https://agenttier.example.com",
  "api_key": "ak_...",
  "token": "..."
}
```

Override the path with `AGENTTIER_CONFIG=/path/to/config.json`.

## Troubleshooting

- **`agenttier: no API URL configured`** — set `--api-url`, `AGENTTIER_API_URL`, or run `agenttier login`.
- **`HTTP 401`** — pass `--api-key` or `--token` (or run `agenttier login` to save credentials).
- **`HTTP 400: sandbox is in mode "code"`** — `/configure` and `/invoke` are agent-mode only. Create with `--template <agent-template>` or use `sandbox exec` for code-mode.
- **`HTTP 429`** — `maxConcurrentInvokes` cap hit. The error body includes the `limit` and `Retry-After` seconds.
