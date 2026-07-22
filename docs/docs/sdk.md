# Python SDK

`pip install agenttier` gives you sync and async clients with typed Pydantic
models, auto-detected auth, streaming HTTP, and a typed exception hierarchy.
The same install also ships the [`agenttier` CLI](cli-reference.md) on your
`PATH`.

Source: [`python-sdk/`](https://github.com/agenttier/agenttier/tree/main/python-sdk).
PyPI: [pypi.org/project/agenttier](https://pypi.org/project/agenttier/).

## Install

```bash
pip install agenttier
```

Supported Python: 3.10, 3.11, 3.12, 3.13. Runtime deps are `httpx` and
`pydantic` — nothing else.

## Hello sandbox

```python
from agenttier import AgentTierClient

with AgentTierClient(api_url="https://agenttier.company.com") as client:
    sandbox = client.create_sandbox(template="general-coding", name="hello")
    sandbox.wait_until_running()

    result = sandbox.exec("uname -a")
    print(result.stdout.strip())
    print("exit:", result.exit_code)

    sandbox.terminate()
```

## Authentication

The SDK auto-detects credentials in priority order:

1. `AGENTTIER_API_KEY` env var → sent as `X-API-Key`.
2. `AGENTTIER_TOKEN` env var → sent as `Authorization: Bearer <token>` (OIDC JWT).
3. In-cluster ServiceAccount token at `/var/run/secrets/kubernetes.io/serviceaccount/token`.

4. Unauthenticated — accepted only when the Router is explicitly run with `--dev-auth` (Helm `auth.devAuth: true`). A Router with no OIDC issuer and no dev-auth rejects unauthenticated calls with 401.

Or pass an explicit provider:

```python
from agenttier import AgentTierClient, APIKeyAuth, BearerTokenAuth

# API key
client = AgentTierClient(
    api_url="https://agenttier.company.com",
    auth=APIKeyAuth("sk_live_abc123"),
)

# OIDC JWT you fetched yourself
client = AgentTierClient(
    api_url="https://agenttier.company.com",
    auth=BearerTokenAuth(jwt_string),
)
```

## Retries

By default the SDK fails fast on transient errors. Enable retries by passing a `RetryConfig`:

```python
from agenttier import AgentTierClient, RetryConfig

with AgentTierClient(
    api_url="https://agenttier.company.com",
    retry=RetryConfig(max_retries=3, backoff_factor=0.25, backoff_max=8.0),
) as client:
    ...
```

Behavior:

- Retries on **408, 429, 500, 502, 503, 504** by default. Override with `retry_status=frozenset({...})`.
- Retries on connection errors for idempotent methods (`GET`, `HEAD`, `OPTIONS`, `PUT`, `DELETE`). `POST` is retried only when `retry_post=True` since most Router endpoints are not idempotent.
- Honors the **`Retry-After`** header on `429` and `503` (capped at `backoff_max` so a misbehaving server can't park the SDK forever). Disable with `respect_retry_after=False`.
- Backoff is `min(backoff_max, backoff_factor * 2 ** attempt)` plus 0–25% random jitter so concurrent clients don't synchronize.
- **SSE-streaming endpoints** (`/configure`, `/invoke`) bypass retries entirely. Replaying half a stream would emit duplicate events; the SDK detects `Accept: text/event-stream` and passes the request through unchanged.

The same `retry=` parameter works on `AsyncAgentTierClient`.

## Async

```python
import asyncio
from agenttier import AsyncAgentTierClient

async def main():
    async with AsyncAgentTierClient(api_url="https://agenttier.company.com") as client:
        sandbox = await client.create_sandbox(
            template="general-coding", name="async-demo",
        )
        await sandbox.wait_until_running()
        result = await sandbox.exec("python -c 'print(2+2)'")
        print(result.stdout)
        await sandbox.terminate()

asyncio.run(main())
```

Both clients share the same exception hierarchy and model types.

## Agent mode

`mode: agent` sandboxes are driven through `sandbox.agent.configure(...)` and
`sandbox.agent.invoke(...)` instead of the terminal. The SDK ships sync and
async surfaces; both speak the Router's Server-Sent Events wire format
under the hood and return typed Pydantic models.

```python
from agenttier import AgentTierClient

with AgentTierClient(api_url="https://agenttier.company.com") as client:
    sb = client.create_sandbox(template="langgraph-agent", name="my-agent")
    sb.wait_until_running()

    # 1. Upload code + run install + persist the entrypoint
    sb.agent.configure(
        files=[
            ("/workspace/agent.py", "./agent.py"),  # local path
            {"path": "/workspace/requirements.txt", "content": "langgraph\nhttpx\n"},
        ],
        install_command=["pip", "install", "-r", "/workspace/requirements.txt"],
        entrypoint=["python", "/workspace/agent.py"],
        on_log=lambda stream, line: print(f"[install:{stream}] {line}"),
    )

    # 2. Invoke and aggregate the result
    result = sb.agent.invoke({"prompt": "summarize this PR"})
    print(result.stdout)
    print("exit:", result.exit_code, "duration:", result.duration_ms, "ms")

    # 3. Or stream events live for a richer UI
    for event in sb.agent.invoke_stream({"prompt": "..."}):
        if event.event == "log":
            print(event.data["data"])
        elif event.event == "exit":
            print("done:", event.data["exitCode"])
```

`configure` accepts:

- `files`: list of `(path, source)` tuples or `{"path": ..., "content": ...}` /
  `{"path": ..., "contentBase64": ...}` dicts. Tuple sources can be local paths
  (str / Path) or `bytes`. Binary input auto-base64s.
- `install_command`: argv list run once. Idempotent — re-running with the
  same files + command is a no-op.
- `entrypoint`: argv list persisted onto the sandbox so `invoke()` knows what
  to run.
- `on_log=callback(stream, line)`: optional live install logs.

`invoke` accepts a `payload` (dict / str / bytes), an optional `prompt`
(appended as `--prompt=` to argv and fed to stdin when payload is empty),
and an `invoke_timeout` Go duration string (e.g. `"5m"`) to lower the
server-side cap below the template's default.

`invoke_cancel(invoke_id)` terminates an in-flight invoke. Pair it with the
`invoke_id` from the first event of `invoke_stream`. Best-effort: returns
silently on success, raises `NotFoundError` if the invoke already
completed.

Async users get the same shapes via `await sandbox.agent.configure(...)`,
`await sandbox.agent.invoke(...)`, and `async for event in
sandbox.agent.invoke_stream(...)`.

See [agent-memory.md](agent-memory.md) for the three patterns AgentTier
supports for agent memory.

## API reference

### Clients

- `AgentTierClient(api_url, auth=None, timeout=30.0, verify=True)`
- `AsyncAgentTierClient(api_url, auth=None, timeout=30.0, verify=True)`

Use them as context managers (`with` / `async with`) so the underlying HTTPX
client closes cleanly. Methods:

| Method | Returns |
| --- | --- |
| `create_sandbox(template, name, namespace="default", timeout=None, idle_timeout=None, storage_size=None)` | `Sandbox` |
| `list_sandboxes(namespace=None, status=None)` | `list[SandboxSummary]` |
| `get_sandbox(sandbox_id)` | `Sandbox` |
| `list_templates()` | `list[Template]` |
| `get_template(name)` | `Template` |
| `current_user()` | `CurrentUser` (includes `is_admin` bit) |
| `sandboxes.create_bulk(items)` / `sandboxes.bulk_action(ids, action)` | `list[BulkCreateResultItem]` / `list[BulkActionResultItem]` |

### Resource-group sub-clients

Every other new REST surface hangs off a same-named attribute on the client,
sync and async alike — construct nothing directly, just call
`client.<group>.<method>()`:

| Attribute | Wraps | Key methods |
| --- | --- | --- |
| `client.governance` | `/governance/policies*`, `/governance/effective` | `list()`, `get(namespace)`, `set(policy, namespace=None)`, `delete(namespace)`, `effective(namespace=None)` — write ops admin-only |
| `client.analytics` | `/analytics/usage`, `/analytics/costs` | `usage()`, `costs()` — admin-only |
| `client.audit` | `/audit/events` | `list_events()` — admin-only |
| `client.admin` | `/admin/sandboxes`, `/admin/sharing` | `sandboxes()`, `sharing()` — admin-only |
| `client.user` | `/user/preferences` | `preferences_get()`, `preferences_set(dict)` |
| `client.api_keys` | `/user/api-keys*` | `list()`, `create(name=None, expires_in=None, sandbox_id=None, action_groups=None)`, `revoke(key_id)` |
| `client.warmpool` | `/warmpool/status`, `/warmpool/config` | `status()`, `set_config(pools)` — write admin-only |
| `client.cluster` | `/cluster/status`, `/cluster/nodes`, `/cluster/headroom` | `status()`, `nodes()` (admin), `headroom_get()`, `headroom_set(replicas, cpu=None, memory=None)` (admin) |
| `client.webhooks` | `/webhooks*` | `create(url, event_types, sandbox_id=None, namespace=None)`, `list()`, `delete(webhook_id)`, `deliveries(webhook_id)` |

`client.api_keys.create(sandbox_id=...)` mints a
[sandbox-scoped key](api/new-endpoints.md#sandbox-scoped-api-keys) instead of
a full-access user-level key — pass `action_groups` to narrow it below the
default set. `agenttier.webhooks.verify_signature(payload, header, secret)`
is a free function (not a client method) for verifying inbound webhook
deliveries on the *receiver* side — see
[Webhooks](api/new-endpoints.md#webhooks-apiv1webhooks) for the full
contract.

### Sandbox handle

```python
sandbox = client.create_sandbox(template="general-coding", name="demo")

# State
sandbox.status()                    # SandboxSummary
sandbox.phase                       # SandboxPhase enum
sandbox.wait_until_running(timeout=120.0)  # SandboxSummary

# Lifecycle
sandbox.stop()
sandbox.resume()
sandbox.terminate()   # alias: .delete()

# Execution
result = sandbox.exec("ls -la /workspace", timeout=30)
# CommandResult(stdout=..., stderr=..., exit_code=...)

# Port forwarding
fp = sandbox.forward_port(8080)
# ForwardedPort(port=8080, protocol='http', internal_url=..., preview_url=...)
sandbox.list_ports()
sandbox.remove_port(8080)

# Live mutation (PATCH) — idleTimeout/labels/annotations apply immediately;
# resources persist but need a stop+resume to take effect.
result = sandbox.update(idle_timeout="1h", labels={"team": "platform"})
result.restart_required  # bool
result.applied           # {"idleTimeout": "immediately", "labels": "immediately"}

# Backups
sandbox.backups.list()                       # list[BackupInfo]
backup = sandbox.backups.create()             # on-demand snapshot
restored = sandbox.backups.restore(backup.name)  # Sandbox, phase Pending
sandbox.backups.delete(backup.name)

# Sharing
sandbox.sharing.list()                        # SharingInfo
sandbox.sharing.grant("teammate@example.com", level="viewer")
sandbox.sharing.revoke("teammate@example.com")
link = sandbox.sharing.create_link(level="viewer", expires_in="24h")
```

The async counterpart `AsyncSandbox` has the same methods with `await` and the
same return types. `sandbox.update()` raises `ValueError` locally (before any
network call) if none of `idle_timeout`/`resources`/`labels`/`annotations` are
provided, and `PolicyViolationError` if the patch would exceed a governance
value cap. See [PATCH /api/v1/sandboxes/{id}](api/new-endpoints.md#patch-apiv1sandboxesid-live-mutation)
and [Backups](api/new-endpoints.md#backups-apiv1sandboxesidbackups) for the
full wire contracts.

## Models

Models are Pydantic v2 objects. Field names are snake_case in Python but
accept the Router's camelCase JSON transparently.

- `SandboxSummary` — `sandbox_id`, `name`, `namespace`, `status`, `phase`, `pod_name`, `pvc_name`, `template_ref`, `created_at`, `last_activity_at`, `created_by`, `message`.
- `SandboxPhase` — enum (`CREATING`, `RUNNING`, `STOPPED`, `ERROR`, `DELETING`, `UNKNOWN`).
- `CommandResult` — `stdout`, `stderr`, `exit_code`.
- `ForwardedPort` — `port`, `protocol`, `preview_url`, `internal_url`.
- `Template` — `name`, `description`, `image`, `resource_version`, `spec` (free-form dict).
- `CurrentUser` — `sub`, `email`, `name`, `groups`, `is_admin`.
- `CreatedBy` — `email`, `display_name`.
- `AuditEvent` — `timestamp`, `event_type`, `sandbox_id`, `sandbox_name`, `namespace`, `user_email`, `details`.
- `UsageAnalytics` — fleet-wide rollups.
- `SharePermission`, `SharingInfo`, `ShareLinkInfo`, `ShareLinkCreated` — sandbox sharing.
- `BackupInfo` — `name`, `created_at`, `kind` (`scheduled-backup` or `clone`), `ready_to_use`, `restore_size`.
- `Policy`, `NamespacePolicy`, `PolicyList`, `EffectivePolicy` — governance.
- `TemplateCost`, `CostEstimate` — analytics.
- `APIKeyMetadata`, `APIKeyCreated` — API keys, including `sandbox_id`/`action_groups` for scoped keys.
- `PoolConfig`, `PoolStatus`, `WarmPoolStatus` — warm pool.
- `ClusterStatus`, `NodeCapacity`, `NodeCapacityResponse`, `HeadroomConfig` — cluster.
- `WebhookSubscription`, `WebhookSubscriptionCreated`, `WebhookDelivery` — webhooks.
- `BulkCreateItem`, `BulkCreateResultItem`, `BulkActionResultItem`, `PatchResult` — bulk + PATCH.

## MCP server (optional extra)

`pip install agenttier[mcp]` adds an MCP server that mirrors the Python SDK
1:1 — one MCP tool per public SDK method, so the tool set and the SDK never
drift independently. Every fleet-wide tool (`governance_*`, `analytics_*`,
`audit_*`, `admin_*`, `warmpool_*`, `cluster_nodes`, `cluster_set_headroom`)
requires the underlying credential to carry admin privileges, same as the
REST endpoints they wrap.

Two transports, same tool set:

```bash
# stdio — for local agent runtimes that spawn the server as a subprocess
agenttier-mcp
# equivalently: python -m agenttier.mcp.stdio_main

# HTTP/SSE — for remote/hosted agent runtimes connecting over the network
agenttier-mcp-http --host 0.0.0.0 --port 8765
# equivalently: python -m agenttier.mcp.http_main --host 0.0.0.0 --port 8765
```

The HTTP binding defaults to `streamable-http` (the modern MCP HTTP
transport); pass `--transport sse` for the legacy SSE transport. Both console
scripts authenticate to the Router with the same credential types as the SDK
(`AGENTTIER_API_URL`/`AGENTTIER_API_KEY`/`AGENTTIER_TOKEN`) — a sandbox-scoped
key works here too, and tool calls that exceed its scope surface the same 403
an equivalent direct SDK call would. A malformed or missing required tool
argument surfaces as the SDK's own local-validation `ValueError`, not a raw
stack trace.

## Error handling

Every SDK error inherits from `AgentTierError`. Catch that to handle anything.

```python
from agenttier import (
    AgentTierClient,
    AgentTierError,
    AuthenticationError,
    AuthorizationError,
    PolicyViolationError,
    NotFoundError,
    ConflictError,
    SandboxTimeoutError,
    SandboxErrorState,
    APIError,
)

try:
    sandbox = client.create_sandbox(template="general-coding", name="demo")
    sandbox.wait_until_running(timeout=60)
except PolicyViolationError as e:
    for v in e.violations:
        print(f"Rejected by policy [{v['code']}]: {v['message']}")
except AuthenticationError:
    print("Bad credentials. Check AGENTTIER_API_KEY or AGENTTIER_TOKEN.")
except AuthorizationError:
    print("Authenticated but not authorized.")
except NotFoundError:
    print("Sandbox, template, or resource not found.")
except ConflictError:
    print("Operation is invalid for the current state.")
except SandboxTimeoutError:
    print("Sandbox didn't reach Running in time.")
except SandboxErrorState as e:
    print(f"Sandbox went to Error phase: {e}")
except APIError as e:
    print(f"HTTP {e.status_code}: {e}")
except AgentTierError as e:
    print(f"SDK error: {e}")
```

`PolicyViolationError.violations` is a list of `{"code": ..., "message": ...}`
dicts. The codes (`user_quota_exceeded`, `template_not_allowed`,
`cpu_limit_exceeded`, etc.) are stable — safe to branch on in UI code. See
[Governance](governance.md#violations) for the full list.

## Real-world snippets

### Run tests in a sandbox and download the result

```python
with AgentTierClient(api_url=API) as client:
    sbx = client.create_sandbox(template="general-coding", name="ci-run-123")
    sbx.wait_until_running()

    # Clone and test
    r = sbx.exec(
        "git clone https://github.com/org/repo /workspace/r && "
        "cd /workspace/r && make test > /tmp/test.log 2>&1; echo $?",
        timeout=300,
    )
    print("tests finished, exit:", r.stdout.strip())

    # Stream the log back via exec (file transfer API lands in 0.2.x).
    log = sbx.exec("cat /tmp/test.log").stdout
    with open("test.log", "w") as f:
        f.write(log)

    sbx.terminate()
```

### Orchestrate a handful of agents in parallel

```python
import asyncio
from agenttier import AsyncAgentTierClient

API = "https://agenttier.company.com"
TASKS = ["fix-auth-bug", "add-feature-x", "upgrade-deps"]

async def run_agent(client: AsyncAgentTierClient, name: str) -> str:
    sbx = await client.create_sandbox(template="claude-code-bedrock", name=name)
    try:
        await sbx.wait_until_running()
        r = await sbx.exec(
            f"claude -p 'Task: {name}. Work in /workspace, commit when done.'",
            timeout=1800,
        )
        return r.stdout
    finally:
        await sbx.terminate()

async def main():
    async with AsyncAgentTierClient(api_url=API) as client:
        results = await asyncio.gather(*[run_agent(client, t) for t in TASKS])
    for task, output in zip(TASKS, results):
        print(f"--- {task} ---\n{output[:500]}…")

asyncio.run(main())
```

### Preview a running web server through a forwarded port

```python
with AgentTierClient(api_url=API) as client:
    sbx = client.create_sandbox(template="general-coding", name="preview")
    sbx.wait_until_running()

    # Start a server inside the sandbox (fire-and-forget is just a timeout=1).
    sbx.exec("nohup python3 -m http.server 8080 > /tmp/srv.log 2>&1 &", timeout=5)

    port = sbx.forward_port(8080)
    print("Internal URL:", port.internal_url)
    print("Preview URL:", port.preview_url or "(set networking.previewDomain to enable public URL)")
```

## Command-line install

Quick one-off without a venv:

```bash
python3 -m pip install --user agenttier
python3 -c "from agenttier import __version__; print(__version__)"
```

For a pinned install in your own project, prefer an explicit range:

```toml
# pyproject.toml
[project]
dependencies = ["agenttier>=0.2.0,<0.3"]
```

## Upgrading the SDK

```bash
pip install --upgrade agenttier
```

The SDK follows the same version as the rest of the platform, but it is
versioned independently on PyPI. You can pair an older SDK with a newer server
as long as both support the endpoints you call; the SDK tolerates new server
fields gracefully thanks to Pydantic's `extra="ignore"` setting on every model.

## License

Apache-2.0.
