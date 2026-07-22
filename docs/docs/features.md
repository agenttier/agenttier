# Features

What AgentTier ships today, grouped by what you probably need first.

## Declarative sandboxes

- **Kubernetes CRDs** — `Sandbox` (namespace-scoped), `SandboxTemplate` (namespace-scoped), `ClusterSandboxTemplate` (cluster-scoped). Manage sandboxes with `kubectl`, GitOps (Argo CD / Flux), or through the REST API, SDK, or Web UI.
- **State machine** — Creating → Running → Stopped → Running → Deleting, with an Error sink and Kubernetes Events at every transition so `kubectl describe sandbox` tells the full story.
- **Stop and resume** — Stop deletes the Pod while preserving the PVC. Resume re-attaches the same volume in about two seconds. Workspace contents, installed packages, and git state are exactly as left.
- **Cloning via VolumeSnapshot** — `POST /api/v1/sandboxes/{id}/clone` takes a CSI VolumeSnapshot of the source PVC, then creates a new sandbox whose PVC is hydrated from that snapshot. The clone inherits the source's spec (template, env, ports, agent harness) and its workspace contents byte-for-byte. See [Cloning](cloning.md).
- **Idle and max-runtime timeouts** — per-sandbox via `spec.idleTimeout` / `spec.timeout`, or per-namespace via governance caps. A configurable grace window notifies connected terminal sessions before auto-stop.
- **Self-healing** — restart on transient pod failures (OOM, preemption) with 10s / 20s / 40s / 80s / 160s exponential backoff. Permanent failure modes (image pull forever, config error) are surfaced on the sandbox `status.conditions`.

## Warm pod pool

- **Sub-second startup** — a leader-elected controller keeps N pre-provisioned Pods hot. When a user creates a sandbox, AgentTier claims one from the pool (measured 791 ms vs ~10 s cold).
- **Immediate PVC binding** — the warm pool uses a `gp3-immediate` StorageClass so the EBS volume is provisioned up-front; pod scheduling no longer waits on `WaitForFirstConsumer`.
- **Runtime reconfiguration** — change pool size or template through the Settings page; the controller picks it up from the `agenttier-warmpool-config` ConfigMap without a redeploy.

## Templates and agent harnesses

- **Field-level merge with inheritance** — `spec.inheritsFrom` chains templates up to depth 10. Sandbox spec overrides template spec overrides parent template overrides cluster defaults, one field at a time.
- **Harness config** — tell AgentTier which shell, tools, system prompt, and hooks to run. Hooks fire on start / idle / stop / resume.
- **Init scripts** — run cluster-approved setup commands before the container becomes Running (install extra tooling, clone a repo, wait for a service).
- **Embedded files** — templates can seed files into the workspace (e.g. a default `.tmux.conf`, a README, a code-of-conduct).
- **Reference images** — `general-coding` (Ubuntu + Node + Python + Go), `claude-code-bedrock` (Claude Code CLI wired to AWS Bedrock via IRSA), `openclaw-bedrock` (OpenClaw CLI wired to AWS Bedrock via IRSA), `strands-bedrock` (Strands Agents Python SDK wired to AWS Bedrock via IRSA), `langgraph-agent` (Python + LangGraph + LangChain for `mode: agent`), and `rl-rollout` (PyTorch + Ray RLlib + Gymnasium + Stable-Baselines3). All published on `ghcr.io/agenttier/sandbox-*`.

## Security and isolation

- **NetworkPolicy by default** — deny-all egress, allow DNS. Opt-in egress rules per template (e.g. "allow github.com and pypi.org"). Inter-sandbox peering is opt-in via label selectors.
- **Hardened pod defaults** — non-root, read-only root filesystem, drop all capabilities, `seccomp=RuntimeDefault`, per-sandbox ServiceAccounts with zero cluster permissions.
- **Kernel isolation** — optional gVisor RuntimeClass for untrusted workloads.
- **Per-session credentials** — STS AssumeRole or Kubernetes Secrets projected into the exec session at terminal open time (not baked into the image).
- **IRSA / Workload Identity** — zero long-lived cloud keys. IAM roles attach to the sandbox's ServiceAccount on EKS, Workload Identity does the same on GKE.
- **Signed container images** — every released image is cosign-signed with keyless OIDC (GitHub Actions identity). SPDX + CycloneDX SBOMs attached as OCI attestations. See [Verifying images](verifying-images.md).

## Interactive access

