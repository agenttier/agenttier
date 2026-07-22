# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Sync + async wrappers around the activity/audit log REST surface.

Wraps ``GET /audit/events`` (admin-only on the Router). Use ``client.audit``
to obtain an instance; don't construct directly.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from agenttier._http import raise_for_status
from agenttier.models import AuditEvent

if TYPE_CHECKING:  # pragma: no cover
    import httpx

    from agenttier.async_client import AsyncAgentTierClient
    from agenttier.client import AgentTierClient


class AuditAPI:
    """Sync wrapper for ``/audit/events``. Admin-only on the Router."""

    def __init__(self, client: "AgentTierClient") -> None:
        self._http: "httpx.Client" = client._http

    def list_events(self) -> list[AuditEvent]:
        """Return the activity log (Kubernetes Events for Sandbox objects), most recent first."""
        resp = self._http.get("/audit/events")
        raise_for_status(resp)
        return [AuditEvent.model_validate(e) for e in (resp.json().get("events") or [])]


class AsyncAuditAPI:
    """Async mirror of :class:`AuditAPI`. See its docstring for the full contract."""

    def __init__(self, client: "AsyncAgentTierClient") -> None:
        self._http: "httpx.AsyncClient" = client._http

    async def list_events(self) -> list[AuditEvent]:
        resp = await self._http.get("/audit/events")
        raise_for_status(resp)
        return [AuditEvent.model_validate(e) for e in (resp.json().get("events") or [])]
