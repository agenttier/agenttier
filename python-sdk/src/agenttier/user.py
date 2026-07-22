# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Sync + async wrappers around the user-preferences REST surface.

Wraps ``GET/PUT /user/preferences``. Lives in its own module, attached as
``client.user``, so :class:`agenttier.client.AgentTierClient` /
:class:`agenttier.async_client.AsyncAgentTierClient` don't grow a
preferences-specific import surface — same pattern as ``governance.py``.

Preferences are a free-form JSON object on the Router side (a per-user
ConfigMap), so this module deliberately does not impose a schema — callers
pass/receive a plain ``dict``.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from agenttier._http import raise_for_status

if TYPE_CHECKING:  # pragma: no cover
    import httpx

    from agenttier.async_client import AsyncAgentTierClient
    from agenttier.client import AgentTierClient


class UserAPI:
    """Sync wrapper for ``/user/preferences``.

    Use ``client.user`` to obtain an instance; don't construct directly.
    """

    def __init__(self, client: "AgentTierClient") -> None:
        self._http: "httpx.Client" = client._http

    def preferences_get(self) -> dict[str, Any]:
        """Return the caller's saved preferences (``{}`` if none saved yet)."""
        resp = self._http.get("/user/preferences")
        raise_for_status(resp)
        return dict(resp.json() or {})

    def preferences_set(self, preferences: dict[str, Any]) -> dict[str, Any]:
        """Replace the caller's preferences wholesale and return the stored value."""
        if not isinstance(preferences, dict):
            raise ValueError("preferences must be a dict")
        resp = self._http.put("/user/preferences", json=preferences)
        raise_for_status(resp)
        return dict(resp.json() or {})


class AsyncUserAPI:
    """Async mirror of :class:`UserAPI`. See its docstrings for the full contract."""

    def __init__(self, client: "AsyncAgentTierClient") -> None:
        self._http: "httpx.AsyncClient" = client._http

    async def preferences_get(self) -> dict[str, Any]:
        resp = await self._http.get("/user/preferences")
        raise_for_status(resp)
        return dict(resp.json() or {})

    async def preferences_set(self, preferences: dict[str, Any]) -> dict[str, Any]:
        if not isinstance(preferences, dict):
            raise ValueError("preferences must be a dict")
        resp = await self._http.put("/user/preferences", json=preferences)
        raise_for_status(resp)
        return dict(resp.json() or {})
