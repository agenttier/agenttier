# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Transport-agnostic MCP tool functions, one per public SDK method (FR7.1).

Each function in :data:`ALL_TOOLS` is a plain, synchronous Python callable —
no dependency on the ``mcp`` package or on stdio/HTTP/SSE at all. That keeps
this layer usable by any binding (see :mod:`agenttier.mcp.server`) and
trivially unit-testable without spinning up a transport.

Grouped by the SDK module they mirror; import order here is registration
order (cosmetic — tool names are what an MCP client actually keys off).
"""

from __future__ import annotations

from typing import Callable

from agenttier.mcp.tools import files, fleet, sandboxes, sharing_backups, webhooks

# --- flat registry, one entry per tool ---------------------------------------

ALL_TOOLS: dict[str, Callable[..., object]] = {
    # sandboxes.py
    "sandbox_create": sandboxes.sandbox_create,
    "sandbox_list": sandboxes.sandbox_list,
    "sandbox_status": sandboxes.sandbox_status,
    "sandbox_stop": sandboxes.sandbox_stop,
    "sandbox_resume": sandboxes.sandbox_resume,
    "sandbox_terminate": sandboxes.sandbox_terminate,
    "sandbox_clone": sandboxes.sandbox_clone,
    "sandbox_run_command": sandboxes.sandbox_run_command,
    "sandbox_list_ports": sandboxes.sandbox_list_ports,
    "sandbox_forward_port": sandboxes.sandbox_forward_port,
    "sandbox_remove_port": sandboxes.sandbox_remove_port,
    "sandbox_update": sandboxes.sandbox_update,
    "template_list": sandboxes.template_list,
    "template_get": sandboxes.template_get,
    "current_user": sandboxes.current_user,
    # files.py
    "file_list": files.file_list,
    "file_read": files.file_read,
    "file_write": files.file_write,
    # sharing_backups.py
    "sharing_list": sharing_backups.sharing_list,
    "sharing_grant": sharing_backups.sharing_grant,
    "sharing_revoke": sharing_backups.sharing_revoke,
    "sharing_create_link": sharing_backups.sharing_create_link,
    "backup_list": sharing_backups.backup_list,
    "backup_create": sharing_backups.backup_create,
    "backup_restore": sharing_backups.backup_restore,
    "backup_delete": sharing_backups.backup_delete,
    # fleet.py
    "governance_list_policies": fleet.governance_list_policies,
    "governance_get_policy": fleet.governance_get_policy,
    "governance_set_policy": fleet.governance_set_policy,
    "governance_delete_policy": fleet.governance_delete_policy,
    "governance_effective_policy": fleet.governance_effective_policy,
    "analytics_usage": fleet.analytics_usage,
    "analytics_costs": fleet.analytics_costs,
    "audit_list_events": fleet.audit_list_events,
    "admin_list_sandboxes": fleet.admin_list_sandboxes,
    "admin_sharing_overview": fleet.admin_sharing_overview,
    "user_get_preferences": fleet.user_get_preferences,
    "user_set_preferences": fleet.user_set_preferences,
    "apikey_list": fleet.apikey_list,
    "apikey_create": fleet.apikey_create,
    "apikey_revoke": fleet.apikey_revoke,
    "warmpool_status": fleet.warmpool_status,
    "warmpool_set_config": fleet.warmpool_set_config,
    "cluster_status": fleet.cluster_status,
    "cluster_nodes": fleet.cluster_nodes,
    "cluster_get_headroom": fleet.cluster_get_headroom,
    "cluster_set_headroom": fleet.cluster_set_headroom,
    "sandbox_create_bulk": fleet.sandbox_create_bulk,
    "sandbox_bulk_action": fleet.sandbox_bulk_action,
    # webhooks.py
    "webhook_create": webhooks.webhook_create,
    "webhook_delete": webhooks.webhook_delete,
    "webhook_deliveries": webhooks.webhook_deliveries,
    "webhook_list": webhooks.webhook_list,
}

__all__ = ["ALL_TOOLS"]