- **Browser terminal** — full PTY over WebSocket with xterm.js. Resize, ANSI colors, paste, copy, and a 30-second reconnection window for network blips.
- **Non-interactive exec** — `POST /api/v1/sandboxes/{id}/exec` returns `stdout` / `stderr` / `exitCode`. Matches how the SDK's `sandbox.exec()` is wired.
- **Hierarchical file browser** — Web UI Settings page lets you click into folders, breadcrumb back, download a single file, download a single folder as `.zip`, or download the entire workspace as `.zip`. The archive endpoint streams the tree directly: Router execs `tar` in the pod and re-encodes to zip on the fly via Go's `archive/zip`, so no in-pod `zip` binary is needed and large workspaces stream end-to-end without buffering. Locked to the `/workspace` subtree. Mirror surface in the SDK (`sandbox.files.archive(...)`) and CLI (`agenttier sandbox files archive ...`).
- **Port forwarding** — expose any container port with one click (Web UI) or one API call. AgentTier creates a ClusterIP Service, adds an Ingress when a preview domain is configured, and also offers an authenticated in-Router reverse proxy so users can reach ports even without DNS. See [Port forwarding](port-forwarding.md).

## Multi-tenancy and governance

- **OIDC + API keys** — Cognito, Okta, Azure AD, Auth0, Google — anything with a JWKS endpoint works. JWTs are verified against the provider's JWKS (RS256 signature, issuer, audience, expiry). API keys are minted on demand, stored as SHA-256 hashes with an LRU cache, and returned in plaintext exactly once. Auth fails closed: with no OIDC issuer configured, every API request is rejected with 401 unless the operator explicitly opts into `auth.devAuth: true` (local development only, never production).
- **Governance policies** — cluster-wide default + per-namespace overrides with field-level merge. Enforced synchronously at sandbox creation; violations return a structured `policy_violation` body with stable machine codes so UIs pinpoint the failing field. See [Governance](governance.md) for the full rule list.
- **Admission webhook (opt-in)** — closes the kubectl-bypass: a mutating admission webhook stamps `spec.createdBy` from the authenticated user and runs the same governance check at admission, so direct `kubectl apply` / GitOps writes can't forge ownership or skip policy. Requires cert-manager; fail-closed by default. See [Governance → Enforcement everywhere](governance.md).
- **Admin-gated editor** — `Settings → Governance` in the Web UI renders the active policies; only users with the admin claim can edit.
- **Audit trail** — lifecycle, terminal, credential, share, clone, and port-forward events recorded as Kubernetes Events. The Activity Log page filters on action, user, and time range. An optional SQL backend (phase 7.13) is planned for long-term retention.

## Web UI

- **Dashboard** — sandbox cards with status, template, age, one-click Stop / Resume / Delete / Open Terminal. Running cards also show an inline Port Forwards panel.
- **Templates editor** — in-browser YAML editor with syntax highlighting, create / save / delete, field validation.
- **Activity Log** — time-ordered events with filters.
- **Metrics** — live sandbox counts, average startup time, reconciliation queue depth.
- **Cost Estimator** — current monthly cost based on running resources.
- **Settings** — governance policies, warm pool sizing and template, operational defaults. Admin-gated.

## Agent-first API surface

