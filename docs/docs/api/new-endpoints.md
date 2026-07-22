# New API endpoints (0.9.x)

This page documents the Router endpoints added for agent-first, programmatic
use: live sandbox mutation, backups, bulk operations, webhooks, and
sandbox-scoped API keys. All of them sit under the existing `/api/v1` prefix,
go through the same `authMiddleware` + `requireAdmin` gates as every other
route, and are purely additive — no existing endpoint's request/response
shape changed. See [API versioning](../api-versioning.md) for the
deprecation policy that governs future changes to this surface.

Every endpoint below has an SDK method — see [Python SDK](../sdk.md) — and a
Go/Python CLI command — see [CLI command reference](../cli-reference.md).

## `PATCH /api/v1/sandboxes/{id}` — live mutation

Partially update a running sandbox's idle timeout, resource requests/limits,
labels, or annotations without recreating it.

```json
PATCH /api/v1/sandboxes/my-sandbox
{
  "idleTimeout": "30m",
  "resources": {"requests": {"cpu": "1", "memory": "2Gi"}, "limits": {"cpu": "2", "memory": "4Gi"}},
  "labels": {"team": "platform"},
  "annotations": {"note": "bumped for load test"}
}
```

At least one field is required (400 otherwise). Response:

```json
{
  "sandboxId": "my-sandbox",
  "applied": {"idleTimeout": "immediately", "labels": "immediately", "resources": "on-restart"},
  "restartRequired": true,
  "message": "resource changes take effect after the sandbox is stopped and resumed"
}
```

