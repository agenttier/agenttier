# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""MCP tools mirroring :mod:`agenttier.sharing` and :mod:`agenttier.backups`
(both accessed as ``Sandbox.sharing`` / ``Sandbox.backups``)."""

from __future__ import annotations

from typing import Any, Optional

from agenttier.mcp._client import get_client
from agenttier.mcp._serialize import dump, dump_list


def sharing_list(sandbox_id: str) -> dict[str, Any]:
    """Return a sandbox's current sharing configuration (users, groups, share links).

    :param sandbox_id: The sandbox's id.
    """
    client = get_client()
    return dump(client.get_sandbox(sandbox_id).sharing.list())


def sharing_grant(
    sandbox_id: str, identity: str, level: str = "viewer", kind: str = "user"
) -> dict[str, Any]:
    """Grant (or update) a user's or group's access level on a sandbox.

    :param sandbox_id: The sandbox's id.
    :param identity: Email (for kind="user") or group name (for kind="group") to grant access to.
    :param level: Access level: "viewer" or "collaborator".
    :param kind: Either "user" or "group".
    """
    client = get_client()
    return dump(client.get_sandbox(sandbox_id).sharing.grant(identity, level=level, kind=kind))


def sharing_revoke(sandbox_id: str, identity: str) -> dict[str, str]:
    """Remove a previously granted user's or group's access to a sandbox.

    :param sandbox_id: The sandbox's id.
    :param identity: The identity (email or group name) to revoke.
    """
    client = get_client()
    client.get_sandbox(sandbox_id).sharing.revoke(identity)
    return {"sandbox_id": sandbox_id, "identity": identity, "status": "revoked"}


def sharing_create_link(
    sandbox_id: str,
    level: str = "viewer",
    expires_in: Optional[str] = None,
    max_uses: int = 0,
) -> dict[str, Any]:
    """Mint an expiring share link for a sandbox. The raw token is returned exactly once.

    :param sandbox_id: The sandbox's id.
    :param level: Access level granted by the link: "viewer" or "collaborator".
    :param expires_in: Go duration string (e.g. "24h") after which the link expires. Omit for no expiry.
    :param max_uses: Maximum number of times the link may be used. 0 means unlimited.
    """
    client = get_client()
    result = client.get_sandbox(sandbox_id).sharing.create_link(
        level=level, expires_in=expires_in, max_uses=max_uses
    )
    return dump(result)


def backup_list(sandbox_id: str) -> list[dict[str, Any]]:
    """List backup and clone snapshots for a sandbox's workspace.

    :param sandbox_id: The sandbox's id.
    """
    client = get_client()
    return dump_list(client.get_sandbox(sandbox_id).backups.list())


def backup_create(sandbox_id: str, snapshot_class: Optional[str] = None) -> dict[str, Any]:
    """Trigger an on-demand backup snapshot of a sandbox's workspace, outside the scheduled interval.

    :param sandbox_id: The sandbox's id.
    :param snapshot_class: Override for the cluster's default VolumeSnapshotClass.
    """
    client = get_client()
    return dump(client.get_sandbox(sandbox_id).backups.create(snapshot_class=snapshot_class))


def backup_restore(
    sandbox_id: str, snapshot_name: str, name: Optional[str] = None
) -> dict[str, Any]:
    """Restore a backup by creating a new sandbox cloned from it.

    :param sandbox_id: The sandbox whose backup to restore from.
    :param snapshot_name: The backup snapshot's name (from backup_list).
    :param name: Name for the new restored sandbox. Auto-generated if omitted.
    """
    client = get_client()
    restored = client.get_sandbox(sandbox_id).backups.restore(snapshot_name, name=name)
    return {"sandbox_id": restored.id, "name": restored.name, "namespace": restored.namespace}


def backup_delete(sandbox_id: str, snapshot_name: str) -> dict[str, str]:
    """Prune a specific backup snapshot on demand.

    :param sandbox_id: The sandbox that owns the backup.
    :param snapshot_name: The backup snapshot's name to delete.
    """
    client = get_client()
    client.get_sandbox(sandbox_id).backups.delete(snapshot_name)
    return {"sandbox_id": sandbox_id, "snapshot_name": snapshot_name, "status": "deleted"}
