# Security

Security model, trust boundaries, and operational notes for AgentTier operators
and contributors. For the vulnerability disclosure process, see
[SECURITY.md](https://github.com/agenttier/agenttier/blob/main/SECURITY.md).

## CRD source of truth

**The controller manages its own CRDs.** On startup, the controller applies the
CRD manifests bundled in `pkg/crds/` (create-or-update). This means:

- A `helm upgrade` that bumps the controller image **automatically brings CRDs
  up to the running version** — no manual `kubectl apply -f config/crd/` step.
- The CRD definitions in `pkg/crds/` are the authoritative copy. They are
  generated from `api/v1alpha1/` via `make generate manifests` and must never
  be edited by hand.
- If you manage CRDs out-of-band (GitOps / Argo CD), set
  `controller.manageCRDs=false`. The controller's ServiceAccount then no longer
  needs `customresourcedefinitions` write access, and you are responsible for
  applying CRD updates after `helm upgrade`.

**Do not** run `kubectl apply -f config/crd/` directly on a cluster where the
controller has `manageCRDs=true` — the next controller start-up will reconcile
the CRDs back to its bundled version.

## In-cluster exec transport

When `harness.useHTTPExec: true` is set on a template, the Router dials the
in-pod sandbox-runtime directly over plain `http://pod-ip:9000` inside the
cluster. The connection is authenticated with a per-sandbox bearer token
(injected as a Kubernetes Secret), but the traffic is **not TLS-encrypted
between the Router Pod and the sandbox Pod**.

**Trust model:** the security guarantee relies on NetworkPolicy. Each sandbox
has a deny-all ingress policy; only the Router's Pod IP (matched by
`podSelector`) is allowed to reach port 9000. An attacker who can observe
intra-cluster traffic (compromised node, compromised CNI) could read exec
payloads in transit.

**Accepted risk:** full mTLS between the Router and sandbox-runtime is on the
roadmap. For sensitive workloads on multi-tenant nodes, prefer the SPDY exec
fallback (set `useHTTPExec: false`) or use gVisor RuntimeClass to reduce the
blast radius of a node compromise.

## Share-link brute-force

Share-link tokens are 256-bit random values compared in constant time. Brute
force is computationally infeasible at this bit-length. The raw token is
supplied as a `?token=` query parameter (or `Authorization: Bearer` header) when
accessing a shared sandbox; there is no rate limit on validation because the
token space makes brute force equivalent to attacking SHA-256 directly.

Share links are created via `POST /api/v1/sandboxes/{id}/share-links`. There is
no per-share-link revoke endpoint; if your threat model includes a compromised
database that leaks token prefixes, revoke the sandbox's user grants
(`DELETE /api/v1/sandboxes/{id}/share/{userId}`) or let the link expire, then
reissue.

## OIDC validator startup gap

If the OIDC issuer URL is unreachable when the Router starts, the OIDC
validator is `nil`. The router **fails closed** — every authenticated request
returns 401 until the validator initialises (it retries in the background).
This is correct behavior: no auth bypass is possible.

The observable symptom is that the Router Pod logs `OIDC validator not
initialised` and returns 401 to all requests. Fix by ensuring the OIDC issuer
is reachable from the Router Pod before or shortly after startup (DNS,
firewall, Cognito endpoint). A future release will add a readiness probe that
holds the Pod NotReady until the validator is live.

## gVisor installer image supply chain

The opt-in gVisor installer DaemonSet (`security.gvisor.installer.enabled=true`)
uses the image `gcr.io/gvisor/gvisor-installer:latest` by default
(`helm/agenttier/values.yaml`). The `:latest` tag is a floating reference —
an upstream change silently alters the installer that runs privileged on your
nodes.

**For production clusters:**

1. Pin to a specific digest: `gcr.io/gvisor/gvisor-installer@sha256:<digest>`
2. Override the Helm value: `--set security.gvisor.installer.image=gcr.io/gvisor/gvisor-installer@sha256:<digest>`
3. Use the alternative options (baked node AMI or manual install) which do not
   require a privileged DaemonSet at all — see [gVisor sandboxing](gvisor.md).

## Authentication and authorization

- **OIDC JWTs** are validated against the issuer's JWKS on every request (with
  LRU caching). The `iss` and `aud` claims are checked.
- **API keys** are stored as SHA-256 hashes in Kubernetes Secrets and compared
  in constant time. A key is returned in plaintext exactly once (at creation).
