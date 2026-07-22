# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Sync + async wrappers around the user API-key REST surface.

Wraps ``GET/POST /user/api-keys`` and ``DELETE /user/api-keys/{keyId}``.
Lives in its own module, attached as ``client.api_keys``, so
:class:`agenttier.client.AgentTierClient` /
:class:`agenttier.async_client.AsyncAgentTierClient` don't grow an
api-key-specific import surface — same pattern as ``governance.py``.

``create()`` accepts optional ``sandbox_id`` + ``action_groups`` so callers
can mint a sandbox-scoped key (FR6) through the same endpoint used for
ordinary user-level keys — omitting both mints a full-access user-level key,
unchanged from today's behavior.
"""

from __future__ import annotations

from datetime import datetime
from typing import TYPE_CHECKING, List, Optional

from pydantic import Field

from agenttier._http import raise_for_status
from agenttier.models import _Model

if TYPE_CHECKING:  # pragma: no cover
    import httpx

    from agenttier.async_client import AsyncAgentTierClient
    from agenttier.client import AgentTierClient


class APIKeyMetadata(_Model):
    """Non-secret metadata for a stored API key, as returned by ``list()``.

    Never includes the plaintext key or its hash — those exist only in the
    one-time :class:`APIKeyCreated` response.
    """

    id: str
    user_id: Optional[str] = Field(default=None, alias="userId")
    name: Optional[str] = None
    created_at: Optional[datetime] = Field(default=None, alias="createdAt")
    expires_at: Optional[datetime] = Field(default=None, alias="expiresAt")
    last_used_at: Optional[datetime] = Field(default=None, alias="lastUsedAt")
    sandbox_id: Optional[str] = Field(default=None, alias="sandboxId")
    action_groups: List[str] = Field(default_factory=list, alias="actionGroups")


class APIKeyCreated(_Model):
    """Response from minting a new API key — the plaintext is shown once."""

    id: str
    key: str
    name: Optional[str] = None
    created_at: Optional[datetime] = Field(default=None, alias="createdAt")
    expires_at: Optional[datetime] = Field(default=None, alias="expiresAt")
    warning: Optional[str] = None


def _validate_create_args(expires_in: Optional[str], sandbox_id: Optional[str], action_groups: Optional[list[str]]) -> None:
    if expires_in is not None and not expires_in:
        raise ValueError("expires_in must be a non-empty duration string when provided (e.g. \"720h\")")
    if action_groups is not None and not sandbox_id:
        raise ValueError("action_groups requires sandbox_id to be set")


class APIKeysAPI:
    """Sync wrapper for ``/user/api-keys*``.

    Use ``client.api_keys`` to obtain an instance; don't construct directly.
    """

    def __init__(self, client: "AgentTierClient") -> None:
        self._http: "httpx.Client" = client._http

    def list(self) -> list[APIKeyMetadata]:
        """Return metadata for every API key owned by the caller."""
        resp = self._http.get("/user/api-keys")
        raise_for_status(resp)
        keys = resp.json().get("keys") or []
        return [APIKeyMetadata.model_validate(k) for k in keys]

    def create(
        self,
        name: Optional[str] = None,
        expires_in: Optional[str] = None,
        *,
        sandbox_id: Optional[str] = None,
        action_groups: Optional[List[str]] = None,
    ) -> APIKeyCreated:
        """Mint a new API key. The plaintext key is returned exactly once.

        ``expires_in`` is a Go duration string (e.g. ``"720h"``); omit for a
        key that never expires. Pass ``sandbox_id`` (+ optional
        ``action_groups``) to mint a sandbox-scoped key restricted to that
        one sandbox instead of a full-access user-level key.
        """
        _validate_create_args(expires_in, sandbox_id, action_groups)
        body: dict[str, object] = {}
        if name is not None:
            body["name"] = name
        if expires_in is not None:
            body["expiresIn"] = expires_in
        if sandbox_id is not None:
            body["sandboxId"] = sandbox_id
        if action_groups is not None:
            body["actionGroups"] = action_groups
        resp = self._http.post("/user/api-keys", json=body)
        raise_for_status(resp)
        return APIKeyCreated.model_validate(resp.json())

    def revoke(self, key_id: str) -> None:
        """Revoke (delete) an API key by its ID."""
        if not key_id:
            raise ValueError("key_id must be a non-empty string")
        resp = self._http.delete(f"/user/api-keys/{key_id}")
        raise_for_status(resp)


class AsyncAPIKeysAPI:
    """Async mirror of :class:`APIKeysAPI`. See its docstrings for the full contract."""

    def __init__(self, client: "AsyncAgentTierClient") -> None:
        self._http: "httpx.AsyncClient" = client._http

    async def list(self) -> list[APIKeyMetadata]:
        resp = await self._http.get("/user/api-keys")
        raise_for_status(resp)
        keys = resp.json().get("keys") or []
        return [APIKeyMetadata.model_validate(k) for k in keys]

    async def create(
        self,
        name: Optional[str] = None,
        expires_in: Optional[str] = None,
        *,
        sandbox_id: Optional[str] = None,
        action_groups: Optional[List[str]] = None,
    ) -> APIKeyCreated:
        _validate_create_args(expires_in, sandbox_id, action_groups)
        body: dict[str, object] = {}
        if name is not None:
            body["name"] = name
        if expires_in is not None:
            body["expiresIn"] = expires_in
        if sandbox_id is not None:
            body["sandboxId"] = sandbox_id
        if action_groups is not None:
            body["actionGroups"] = action_groups
        resp = await self._http.post("/user/api-keys", json=body)
        raise_for_status(resp)
        return APIKeyCreated.model_validate(resp.json())

    async def revoke(self, key_id: str) -> None:
        if not key_id:
            raise ValueError("key_id must be a non-empty string")
        resp = await self._http.delete(f"/user/api-keys/{key_id}")
        raise_for_status(resp)
