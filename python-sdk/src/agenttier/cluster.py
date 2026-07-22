# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Sync + async wrappers around the cluster-status REST surface.

Wraps ``GET /cluster/status``, ``GET /cluster/nodes`` (admin), and
``GET/PUT /cluster/headroom`` (write admin-only). Lives in its own module,
attached as ``client.cluster``, so
:class:`agenttier.client.AgentTierClient` /
:class:`agenttier.async_client.AsyncAgentTierClient` don't grow a
cluster-specific import surface — same pattern as ``governance.py`` /
``warmpool.py``.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Optional

from pydantic import Field

from agenttier._http import raise_for_status
from agenttier.models import _Model

if TYPE_CHECKING:  # pragma: no cover
    import httpx

    from agenttier.async_client import AsyncAgentTierClient
    from agenttier.client import AgentTierClient

# Mirrors the Router's bound (pkg/router/headroom.go handleSetHeadroomConfig:
# "replicas must be in [0, 50]"). Enforced here too so a bad value is
# rejected before the network round trip (FR1.10).
_MAX_HEADROOM_REPLICAS = 50


class ClusterStatus(_Model):
    """Node + pod headcount, as returned by ``GET /cluster/status``."""

    nodes: int = 0
    nodes_ready: int = Field(default=0, alias="nodesReady")
    pods: int = 0
    sandbox_pods: int = Field(default=0, alias="sandboxPods")
    headroom_ready: int = Field(default=0, alias="headroomReady")
    autoscaler_enabled: bool = Field(default=False, alias="autoscalerEnabled")


class NodeResources(_Model):
    """A CPU (millicores) + memory (bytes) pair."""

    cpu_millis: int = Field(default=0, alias="cpuMillis")
    mem_bytes: int = Field(default=0, alias="memBytes")


class NodeCapacity(_Model):
    """Per-node capacity/usage entry, part of ``GET /cluster/nodes`` (admin)."""

    name: str
    ready: bool
    instance_type: Optional[str] = Field(default=None, alias="instanceType")
    node_group: Optional[str] = Field(default=None, alias="nodeGroup")
    allocatable: NodeResources = Field(default_factory=NodeResources)
    requests: NodeResources = Field(default_factory=NodeResources)


class NodeCapacitySummary(_Model):
    """Fleet-wide aggregate accompanying the per-node list."""

    ready: int = 0
    total: int = 0
    cpu_saturation_pct: float = Field(default=0.0, alias="cpuSaturationPct")
    mem_saturation_pct: float = Field(default=0.0, alias="memSaturationPct")
    allocatable: NodeResources = Field(default_factory=NodeResources)
    requests: NodeResources = Field(default_factory=NodeResources)


class NodeCapacityResponse(_Model):
    """Full response from ``GET /cluster/nodes`` (admin-only)."""

    nodes: list[NodeCapacity] = Field(default_factory=list)
    summary: NodeCapacitySummary = Field(default_factory=NodeCapacitySummary)


class HeadroomConfig(_Model):
    """Spare-capacity Deployment config, as used by ``GET/PUT /cluster/headroom``."""

    replicas: int = 0
    cpu: Optional[str] = None
    memory: Optional[str] = None
    ready_replicas: int = Field(default=0, alias="readyReplicas")
    enabled: bool = False


class ClusterAPI:
    """Sync wrapper for ``/cluster/*``.

    Use ``client.cluster`` to obtain an instance; don't construct directly.
    """

    def __init__(self, client: "AgentTierClient") -> None:
        self._http: "httpx.Client" = client._http

    def status(self) -> ClusterStatus:
        """Return the node + pod headcount glance."""
        resp = self._http.get("/cluster/status")
        raise_for_status(resp)
        return ClusterStatus.model_validate(resp.json())

    def nodes(self) -> NodeCapacityResponse:
        """Return per-node capacity/usage detail. Admin-only on the Router side."""
        resp = self._http.get("/cluster/nodes")
        raise_for_status(resp)
        return NodeCapacityResponse.model_validate(resp.json())

    def headroom_get(self) -> HeadroomConfig:
        """Return the current spare-capacity headroom Deployment config."""
        resp = self._http.get("/cluster/headroom")
        raise_for_status(resp)
        return HeadroomConfig.model_validate(resp.json())

    def headroom_set(
        self,
        replicas: int,
        cpu: Optional[str] = None,
        memory: Optional[str] = None,
    ) -> HeadroomConfig:
        """Update the headroom Deployment's replica count and (optionally)
        its per-replica cpu/memory. Admin-only on the Router side.

        ``replicas`` must be in ``[0, 50]``, matching the Router's own bound.
        Omitting ``cpu``/``memory`` leaves the existing values unchanged.
        """
        if not 0 <= replicas <= _MAX_HEADROOM_REPLICAS:
            raise ValueError(f"replicas must be in [0, {_MAX_HEADROOM_REPLICAS}]")
        body: dict[str, object] = {"replicas": replicas}
        if cpu is not None:
            body["cpu"] = cpu
        if memory is not None:
            body["memory"] = memory
        resp = self._http.put("/cluster/headroom", json=body)
        raise_for_status(resp)
        return HeadroomConfig.model_validate(resp.json())


class AsyncClusterAPI:
    """Async mirror of :class:`ClusterAPI`. See its docstrings for the full contract."""

    def __init__(self, client: "AsyncAgentTierClient") -> None:
        self._http: "httpx.AsyncClient" = client._http

    async def status(self) -> ClusterStatus:
        resp = await self._http.get("/cluster/status")
        raise_for_status(resp)
        return ClusterStatus.model_validate(resp.json())

    async def nodes(self) -> NodeCapacityResponse:
        resp = await self._http.get("/cluster/nodes")
        raise_for_status(resp)
        return NodeCapacityResponse.model_validate(resp.json())

    async def headroom_get(self) -> HeadroomConfig:
        resp = await self._http.get("/cluster/headroom")
        raise_for_status(resp)
        return HeadroomConfig.model_validate(resp.json())

    async def headroom_set(
        self,
        replicas: int,
        cpu: Optional[str] = None,
        memory: Optional[str] = None,
    ) -> HeadroomConfig:
        if not 0 <= replicas <= _MAX_HEADROOM_REPLICAS:
            raise ValueError(f"replicas must be in [0, {_MAX_HEADROOM_REPLICAS}]")
        body: dict[str, object] = {"replicas": replicas}
        if cpu is not None:
            body["cpu"] = cpu
        if memory is not None:
            body["memory"] = memory
        resp = await self._http.put("/cluster/headroom", json=body)
        raise_for_status(resp)
        return HeadroomConfig.model_validate(resp.json())