- **Sandbox-scoped API keys** narrow a key to one sandbox plus an explicit
  action-group set (never `delete`), auto-minted into every sandbox as
  `AGENTTIER_SANDBOX_API_KEY`. Enforcement is default-deny: the middleware's
  allow-map is the *only* path to a 200 for a scoped key, so a future route
  added without an explicit scoped-key decision fails closed rather than
  silently admitting one. A mismatch (wrong sandbox, or an action outside the
  key's groups) is 403, not 404 — the caller already proved it holds a key
  for some sandbox, so there's nothing to hide by pretending the target
  doesn't exist. See [Sandbox-scoped API keys](api/new-endpoints.md#sandbox-scoped-api-keys)
  for the full model.
- **Dev auth** (`auth.devAuth=true`) disables all authentication and treats
  every request as admin. It must never be enabled on a publicly-reachable
  Router. The controller logs a loud `AUTHENTICATION DISABLED — LOCAL DEV ONLY`
  warning on startup when this flag is set.
- **Admin routes** (`/admin/sandboxes`, `/admin/sharing`, cluster/namespace
  policy endpoints) are gated by `requireAdmin` middleware. Non-admin
  identities receive 403.

## Webhook subscription SSRF guard

Webhook subscription URLs ([Webhooks](api/new-endpoints.md#webhooks-apiv1webhooks))
are fully caller-controlled, and the controller — which has broader network
reach than any sandboxed workload — makes the outbound delivery call. Every
subscription URL is validated at creation and **re-validated immediately
before each delivery attempt**: `https://` only, and the resolved IP address
(after DNS resolution, not a hostname string match — a string match is
defeated by DNS rebinding) must not fall in a loopback, link-local (including
the `169.254.169.254` cloud metadata address), or RFC 1918 private range.

## Pod security defaults

Every sandbox Pod gets:

- Non-root user (`runAsNonRoot: true`)
- Read-only root filesystem (`readOnlyRootFilesystem: true`)
- All Linux capabilities dropped (`capabilities.drop: [ALL]`)
- `seccompProfile: RuntimeDefault`
- Per-sandbox ServiceAccount with zero cluster RBAC permissions
- A NetworkPolicy with deny-all ingress/egress (DNS always allowed; opt-in egress per template)

These defaults are set by the controller and cannot be weakened via the Sandbox
spec. Templates may tighten them further but not loosen them.

## AWS EKS deployment hardening

The `terraform/aws-eks` reference module (see its
[README](https://github.com/agenttier/agenttier/blob/main/terraform/aws-eks/README.md))
supports a hardened deployment posture on top of the defaults documented
above. These controls apply to the Kubernetes control plane and AWS
infrastructure, not the AgentTier application itself.

### EKS API endpoint modes

`var.endpoint_access_mode` controls how the cluster's Kubernetes API server is
reachable:

- **`public-restricted` (default):** the public endpoint stays enabled, but
  `cluster_endpoint_public_access_cidrs` must be a narrow allowlist —
  `0.0.0.0/0` is rejected by both a variable-level `validation` block and a
  belt-and-suspenders `precondition` on the cluster resource. This is a
  breaking default change from earlier module versions, which defaulted the
  CIDR list to the whole internet.
- **`private`:** the public endpoint is off entirely; only in-VPC principals
  (nodes, CodeBuild, an SSM tunnel) can reach the API. Requires
  `enable_codebuild = true` (also enforced by a `precondition`) because the
  on-cluster deploy steps (installing the AWS Load Balancer Controller and the
  AgentTier Helm chart, running the smoke test) have no other path to the
  cluster — they run inside a CodeBuild project configured with `vpc_config`
  instead of on the operator's laptop.

Creating a `private`-mode cluster itself works fine from a laptop —
`terraform apply` only ever talks to AWS APIs, never to the Kubernetes API
server directly (the `helm` and `kubernetes` Terraform providers were removed
from the module for exactly this reason; the Helm releases that used to run
during `terraform apply` now run from `deploy.sh` after the cluster exists).

### Scoped EKS Access Entries

The module replaces the blanket `enable_cluster_creator_admin_permissions`
convenience flag with an explicit `access_entries` map: the identity running
`terraform apply` (normalized from an STS session ARN to its underlying
role/user ARN via `aws_iam_session_context`, so an assumed-role caller still
gets a valid entry — an unnormalized session ARN is rejected by the EKS API
as an invalid Access Entry principal, which for a private-mode cluster with
no other admin path would leave the cluster orphaned) and, when
`enable_codebuild = true`, the CodeBuild deploy role. Both carry the
`AmazonEKSClusterAdminPolicy` cluster-scope association for v1;
namespace-scoping either principal further is a documented tightening
opportunity, not yet implemented.

### Control-plane logging and GuardDuty

- `cluster_enabled_log_types = ["api", "audit", "authenticator"]` ships a
  managed CloudWatch log group (`eks_log_retention_days`, default 14) so
  `terraform destroy` cleans it up instead of leaving an EKS-auto-created
  group behind. Audit logs can be a meaningful cost driver on a busy cluster —
  tune retention accordingly.
- `enable_guardduty_eks_protection` (default `false`) turns on GuardDuty's EKS
  Audit Log and Runtime Monitoring detectors. Left off by default because
  GuardDuty is frequently managed centrally at the organization/account level
  rather than per-workload; enabling it here when an org-wide detector already
  exists is redundant, not additive.

### MFA

The module does not mint any human-assumable IAM role, so there is no trust
policy to attach an `aws:MultiFactorAuthPresent` condition to. MFA enforcement
for the identity running `terraform apply` (and therefore holding the
`deployer` Access Entry) is the caller's own IAM/SSO configuration — Terraform
cannot retroactively require MFA on a pre-existing principal it didn't create.

### Network isolation

Nodes and the CodeBuild deploy actor run in private subnets behind a NAT
gateway; CodeBuild has no inbound rule at all (egress-only security group) and
reaches the private API endpoint via a dedicated ingress rule on the
EKS-managed cluster security group. Human operator access uses an SSM
Session Manager port-forward through a worker node — no bastion host, no
inbound rule, no SSH key. See [Port forwarding](port-forwarding.md) for the
full runbook.

## Supply chain

Every image published to `ghcr.io/agenttier/*` on a `v*` tag is:

- Keyless-signed with cosign using GitHub Actions' OIDC identity.
- Shipped with SPDX + CycloneDX SBOMs attached as OCI artifacts.
- Built from a pinned, digest-locked base image (see `docker/Dockerfile.controller`,
  `docker/Dockerfile.router`, `images/*/Dockerfile`).

See [Verifying images](verifying-images.md) for `cosign verify` and
`cosign verify-attestation` commands.
