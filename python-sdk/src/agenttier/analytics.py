# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Sync + async wrappers around the fleet-wide analytics REST surface.

Wraps ``GET /analytics/usage`` and ``GET /analytics/costs`` (both admin-only
on the Router). Lives in its own module so :class:`agenttier.client.AgentTierClient`
doesn't grow an analytics-specific import surface — same pattern as
``FilesAPI``/``AgentAPI``. Use ``client.analytics`` to obtain an instance;
don't construct directly.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from pydantic import Field

from agenttier._http import raise_for_status
from agenttier.models import UsageAnalytics, _Model

if TYPE_CHECKING:  # pragma: no cover
    import httpx

    from agenttier.async_client import AsyncAgentTierClient
    from agenttier.client import AgentTierClient


class TemplateCost(_Model):
    """Per-template cost breakdown, one entry of ``CostEstimate.per_template``."""

    template: str
    hourly_cost: float = Field(alias="hourly_cost")
    count: int


class CostEstimate(_Model):
    """Fleet-wide cost estimate returned by ``/analytics/costs``.

    Estimates are computed server-side from a fixed per-resource rate table
    (see ``pkg/router/handlers.go:handleGetCostEstimates``); they are not
    billing data. ``total_estimated_monthly`` extrapolates the current hourly
    rate assuming 24/7 usage — actual bills will differ for sandboxes that
    are stopped part of the time.
    """

    running_sandboxes: int = Field(alias="running_sandboxes")
    stopped_sandboxes: int = Field(alias="stopped_sandboxes")
    total_hourly_compute: float = Field(alias="total_hourly_compute")
    total_hourly_storage: float = Field(alias="total_hourly_storage")
    total_estimated_monthly: float = Field(alias="total_estimated_monthly")
    per_template: list[TemplateCost] = Field(default_factory=list, alias="per_template")


class AnalyticsAPI:
    """Sync wrapper for ``/analytics/*``. Admin-only on the Router."""

    def __init__(self, client: "AgentTierClient") -> None:
        self._http: "httpx.Client" = client._http

    def usage(self) -> UsageAnalytics:
        """Fleet-wide usage summary: status/template breakdowns, avg startup time."""
        resp = self._http.get("/analytics/usage")
        raise_for_status(resp)
        return UsageAnalytics.model_validate(resp.json())

    def costs(self) -> CostEstimate:
        """Fleet-wide cost estimate. See :class:`CostEstimate` for caveats."""
        resp = self._http.get("/analytics/costs")
        raise_for_status(resp)
        return CostEstimate.model_validate(resp.json())


class AsyncAnalyticsAPI:
    """Async mirror of :class:`AnalyticsAPI`. See its docstrings for the full contract."""

    def __init__(self, client: "AsyncAgentTierClient") -> None:
        self._http: "httpx.AsyncClient" = client._http

    async def usage(self) -> UsageAnalytics:
        resp = await self._http.get("/analytics/usage")
        raise_for_status(resp)
        return UsageAnalytics.model_validate(resp.json())

    async def costs(self) -> CostEstimate:
        resp = await self._http.get("/analytics/costs")
        raise_for_status(resp)
        return CostEstimate.model_validate(resp.json())
