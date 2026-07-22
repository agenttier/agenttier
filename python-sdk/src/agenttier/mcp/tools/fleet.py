# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""MCP tools mirroring the fleet-level (non-sandbox-scoped) sub-clients:
governance, analytics, audit, admin, user preferences, API keys, warm pool,
cluster status, and bulk sandbox operations.

Most of these are admin-only on the Router side (governance writes,
analytics, audit, admin views, warm pool config, cluster nodes/headroom);
a non-admin caller gets an ``AuthorizationError`` (surfaced as a tool error
by the MCP framework, per FR7.3's "surface SDK errors as tool errors").
"""

from __future__ import annotations

from typing import Any, Optional, Sequence

from agenttier.bulk import BulkCreateItem
from agenttier.governance import Policy
from agenttier.mcp._client import get_client
from agenttier.mcp._serialize import dump, dump_list
from agenttier.warmpool import PoolConfig

# --- governance --------------------------------------------------------------


def governance_list_policies() -> dict[str, Any]:
    """Return the cluster default governance policy plus every namespace override."""
    client = get_client()
    return dump(client.governance.list())


def governance_get_policy(namespace: str) -> dict[str, Any]:
    """Return the raw (not-yet-resolved) governance policy stored for a namespace.

    :param namespace: The namespace whose policy to look up.
    """
    client = get_client()
    return dump(client.governance.get(namespace))


def governance_set_policy(
    namespace: Optional[str] = None,
    max_sandboxes_per_user: int = 0,
    max_sandboxes_total: int = 0,
    max_cpu: Optional[str] = None,
    max_memory: Optional[str] = None,
    max_storage: Optional[str] = None,
    max_timeout: Optional[str] = None,
    max_idle_timeout: Optional[str] = None,
    allowed_templates: Optional[Sequence[str]] = None,
    max_agent_sandboxes: int = 0,
) -> dict[str, Any]:
    """Create or replace a governance policy. Admin-only.

    Omit namespace to set the cluster-wide default; pass one to set a
    namespace-specific override. Zero/omitted fields mean "no limit".

    :param namespace: Namespace to set an override for. Omit for the cluster-wide default.
    :param max_sandboxes_per_user: Max sandboxes one user may own in scope. 0 = unlimited.
    :param max_sandboxes_total: Max sandboxes total in scope. 0 = unlimited.
    :param max_cpu: Max CPU limit a sandbox may request (e.g. "4").
    :param max_memory: Max memory limit a sandbox may request (e.g. "8Gi").
    :param max_storage: Max workspace storage size a sandbox may request (e.g. "50Gi").
    :param max_timeout: Max sandbox lifetime as a Go duration (e.g. "24h").
    :param max_idle_timeout: Max idle-timeout as a Go duration (e.g. "2h").
    :param allowed_templates: Restrict to only these template names. Empty = all allowed.
    :param max_agent_sandboxes: Max agent-mode sandboxes in scope. 0 = unlimited.
    """
    client = get_client()
    # Built via model_validate(dict) rather than Policy(field=...) kwargs:
    # no mypy pydantic plugin is configured for this project, so mypy can't
    # see that _Model's alias_generator + populate_by_name=True make
    # snake_case kwargs valid at runtime — model_validate sidesteps that
    # static-typing gap entirely (same pattern used to avoid it elsewhere).
    policy = Policy.model_validate(
        {
            "max_sandboxes_per_user": max_sandboxes_per_user,
            "max_sandboxes_total": max_sandboxes_total,
            "max_cpu": max_cpu,
            "max_memory": max_memory,
            "max_storage": max_storage,
            "max_timeout": max_timeout,
            "max_idle_timeout": max_idle_timeout,
            "allowed_templates": list(allowed_templates) if allowed_templates else [],
            "max_agent_sandboxes": max_agent_sandboxes,
        }
    )
    return dump(client.governance.set(policy, namespace=namespace))


def governance_delete_policy(namespace: str) -> dict[str, str]:
    """Delete a namespace-specific governance policy override. Admin-only.

    :param namespace: The namespace override to delete.
    """
    client = get_client()
    client.governance.delete(namespace)
    return {"namespace": namespace, "status": "deleted"}


def governance_effective_policy(namespace: Optional[str] = None) -> dict[str, Any]:
    """Return the fully-resolved governance policy that applies to a namespace.

    :param namespace: Namespace to resolve. Omit to use the Router's default namespace.
    """
    client = get_client()
    return dump(client.governance.effective(namespace=namespace))


# --- analytics / audit / admin ------------------------------------------------


def analytics_usage() -> dict[str, Any]:
    """Fleet-wide usage summary: status/template breakdowns, average startup time. Admin-only."""
    client = get_client()
    return dump(client.analytics.usage())


def analytics_costs() -> dict[str, Any]:
    """Fleet-wide cost estimate (not billing data — see field docs). Admin-only."""
    client = get_client()
    return dump(client.analytics.costs())


def audit_list_events() -> list[dict[str, Any]]:
    """Return the activity log (Kubernetes Events for Sandbox objects), most recent first. Admin-only."""
    client = get_client()
    return dump_list(client.audit.list_events())


def admin_list_sandboxes() -> list[dict[str, Any]]:
    """Fleet-wide sandbox list, unfiltered by ownership. Admin-only."""
    client = get_client()
    return dump_list(client.admin.sandboxes())


def admin_sharing_overview() -> dict[str, Any]:
    """Fleet-wide sharing overview. Admin-only."""
    client = get_client()
    return dict(client.admin.sharing())


# --- user preferences / API keys ---------------------------------------------


def user_get_preferences() -> dict[str, Any]:
    """Return the caller's saved preferences ({} if none saved yet)."""
    client = get_client()
    return dict(client.user.preferences_get())


def user_set_preferences(preferences: dict[str, Any]) -> dict[str, Any]:
    """Replace the caller's preferences wholesale.

    :param preferences: The full preferences object to store (replaces any existing value).
    """
    client = get_client()
    return dict(client.user.preferences_set(preferences))


def apikey_list() -> list[dict[str, Any]]:
    """Return metadata for every API key owned by the caller (never the plaintext key)."""
    client = get_client()
    return dump_list(client.api_keys.list())


def apikey_create(
    name: Optional[str] = None,
    expires_in: Optional[str] = None,
    sandbox_id: Optional[str] = None,
    action_groups: Optional[Sequence[str]] = None,
) -> dict[str, Any]:
    """Mint a new API key. The plaintext key is returned exactly once — store it immediately.

    Pass sandbox_id (+ optional action_groups) to mint a sandbox-scoped key
    restricted to that one sandbox instead of a full-access user-level key.

    :param name: A human-readable label for the key.
    :param expires_in: Go duration string (e.g. "720h") after which the key expires. Omit for no expiry.
    :param sandbox_id: Bind the key to exactly one sandbox (sandbox-scoped key).
    :param action_groups: Action groups to grant a sandbox-scoped key (requires sandbox_id).
    """
    client = get_client()
    result = client.api_keys.create(
        name=name,
        expires_in=expires_in,
        sandbox_id=sandbox_id,
        action_groups=list(action_groups) if action_groups else None,
    )
    return dump(result)


def apikey_revoke(key_id: str) -> dict[str, str]:
    """Revoke (delete) an API key by its ID.

    :param key_id: The API key's id (from apikey_list/apikey_create).
    """
    client = get_client()
    client.api_keys.revoke(key_id)
    return {"key_id": key_id, "status": "revoked"}


# --- warm pool / cluster ------------------------------------------------------


def warmpool_status() -> dict[str, Any]:
    """Return the current warm-pool status across every configured template."""
    client = get_client()
    return dump(client.warmpool.status())


def warmpool_set_config(pools: Sequence[dict[str, Any]]) -> list[dict[str, Any]]:
    """Replace the warm-pool configuration. Admin-only.

    :param pools: List of ``{"template": <name>, "desired_count": <0-10>}`` entries.
    """
    client = get_client()
    configs = [PoolConfig.model_validate(p) for p in pools]
    return dump_list(client.warmpool.set_config(configs))


def cluster_status() -> dict[str, Any]:
    """Return the node + pod headcount glance for the cluster."""
    client = get_client()
    return dump(client.cluster.status())


def cluster_nodes() -> dict[str, Any]:
    """Return per-node capacity/usage detail. Admin-only."""
    client = get_client()
    return dump(client.cluster.nodes())


def cluster_get_headroom() -> dict[str, Any]:
    """Return the current spare-capacity headroom Deployment config."""
    client = get_client()
    return dump(client.cluster.headroom_get())


def cluster_set_headroom(
    replicas: int, cpu: Optional[str] = None, memory: Optional[str] = None
) -> dict[str, Any]:
    """Update the headroom Deployment's replica count and (optionally) per-replica cpu/memory. Admin-only.

    :param replicas: Desired replica count, 0-50.
    :param cpu: Per-replica CPU request/limit (e.g. "500m"). Omit to leave unchanged.
    :param memory: Per-replica memory request/limit (e.g. "1Gi"). Omit to leave unchanged.
    """
    client = get_client()
    return dump(client.cluster.headroom_set(replicas, cpu=cpu, memory=memory))


# --- bulk sandbox operations ---------------------------------------------------


def sandbox_create_bulk(items: Sequence[dict[str, Any]]) -> list[dict[str, Any]]:
    """Create multiple sandboxes in one call.

    A governance cap violation (e.g. exceeding maxSandboxesTotal) fails the
    entire batch — nothing is created. Otherwise each item is independent:
    a bad template on one item doesn't block the others.

    :param items: List of create specs, each like
        ``{"template": <name>, "name": <name>, "namespace": "default", "timeout": ..., "idle_timeout": ..., "storage_size": ...}``.
    """
    client = get_client()
    parsed = [BulkCreateItem.model_validate(item) for item in items]
    return dump_list(client.sandboxes.create_bulk(parsed))


def sandbox_bulk_action(ids: Sequence[str], action: str) -> list[dict[str, Any]]:
    """Apply "stop", "resume", or "delete" to multiple sandboxes in one call.

    Per-item RBAC/existence failures (unknown id, another user's sandbox)
    are reported per-item, not a whole-batch abort.

    :param ids: Sandbox ids to act on.
    :param action: One of "stop", "resume", "delete".
    """
    client = get_client()
    return dump_list(client.sandboxes.bulk_action(list(ids), action))
