# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Sync + async wrappers around the sandbox backup/restore REST surface.

Wraps ``GET/POST /sandboxes/{id}/backups``,
``POST /sandboxes/{id}/backups/{snapshotName}/restore``, and
``DELETE /sandboxes/{id}/backups/{snapshotName}`` — the Router-side surface
over the existing scheduled-VolumeSnapshot backup mechanism
(``pkg/controller/backup``). Lives in its own module so
:class:`agenttier.sandbox.Sandbox` / :class:`agenttier.async_sandbox.AsyncSandbox`
don't grow a backup-specific import surface — same pattern as
``FilesAPI``/``SharingAPI``.
"""

from __future__ import annotations

from datetime import datetime
from typing import TYPE_CHECKING, Optional

from pydantic import Field

from agenttier._http import raise_for_status
from agenttier.models import _Model

if TYPE_CHECKING:  # pragma: no cover
    import httpx

    from agenttier.async_sandbox import AsyncSandbox
    from agenttier.sandbox import Sandbox


class BackupInfo(_Model):
    """One VolumeSnapshot-backed backup, as returned by list/create.

    ``kind`` distinguishes an on-demand/scheduled backup
    (``scheduled-backup``) from a clone snapshot taken as a side effect of
    :meth:`agenttier.sandbox.Sandbox.clone`.
    """

    name: str
    created_at: Optional[datetime] = Field(default=None, alias="createdAt")
    kind: str = ""
    ready_to_use: bool = Field(default=False, alias="readyToUse")
    restore_size: Optional[str] = Field(default=None, alias="restoreSize")


def _validate_restore_args(snapshot_name: str) -> None:
    if not snapshot_name:
        raise ValueError("snapshot_name must be a non-empty string")


class BackupsAPI:
    """Sync wrapper for ``/sandboxes/{id}/backups*``.

    Use ``sandbox.backups`` to obtain an instance; don't construct directly.
    """

    def __init__(self, sandbox: "Sandbox") -> None:
        self._sandbox = sandbox
        self._http: "httpx.Client" = sandbox._http

    def list(self) -> list[BackupInfo]:
        """List backup + clone snapshots for this sandbox's PVC."""
        resp = self._http.get(f"/sandboxes/{self._sandbox.id}/backups")
        raise_for_status(resp)
        body = resp.json()
        items = body.get("backups", body) if isinstance(body, dict) else body
        return [BackupInfo.model_validate(item) for item in items or []]

    def create(self, snapshot_class: Optional[str] = None) -> BackupInfo:
        """Trigger an on-demand backup snapshot outside the scheduled interval.

        The snapshot is labeled the same as a scheduled backup, so retention
        pruning still applies (it doesn't bypass the configured retention
        policy). ``snapshot_class`` overrides the cluster's default
        ``VolumeSnapshotClass``; most callers leave this unset.
        """
        body: dict[str, str] = {}
        if snapshot_class is not None:
            body["snapshotClass"] = snapshot_class
        resp = self._http.post(f"/sandboxes/{self._sandbox.id}/backups", json=body)
        raise_for_status(resp)
        return BackupInfo.model_validate(resp.json())

    def restore(self, snapshot_name: str, *, name: Optional[str] = None) -> "Sandbox":
        """Restore a backup by creating a new Sandbox cloned from it.

        Returns a :class:`agenttier.sandbox.Sandbox` proxy for the new
        sandbox in ``Pending`` state — poll :meth:`Sandbox.status` or use
        :meth:`Sandbox.wait_until_running` until it reaches ``Running``.

        Raises :class:`agenttier.exceptions.NotFoundError` or
        :class:`agenttier.exceptions.ConflictError` if the snapshot has been
        pruned since it was listed (a race between list and restore).
        """
        _validate_restore_args(snapshot_name)
        body: dict[str, str] = {}
        if name is not None:
            body["name"] = name
        resp = self._http.post(
            f"/sandboxes/{self._sandbox.id}/backups/{snapshot_name}/restore",
            json=body,
        )
        raise_for_status(resp)
        payload = resp.json()
        from agenttier.sandbox import Sandbox

        return Sandbox(
            http=self._http,
            sandbox_id=payload["name"],
            name=payload["name"],
            namespace=payload.get("namespace", self._sandbox.namespace),
        )

    def delete(self, snapshot_name: str) -> None:
        """Prune a specific backup snapshot on demand."""
        if not snapshot_name:
            raise ValueError("snapshot_name must be a non-empty string")
        resp = self._http.delete(f"/sandboxes/{self._sandbox.id}/backups/{snapshot_name}")
        raise_for_status(resp)


class AsyncBackupsAPI:
    """Async mirror of :class:`BackupsAPI`. See its docstrings for the full contract."""

    def __init__(self, sandbox: "AsyncSandbox") -> None:
        self._sandbox = sandbox
        self._http: "httpx.AsyncClient" = sandbox._http

    async def list(self) -> list[BackupInfo]:
        resp = await self._http.get(f"/sandboxes/{self._sandbox.id}/backups")
        raise_for_status(resp)
        body = resp.json()
        items = body.get("backups", body) if isinstance(body, dict) else body
        return [BackupInfo.model_validate(item) for item in items or []]

    async def create(self, snapshot_class: Optional[str] = None) -> BackupInfo:
        body: dict[str, str] = {}
        if snapshot_class is not None:
            body["snapshotClass"] = snapshot_class
        resp = await self._http.post(f"/sandboxes/{self._sandbox.id}/backups", json=body)
        raise_for_status(resp)
        return BackupInfo.model_validate(resp.json())

    async def restore(self, snapshot_name: str, *, name: Optional[str] = None) -> "AsyncSandbox":
        _validate_restore_args(snapshot_name)
        body: dict[str, str] = {}
        if name is not None:
            body["name"] = name
        resp = await self._http.post(
            f"/sandboxes/{self._sandbox.id}/backups/{snapshot_name}/restore",
            json=body,
        )
        raise_for_status(resp)
        payload = resp.json()
        from agenttier.async_sandbox import AsyncSandbox

        return AsyncSandbox(
            http=self._http,
            sandbox_id=payload["name"],
            name=payload["name"],
            namespace=payload.get("namespace", self._sandbox.namespace),
        )

    async def delete(self, snapshot_name: str) -> None:
        if not snapshot_name:
            raise ValueError("snapshot_name must be a non-empty string")
        resp = await self._http.delete(f"/sandboxes/{self._sandbox.id}/backups/{snapshot_name}")
        raise_for_status(resp)