**Why `resources` needs a restart:** the controller builds each sandbox's Pod
once, in `reconcileCreating`, and never diffs or rebuilds it against a later
spec change — there is no in-place container resize wired up (Kubernetes
1.27+'s `resize` subresource is not used here). `idleTimeout`, `labels`, and
`annotations` are read live by the reconcile loop or are plain object
metadata, so those three always report `"immediately"`. A `resources` change
is persisted immediately but only takes effect the next time the sandbox's
Pod is rebuilt — stop + resume, or an infra-failure auto-restart.

- **Auth**: owner or admin (same gate as stop/resume/delete).
- **Governance**: re-checked inline against the *value* caps only
  (`maxCpu`/`maxMemory`/`maxTimeout`/`maxIdleTimeout`) — a PATCH never
  changes the sandbox count, so the per-namespace/per-user count quotas
  (`maxSandboxesTotal`/`maxSandboxesPerUser`) are deliberately **not**
  re-evaluated here (that would spuriously reject a value-only PATCH once a
  namespace happens to already be at its count cap). A patch that would
  exceed a value cap gets the standard `policy_violation` 403 — see
  [Governance → Violations](../governance.md#violations).
- **Concurrency**: last-write-wins, no `resourceVersion` check — the same
  read-modify-`Update()` pattern every other spec-mutating handler in this
  Router uses (stop, share, template update).
- **Audit**: emits a `patch` audit event on success.

## Backups — `/api/v1/sandboxes/{id}/backups*`

A REST surface over the existing scheduled-VolumeSnapshot backup mechanism
(see [Backup and restore](../backup.md) for the underlying Layer 1
mechanism). All four endpoints use the same owner-or-admin RBAC as other
sandbox mutations.

| Method | Path | Effect |
| --- | --- | --- |
| `GET` | `/sandboxes/{id}/backups` | List this sandbox's backup + clone snapshots. |
| `POST` | `/sandboxes/{id}/backups` | Trigger an on-demand snapshot outside the scheduled interval. |
| `POST` | `/sandboxes/{id}/backups/{snapshotName}/restore` | Create a new sandbox cloned from the snapshot. |
| `DELETE` | `/sandboxes/{id}/backups/{snapshotName}` | Delete a specific snapshot. |

`GET` returns both the scheduler's own snapshots (`agenttier.io/snapshot-kind=scheduled-backup`)
and clone snapshots taken via `sandbox.clone()`, tagged by `kind`:

```json
{"backups": [
  {"name": "sbx-pvc-backup-1721234567", "kind": "scheduled-backup", "readyToUse": true, "createdAt": "2026-07-21T04:16:07Z"},
  {"name": "sbx-pvc-clone-1721200000", "kind": "clone", "readyToUse": true}
]}
```

`POST /backups` labels the new snapshot exactly like the scheduler's own
backups, so retention pruning still applies — an on-demand backup doesn't
bypass the configured `retentionDays`.

`POST /backups/{snapshotName}/restore` reuses the same construction as
`POST /sandboxes/{id}/clone`: it creates a new `Sandbox` with
`spec.cloneFromSnapshot` set, and re-checks governance the same way a normal
create does. If the snapshot was pruned between a `GET /backups` listing and
the restore call, this returns `404` (not a silent no-op) — mid-deletion
snapshots return `409` so the caller knows to retry rather than assume the
snapshot never existed. Both delete and restore verify the snapshot actually
belongs to the path's `{id}` sandbox before acting on it (a 404, not a leak,
for a snapshot name that belongs to someone else's sandbox in the same
namespace).

## Bulk operations — `/api/v1/sandboxes/bulk` and `/bulk-action`

Batch create or batch stop/resume/delete in one call, with per-item results
so a partial failure never loses the sandboxes that did succeed.

```json
POST /api/v1/sandboxes/bulk
{"items": [
  {"name": "worker-1", "templateRef": {"name": "general-coding", "kind": "ClusterSandboxTemplate"}},
  {"name": "worker-2", "templateRef": {"name": "general-coding", "kind": "ClusterSandboxTemplate"}}
]}
```

```json
{"results": [
  {"index": 0, "status": "created", "sandboxId": "worker-1"},
  {"index": 1, "status": "error", "error": "policy_violation: template \"bad-template\" is not in allowedTemplates"}
]}
```

```json
POST /api/v1/sandboxes/bulk-action
{"action": "stop", "ids": ["worker-1", "worker-2", "not-mine"]}
```

```json
{"results": [
  {"id": "worker-1", "status": "ok"},
  {"id": "worker-2", "status": "ok"},
  {"id": "not-mine", "status": "error", "error": "access denied"}
]}
```

**Two different failure models in the same call.** Per-item concerns — a
disallowed template, an over-cap resource request, an unknown or
someone-else's sandbox ID — are independent: one bad item never aborts its
siblings. The **governance sandbox-count cap** is the one exception: it is
evaluated once for the whole batch *before* anything is created, and a batch
that would exceed `maxSandboxesTotal`/`maxSandboxesPerUser`/`maxAgentSandboxes`
is rejected in full — nothing is created — with `409 quota_would_exceed`.
Shrink the batch and retry rather than relying on partial success to find
the cap. Past that gate, each item still goes through the same per-item
governance check (`allowedTemplates`, `approvedRegistries`, resource/timeout
value caps) that a single `POST /sandboxes` call would run.

**Rate limiting**: a bulk call of N items costs **N** rate-limit units, never
less — equivalent to N individual calls (`ratelimit.go`'s per-route cost
hook).

**Scoped keys are rejected outright** (403) on both endpoints — bulk
operations are not sandbox-scoped, regardless of which sandbox IDs appear in
the batch. See [Sandbox-scoped API keys](#sandbox-scoped-api-keys) below.

## Webhooks — `/api/v1/webhooks*`

Register a URL to receive sandbox lifecycle and related events instead of
polling. HMAC-signed, at-least-once delivery, driven by a leader-elected loop
in the **controller** (not the Router — the Router is stateless and
multi-replica, the wrong place to own a single delivery cursor).

**Delivery is opt-in at the Helm level.** Subscriptions can be created via
the API regardless of chart configuration, but nothing is delivered unless
`optional.webhooks.delivery.enabled: true` is set (default `false`, same
opt-in posture as scheduled backups). `optional.webhooks.delivery.intervalSeconds`
(default `30`) controls how often the delivery loop checks for new events.

| Method | Path | Effect |
| --- | --- | --- |
| `POST` | `/webhooks` | Create a subscription. Signing secret shown once. |
| `GET` | `/webhooks` | List the caller's own subscriptions. |
| `DELETE` | `/webhooks/{id}` | Delete a subscription (owner or admin). |
| `GET` | `/webhooks/{id}/deliveries` | Recent delivery attempts, for debugging. |

```json
POST /api/v1/webhooks
{"url": "https://example.com/hooks/agenttier", "eventTypes": ["sandbox.running", "sandbox.error"]}
```

```json
{
  "id": "wh-abc123", "url": "https://example.com/hooks/agenttier",
  "eventTypes": ["sandbox.running", "sandbox.error"],
  "secret": "base64url-32-bytes...",
  "warning": "Store this secret now — it is shown only once."
}
```

Event types (fixed vocabulary — enforced at creation, both server- and
client-side):

```
sandbox.creating  sandbox.running  sandbox.stopped  sandbox.error  sandbox.deleting
backup.created    backup.pruned
share.granted     share.revoked
agent.invoke.started  agent.invoke.completed  agent.invoke.failed
```

All 12 event types have a live source and are delivered: the `sandbox.*`
transitions are emitted by the controller's reconciler (`Recorder.Event`
calls against the Sandbox object); `backup.*`, `share.*`, and
`agent.invoke.*` are emitted by the Router via a shared
`Server.emitSandboxEvent` helper (`pkg/router/events.go`) — a plain
`corev1.Event` create through the Router's existing Kubernetes client, no
`EventRecorder`/broadcaster plumbing needed — called from the backup,
sharing, and agent-invoke handlers respectively. The controller's delivery
loop maps every one of these Event reasons to its webhook type
(`eventReasonToWebhookType`, `pkg/controller/webhook_delivery/events.go`).

**Delivery mechanics:**

- Each delivery is a `POST` to the subscription's URL with
  `X-AgentTier-Signature: sha256=<hex hmac(secret, raw-body)>`. Verify with
  `agenttier.webhooks.verify_signature(payload, header, secret)` (Python SDK)
  — never compare digests with `==`.
- Retried up to 5 attempts with exponential backoff (1s, 2s, 4s, 8s, 16s) on
  a non-2xx response or connection failure.
- A subscription with 15 consecutive delivery failures is **auto-disabled**
  — it stops being dispatched to until the owner re-creates it. This bounds
  queue growth if a receiver is permanently down (no unbounded retry queue).
  `GET /webhooks/{id}/deliveries` returns a bounded history (last 20 attempts
  per subscription) so the owner can see why.
- At-least-once delivery survives controller restarts and leader failover: a
  timestamp-based cursor (persisted in the `agenttier-webhook-cursor`
  ConfigMap) is only advanced *after* a full dispatch pass completes, so a
  crash mid-pass reprocesses that window on the next leader rather than
  skipping it.
- **SSRF guard**: subscription URLs are fully caller-controlled and the
  controller (broader network reach than a sandboxed workload) makes the
  outbound call, so the URL is validated at creation *and* re-validated
  immediately before every delivery attempt — `https://` only, and the
  resolved IP (post-DNS, not a hostname string match — this defeats DNS
  rebinding) must not fall in a loopback, link-local (including the
  `169.254.169.254` cloud metadata address), or RFC 1918 private range.

## Sandbox-scoped API keys

A narrower credential type than a full user-level API key: bound to exactly
one sandbox plus a set of action groups, so an agent running inside its own
sandbox can call back into the Router without holding its owner's full-access
key.

### Minting

`POST /user/api-keys` (the existing user-key mint endpoint) accepts two new
optional fields:

```json
POST /api/v1/user/api-keys
{"sandboxId": "my-sandbox", "scopes": ["run-command", "files:read"]}
```

Note the request field is `scopes`, not `actionGroups` — the response body
(and the persisted record) uses `actionGroups`, but the mint *request* uses
`scopes`. Since the Router's JSON decoder silently ignores unknown fields, a
request that mistakenly sends `actionGroups` here has no effect: it's
treated as if no scopes were requested at all.

Passing `sandboxId` with no `scopes` mints the **default** set:
`run-command`, `files:read`, `files:write`, `ports`, `agent:invoke`,
`agent:configure`, `resume`, `stop`. `scopes` without `sandboxId` is
rejected (400) — action-group scoping only applies to sandbox-scoped keys.

**Auto-minting.** In practice you rarely mint one of these by hand: the
controller auto-mints a scoped key for every sandbox at create time (mirroring
how the sandbox-runtime bearer token is already injected today) and makes it
available inside the pod as the environment variable
`AGENTTIER_SANDBOX_API_KEY`. The plaintext is never returned over the API to
the creator or written to logs — only the sandbox itself receives it, via a
Kubernetes Secret mounted as an env var.

### Action-group vocabulary

`run-command`, `files:read`, `files:write`, `ports`, `agent:invoke`,
`agent:configure`, `resume`, `stop`. **`delete` is not a member of this
vocabulary at all** — a scoped key can never destroy the sandbox that backs
it, by construction (mint-time rejects it with 400; there is also no route a
scoped key could reach that maps to a delete action, even if a record were
somehow hand-edited to include it).

### Enforcement

A request authenticated with a scoped key is checked, in order:

1. Is `<method> <path>` in the enforcement middleware's explicit allow-map?
   If not, **403** — every route not in the map is denied by default. This
   is deliberately "default-deny, not default-allow": a future route added
   to the Router without an explicit scoped-key decision fails closed.
2. Does the request's `{id}` path parameter match the key's bound sandbox?
   A mismatch is **403, not 404** — the caller already proved it holds a key
   for *some* sandbox, so there's nothing to hide by pretending the target
   doesn't exist.
3. Is the route's required action present in the key's action groups? If
   not, **403**.

Bulk endpoints, webhook endpoints, and admin/user-level endpoints are not in
the allow-map at all — a scoped key gets 403 on all of them regardless of
which sandbox IDs or action groups it carries. This holds even if the key is
exfiltrated and used from outside the sandbox's pod: the boundary is the key
itself, not the network origin.

### Revocation

Scoped keys are revoked automatically when their bound sandbox is deleted
(via the same finalizer cleanup path the sandbox-delete flow already uses).
A stopped-but-not-deleted sandbox's scoped key remains valid — its default
`resume` action group lets the agent bring its own sandbox back up without
the owner's intervention. Revocation deletes the underlying credential
record immediately; a Router replica that already cached the now-deleted
key's validation result may continue to accept it for up to that replica's
cache TTL (5 minutes) before the next lookup misses the cache and 404s — a
bounded, documented staleness window, not an unbounded gap.

### Admin visibility

`GET /admin/sandboxes` (admin-only) lists every sandbox cluster-wide and
joins in each sandbox's scoped API keys under a `scopedApiKeys` field —
metadata only (`id`/`sandboxId`/`actionGroups`/`createdAt`/etc via the same
shape as `GET /user/api-keys`), never the plaintext key or its hash. A
sandbox with no scoped key simply omits the field rather than returning an
error. Scoped-key metadata is also visible via `GET /user/api-keys` for the
key's own owner (see `decisions.md` DL10, resolved).
