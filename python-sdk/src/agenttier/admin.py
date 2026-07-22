# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Sync + async wrappers around the admin fleet-view REST surface.

Wraps ``GET /admin/sandboxes`` and ``GET /admin/sharing`` (both admin-only on
the Router). Use ``client.admin`` to obtain an instance; don't construct
directly.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any

from agenttier._http import raise_for_status
from agenttier.models import SandboxSummary

if TYPE_CHECKING:  # pragma: no cover
    import httpx

    from agenttier.async_client import AsyncAgentTierClient
    from agenttier.client import AgentTierClient


class AdminAPI:
    """Sync wrapper for ``/admin/*``. Admin-only on the Router."""

    def __init__(self, client: "AgentTierClient") -> None:
        self._http: "httpx.Client" = client._http

    def sandboxes(self) -> list[SandboxSummary]:
        """Fleet-wide sandbox list, unfiltered by ownership (admin view)."""
        resp = self._http.get("/admin/sandboxes")
        raise_for_status(resp)
        return [SandboxSummary.model_validate(s) for s in (resp.json().get("sandboxes") or [])]

    def sharing(self) -> dict[str, Any]:
        """Fleet-wide sharing overview (admin view).

        Returned as a free-form mapping — the Router's response shape for
        this endpoint is still evolving; see FR1.4 in ``requirements.md``.
        """
        resp = self._http.get("/admin/sharing")
        raise_for_status(resp)
        return dict(resp.json() or {})


class AsyncAdminAPI:
    """Async mirror of :class:`AdminAPI`. See its docstrings for the full contract."""

    def __init__(self, client: "AsyncAgentTierClient") -> None:
        self._http: "httpx.AsyncClient" = client._http

    async def sandboxes(self) -> list[SandboxSummary]:
        resp = await self._http.get("/admin/sandboxes")
        raise_for_status(resp)
        return [SandboxSummary.model_validate(s) for s in (resp.json().get("sandboxes") or [])]

    async def sharing(self) -> dict[str, Any]:
        resp = await self._http.get("/admin/sharing")
        raise_for_status(resp)
        return dict(resp.json() or {})