- **Live sandbox mutation** — `PATCH /api/v1/sandboxes/{id}` updates idle timeout, resource requests/limits, labels, and annotations on a running sandbox without recreating it; the response reports whether each field applied immediately or needs a stop+resume. See [New API endpoints](api/new-endpoints.md#patch-apiv1sandboxesid-live-mutation).
- **Backup/restore API** — trigger on-demand backups, list, restore, and delete snapshots over REST instead of `kubectl` — a thin surface over the existing scheduled-VolumeSnapshot mechanism. See [Backup and restore](backup.md).
- **Bulk operations** — create or stop/resume/delete many sandboxes in one call, with per-item results so one bad item never loses its siblings; governance sandbox-count caps are checked fail-fast for the whole batch up front. See [Bulk operations](api/new-endpoints.md#bulk-operations-apiv1sandboxesbulk-and-bulk-action).
- **Webhooks** — HMAC-signed, at-least-once event subscriptions covering the full FR5.2 vocabulary (sandbox phase transitions, backup created/pruned, share granted/revoked, agent invoke started/completed/failed) so agents/orchestrators react instead of poll. Delivered by a leader-elected controller loop with retry/backoff and auto-disable. See [Webhooks](api/new-endpoints.md#webhooks-apiv1webhooks).
- **Sandbox-scoped API keys** — a credential narrower than a full user-level key: bound to one sandbox plus an explicit action-group set (never `delete`), auto-minted into every sandbox so an agent can call back into the Router without its owner's full-access key. See [Sandbox-scoped API keys](api/new-endpoints.md#sandbox-scoped-api-keys).
- **MCP server (optional)** — `pip install agenttier[mcp]` mirrors the Python SDK 1:1 as MCP tools, stdio and HTTP/SSE transports. See [SDK → MCP server](sdk.md#mcp-server-optional-extra).

## Client tooling

- **Python SDK** — `pip install agenttier`. Sync + async clients, typed Pydantic models, auto-detected auth, structured exception hierarchy, full coverage of sharing/governance/audit/analytics/admin/user/API-keys/warm-pool/cluster/webhooks/backups/bulk/PATCH. See [SDK](sdk.md).
- **CLI** — `agenttier`, in both a pure-Python distribution and a native Go binary (linux/macOS/Windows, amd64+arm64) at full command-family parity. See [CLI](cli.md) and [CLI command reference](cli-reference.md).
- **REST API** — sandboxes, templates, governance, port forwarding, audit, analytics, warm pool, identity, backups, bulk operations, webhooks, sandbox-scoped keys. Documented inline in [`pkg/router/server.go`](https://github.com/agenttier/agenttier/blob/main/pkg/router/server.go), the [new API endpoints](api/new-endpoints.md) page, and exercised by the SDK.

## Observability

- **OpenTelemetry** — distributed traces across the router and controller. Every HTTP request gets a server span (`router.GET`, `router.POST`); agent-mode `/configure` and `/invoke` get bounded-cardinality spans (`agenttier.configure`, `agenttier.invoke`) with `template`, `outcome`, and a non-reversible `actor_hash`; and every controller reconcile gets a `controller.reconcile_sandbox` span tagged with the sandbox's name, namespace, and phase, so a sandbox traces end-to-end across the API call, the reconcile, and the pod. Wires to any OTLP collector; the chart ships an opt-in collector for clusters without one. See [Observability](observability.md) for setup and backend integration.
- **Cluster capacity** — the Web UI Metrics page surfaces per-node allocatable vs. requested CPU/memory, a requests-based saturation percentage, and the managed node group (EKS / Karpenter / GKE / AKS) via the admin-only `GET /api/v1/cluster/nodes` endpoint, complementing the left-nav node/pod glance widget.
- **Trace-correlated logs** — slog JSON output stamps `trace_id` and `span_id` on every log line written under an active span context, so a single trace ID pivots between OTel UI and `kubectl logs` without any extra setup.
- **Prometheus** — `/metrics` exposes invoke + configure counters and histograms partitioned by template and outcome, plus rate-limit and throttling counters. Optional `ServiceMonitor` for Prometheus Operator (`observability.prometheus.serviceMonitor=true`).
- **Kubernetes Events** — every lifecycle transition emits a typed Event on the Sandbox resource so `kubectl describe sandbox` is a first-class debugging surface.
- **Startup logging** — `startupDurationMs` is logged per creation and recorded on an Event for regression tracking.

## Deployment and operations

- **Single Helm chart** — one `helm install agenttier agenttier/agenttier` deploys controller, router, web UI, CRDs, RBAC, and all opt-ins.
- **CRDs upgrade automatically** — the controller create-or-updates its bundled CRDs on startup, so `helm upgrade` (which never upgrades CRDs itself) makes new CRD fields usable immediately and fresh installs need no manual `kubectl apply`. Disable with `controller.manageCRDs=false` for GitOps-managed CRDs.
- **Multi-cluster** — works on EKS, GKE, AKS, kind, and any self-managed Kubernetes 1.27+ with NetworkPolicy-capable CNI.
- **Leader-elected HA** — multi-replica controller with Lease-based election. Graceful degradation for non-critical dependency failures (e.g. can't reach OTel collector).
- **Cluster autoscaling out of the box** — opt-in upstream Cluster Autoscaler installs cloud-neutral via Helm (works on EKS, GKE, AKS, OpenStack, Cluster API). Pair with the `headroom` Deployment for N+1 spare-node capacity: pause Pods at negative priority squat on a spare node, sandboxes preempt them instantly, evicted Pods trigger a fresh node in the background. See [Scaling](scaling.md) for sizing math + cost trade-offs.
- **Kubernetes-native state** — defaults to Kubernetes etcd + Events + ConfigMaps for all state. An optional SQL backend (Postgres / MySQL / SQLite) is on the roadmap for compliance-driven long-term retention.
- **Terraform** — EKS / GKE / AKS modules under [`terraform/`](https://github.com/agenttier/agenttier/tree/main/terraform) for fully-provisioned reference deployments.

## What is not here yet

Roadmap items that are *not* shipped in the current release and will return real errors or missing features if you rely on them:

- Sharing and collaboration (viewer/collaborator roles, expiring share links) — planned for 0.2.x.
- File transfer API — planned for 0.2.x.
- Email/Slack notification targets — webhooks (see [Webhooks](api/new-endpoints.md#webhooks-apiv1webhooks)) are the one delivery mechanism; there is no plan to add SMTP/Slack as first-class webhook targets beyond what a subscriber's own receiver can do with the payload.
- WebSocket ping frames + ALB migration — planned for 0.2.x; sessions through AWS Classic ELBs may still need manual reconnection every 60 minutes without the `connection-idle-timeout` annotation tweak.
- Optional SQL backend for audit + analytics long-term retention — planned for 0.3.x.

Track progress in the [GitHub issues](https://github.com/agenttier/agenttier/issues) or the `todo.md` file in the repo if you are contributing.
