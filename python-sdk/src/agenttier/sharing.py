# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Sync + async wrappers around the sandbox sharing REST surface.

Wraps ``GET/POST /sandboxes/{id}/share``, ``DELETE /sandboxes/{id}/share/{userId}``,
and ``POST /sandboxes/{id}/share-links``. Lives in its own module so the
:class:`agenttier.sandbox.Sandbox` / :class:`agenttier.async_sandbox.AsyncSandbox`
classes don't grow a sharing-specific import surface — same pattern as
``FilesAPI``/``AgentAPI``.
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

_VALID_LEVELS = ("viewer", "collaborator")
_VALID_KINDS = ("user", "group")


class SharePermission(_Model):
    """One identity's access level on a sandbox."""

    identity: str
    level: str


class ShareLinkInfo(_Model):
    """Non-secret metadata for an existing share link.

    The raw token is never included here — it's only returned once, at
    creation time, by :class:`ShareLinkCreated`.
    """

    id: str
    level: str
    expires_at: Optional[datetime] = Field(default=None, alias="expiresAt")
    max_uses: int = Field(default=0, alias="maxUses")
    used_count: int = Field(default=0, alias="usedCount")


class SharingInfo(_Model):
    """Full sharing configuration for a sandbox, as returned by GET/POST /share."""

    users: list[SharePermission] = Field(default_factory=list)
    groups: list[SharePermission] = Field(default_factory=list)
    share_links: list[ShareLinkInfo] = Field(default_factory=list, alias="shareLinks")


class ShareLinkCreated(_Model):
    """Response from creating a share link — the raw token is shown once."""

    id: str
    token: str
    level: str
    expires_at: Optional[datetime] = Field(default=None, alias="expiresAt")
    max_uses: int = Field(default=0, alias="maxUses")
    warning: Optional[str] = None


def _validate_grant_args(identity: str, level: str, kind: str) -> None:
    if not identity:
        raise ValueError("identity must be a non-empty string")
    if level not in _VALID_LEVELS:
        raise ValueError(f"level must be one of {_VALID_LEVELS}, got {level!r}")
    if kind not in _VALID_KINDS:
        raise ValueError(f"kind must be one of {_VALID_KINDS}, got {kind!r}")


def _validate_create_link_args(level: str, max_uses: int) -> None:
    if level not in _VALID_LEVELS:
        raise ValueError(f"level must be one of {_VALID_LEVELS}, got {level!r}")
    if max_uses < 0:
        raise ValueError("max_uses must be >= 0 (0 = unlimited)")


class SharingAPI:
    """Sync wrapper for ``/sandboxes/{id}/share*``.

    Use ``sandbox.sharing`` to obtain an instance; don't construct directly.
    """

    def __init__(self, sandbox: "Sandbox") -> None:
        self._sandbox = sandbox
        self._http: "httpx.Client" = sandbox._http

    def list(self) -> SharingInfo:
        """Return the sandbox's current sharing configuration."""
        resp = self._http.get(f"/sandboxes/{self._sandbox.id}/share")
        raise_for_status(resp)
        return SharingInfo.model_validate(resp.json())

    def grant(self, identity: str, level: str = "viewer", kind: str = "user") -> SharingInfo:
        """Grant (or update) a user's or group's access level.

        ``level`` must be ``"viewer"`` or ``"collaborator"``. Idempotent:
        re-granting an identity already present updates its level.
        """
        _validate_grant_args(identity, level, kind)
        resp = self._http.post(
            f"/sandboxes/{self._sandbox.id}/share",
            json={"identity": identity, "level": level, "kind": kind},
        )
        raise_for_status(resp)
        return SharingInfo.model_validate(resp.json())

    def revoke(self, identity: str) -> None:
        """Remove a previously granted user's or group's access."""
        if not identity:
            raise ValueError("identity must be a non-empty string")
        resp = self._http.delete(f"/sandboxes/{self._sandbox.id}/share/{identity}")
        raise_for_status(resp)

    def create_link(
        self,
        level: str = "viewer",
        expires_in: Optional[str] = None,
        max_uses: int = 0,
    ) -> ShareLinkCreated:
        """Mint an expiring share link. The raw token is returned exactly once.

        ``expires_in`` is a Go duration string (e.g. ``"24h"``); omit for a
        link that never expires. ``max_uses`` of ``0`` means unlimited uses.
        """
        _validate_create_link_args(level, max_uses)
        body: dict[str, object] = {"level": level, "maxUses": max_uses}
        if expires_in is not None:
            body["expiresIn"] = expires_in
        resp = self._http.post(f"/sandboxes/{self._sandbox.id}/share-links", json=body)
        raise_for_status(resp)
        return ShareLinkCreated.model_validate(resp.json())


class AsyncSharingAPI:
    """Async mirror of :class:`SharingAPI`. See its docstrings for the full contract."""

    def __init__(self, sandbox: "AsyncSandbox") -> None:
        self._sandbox = sandbox
        self._http: "httpx.AsyncClient" = sandbox._http

    async def list(self) -> SharingInfo:
        resp = await self._http.get(f"/sandboxes/{self._sandbox.id}/share")
        raise_for_status(resp)
        return SharingInfo.model_validate(resp.json())

    async def grant(self, identity: str, level: str = "viewer", kind: str = "user") -> SharingInfo:
        _validate_grant_args(identity, level, kind)
        resp = await self._http.post(
            f"/sandboxes/{self._sandbox.id}/share",
            json={"identity": identity, "level": level, "kind": kind},
        )
        raise_for_status(resp)
        return SharingInfo.model_validate(resp.json())

    async def revoke(self, identity: str) -> None:
        if not identity:
            raise ValueError("identity must be a non-empty string")
        resp = await self._http.delete(f"/sandboxes/{self._sandbox.id}/share/{identity}")
        raise_for_status(resp)

    async def create_link(
        self,
        level: str = "viewer",
        expires_in: Optional[str] = None,
        max_uses: int = 0,
    ) -> ShareLinkCreated:
        _validate_create_link_args(level, max_uses)
        body: dict[str, object] = {"level": level, "maxUses": max_uses}
        if expires_in is not None:
            body["expiresIn"] = expires_in
        resp = await self._http.post(f"/sandboxes/{self._sandbox.id}/share-links", json=body)
        raise_for_status(resp)
        return ShareLinkCreated.model_validate(resp.json())
