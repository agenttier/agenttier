# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Sync + async wrappers around the warm-pool REST surface.

Wraps ``GET /warmpool/status`` and ``PUT /warmpool/config``. Lives in its
own module, attached as ``client.warmpool``, so
:class:`agenttier.client.AgentTierClient` /
:class:`agenttier.async_client.AsyncAgentTierClient` don't grow a
warm-pool-specific import surface — same pattern as ``governance.py``.

``PUT /warmpool/config`` is admin-only on the Router side; a non-admin
caller gets :class:`agenttier.exceptions.AuthorizationError`.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Sequence

from pydantic import Field

from agenttier._http import raise_for_status
from agenttier.models import _Model

if TYPE_CHECKING:  # pragma: no cover
    import httpx

    from agenttier.async_client import AsyncAgentTierClient
    from agenttier.client import AgentTierClient

# Mirrors the Router's per-pool bound (pkg/router/handlers.go
# handleSetWarmPoolConfig: "pools[%d].desiredCount must be 0-10"). Enforcing
# it here too means a bad value is rejected before the network round trip
# (FR1.10) instead of surfacing as a 400 from the server.
_MAX_POOL_DESIRED_COUNT = 10


class PoolConfig(_Model):
    """One per-template warm-pool entry, used as input to :meth:`set_config`."""

    template: str
    desired_count: int = Field(alias="desiredCount")


class PoolStatus(_Model):
    """Live state of one per-template pool, as returned by ``status()``."""

    template: str
    desired_count: int = Field(alias="desiredCount")
    ready_count: int = Field(alias="readyCount")
    pending_count: int = Field(alias="pendingCount")


class WarmPoolStatus(_Model):
    """Full warm-pool status returned by ``GET /warmpool/status``.

    ``pools`` is always populated (one entry per configured template);
    the top-level ``desired_count``/``ready_count``/``pending_count``/
    ``template`` fields are a deprecated legacy convenience mirrored from
    ``pools[0]`` when there's exactly one entry — prefer ``pools``.
    """

    pools: list[PoolStatus] = Field(default_factory=list)
    desired_count: int = Field(default=0, alias="desiredCount")
    ready_count: int = Field(default=0, alias="readyCount")
    pending_count: int = Field(default=0, alias="pendingCount")
    template: str = ""


def _validate_pools(pools: Sequence[PoolConfig]) -> None:
    if not pools:
        raise ValueError("pools must be a non-empty sequence of PoolConfig")
    for i, p in enumerate(pools):
        if not p.template:
            raise ValueError(f"pools[{i}].template must be a non-empty string")
        if not 0 <= p.desired_count <= _MAX_POOL_DESIRED_COUNT:
            raise ValueError(f"pools[{i}].desired_count must be 0-{_MAX_POOL_DESIRED_COUNT}")


class WarmPoolAPI:
    """Sync wrapper for ``/warmpool/*``.

    Use ``client.warmpool`` to obtain an instance; don't construct directly.
    """

    def __init__(self, client: "AgentTierClient") -> None:
        self._http: "httpx.Client" = client._http

    def status(self) -> WarmPoolStatus:
        """Return the current warm-pool status across every configured template."""
        resp = self._http.get("/warmpool/status")
        raise_for_status(resp)
        return WarmPoolStatus.model_validate(resp.json())

    def set_config(self, pools: Sequence[PoolConfig]) -> list[PoolConfig]:
        """Replace the warm-pool configuration with ``pools``.

        Each entry's ``desired_count`` must be in ``[0, 10]``, matching the
        Router's own bound. Admin-only on the Router side.
        """
        _validate_pools(pools)
        body = {"pools": [p.model_dump(by_alias=True) for p in pools]}
        resp = self._http.put("/warmpool/config", json=body)
        raise_for_status(resp)
        data = resp.json()
        return [PoolConfig.model_validate(p) for p in data.get("pools", [])]


class AsyncWarmPoolAPI:
    """Async mirror of :class:`WarmPoolAPI`. See its docstrings for the full contract."""

    def __init__(self, client: "AsyncAgentTierClient") -> None:
        self._http: "httpx.AsyncClient" = client._http

    async def status(self) -> WarmPoolStatus:
        resp = await self._http.get("/warmpool/status")
        raise_for_status(resp)
        return WarmPoolStatus.model_validate(resp.json())

    async def set_config(self, pools: Sequence[PoolConfig]) -> list[PoolConfig]:
        _validate_pools(pools)
        body = {"pools": [p.model_dump(by_alias=True) for p in pools]}
        resp = await self._http.put("/warmpool/config", json=body)
        raise_for_status(resp)
        data = resp.json()
        return [PoolConfig.model_validate(p) for p in data.get("pools", [])]
