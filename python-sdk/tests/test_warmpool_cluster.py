# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Tests for the warm-pool and cluster wrappers on the client.

Both ``warmpool.py`` and ``cluster.py`` mirror ``governance.py``'s
constructor contract (``__init__(self, client)`` needing only
``client._http``) so they're usable standalone before the
``client.warmpool``/``client.cluster`` convenience properties are wired on
``AgentTierClient``/``AsyncAgentTierClient`` in the hub-wiring task. These
tests construct the API classes directly against a real client for that
reason.

Exercises the public surface only; the Router-side behavior is covered by
router integration tests. Uses pytest-httpx to stub HTTP responses.
"""

from __future__ import annotations

import pytest
from pytest_httpx import HTTPXMock

from agenttier import AgentTierClient, AsyncAgentTierClient, AuthorizationError
from agenttier.cluster import AsyncClusterAPI, ClusterAPI
from agenttier.warmpool import AsyncWarmPoolAPI, PoolConfig, WarmPoolAPI

API_URL = "http://router.test"
BASE = f"{API_URL}/api/v1"


# ------- warmpool ---------------------------------------------------------


def test_warmpool_status_parses_pools(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/warmpool/status",
        json={
            "pools": [
                {"template": "general-coding", "desiredCount": 3, "readyCount": 2, "pendingCount": 1},
            ],
            "desiredCount": 3,
            "readyCount": 2,
            "pendingCount": 1,
            "template": "general-coding",
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        status = WarmPoolAPI(client).status()
    assert status.pools[0].template == "general-coding"
    assert status.pools[0].ready_count == 2
    assert status.desired_count == 3


def test_warmpool_set_config_puts_pools(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="PUT",
        url=f"{BASE}/warmpool/config",
        match_json={"pools": [{"template": "general-coding", "desiredCount": 5}]},
        json={"status": "updated", "pools": [{"template": "general-coding", "desiredCount": 5}]},
    )
    with AgentTierClient(api_url=API_URL) as client:
        result = WarmPoolAPI(client).set_config([PoolConfig(template="general-coding", desired_count=5)])
    assert result[0].template == "general-coding"
    assert result[0].desired_count == 5


def test_warmpool_set_config_rejects_empty_pools(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="pools"):
            WarmPoolAPI(client).set_config([])


def test_warmpool_set_config_rejects_out_of_range_desired_count(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="desired_count"):
            WarmPoolAPI(client).set_config([PoolConfig(template="t", desired_count=11)])


def test_warmpool_set_config_rejects_empty_template(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="template"):
            WarmPoolAPI(client).set_config([PoolConfig(template="", desired_count=1)])


def test_warmpool_set_config_non_admin_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """warmpool/config PUT is admin-only on the Router; non-admin -> 403 ->
    AuthorizationError. Auth-scope negative test required by NFR8."""
    httpx_mock.add_response(
        method="PUT",
        url=f"{BASE}/warmpool/config",
        status_code=403,
        json={"error": "admin_required"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            WarmPoolAPI(client).set_config([PoolConfig(template="t", desired_count=1)])


@pytest.mark.asyncio
async def test_async_warmpool_status_and_set_config(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/warmpool/status",
        json={"pools": [{"template": "t", "desiredCount": 1, "readyCount": 1, "pendingCount": 0}]},
    )
    httpx_mock.add_response(
        method="PUT",
        url=f"{BASE}/warmpool/config",
        json={"pools": [{"template": "t", "desiredCount": 2}]},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        api = AsyncWarmPoolAPI(client)
        status = await api.status()
        result = await api.set_config([PoolConfig(template="t", desired_count=2)])
    assert status.pools[0].template == "t"
    assert result[0].desired_count == 2


@pytest.mark.asyncio
async def test_async_warmpool_set_config_non_admin_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """Async twin of the sync 403 negative test above (NFR8)."""
    httpx_mock.add_response(
        method="PUT",
        url=f"{BASE}/warmpool/config",
        status_code=403,
        json={"error": "admin_required"},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            await AsyncWarmPoolAPI(client).set_config([PoolConfig(template="t", desired_count=1)])


# ------- cluster -----------------------------------------------------------


def test_cluster_status_parses_response(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/cluster/status",
        json={
            "nodes": 5,
            "nodesReady": 4,
            "pods": 20,
            "sandboxPods": 15,
            "headroomReady": 1,
            "autoscalerEnabled": True,
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        status = ClusterAPI(client).status()
    assert status.nodes == 5
    assert status.nodes_ready == 4
    assert status.autoscaler_enabled is True


def test_cluster_nodes_parses_response(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/cluster/nodes",
        json={
            "nodes": [
                {
                    "name": "node-1",
                    "ready": True,
                    "instanceType": "t3.large",
                    "nodeGroup": "agenttier-e2e",
                    "allocatable": {"cpuMillis": 2000, "memBytes": 4000000000},
                    "requests": {"cpuMillis": 500, "memBytes": 1000000000},
                }
            ],
            "summary": {
                "ready": 1,
                "total": 1,
                "cpuSaturationPct": 25.0,
                "memSaturationPct": 25.0,
                "allocatable": {"cpuMillis": 2000, "memBytes": 4000000000},
                "requests": {"cpuMillis": 500, "memBytes": 1000000000},
            },
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        resp = ClusterAPI(client).nodes()
    assert resp.nodes[0].name == "node-1"
    assert resp.nodes[0].instance_type == "t3.large"
    assert resp.summary.cpu_saturation_pct == 25.0


def test_cluster_nodes_non_admin_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """cluster/nodes is admin-only on the Router; non-admin -> 403 ->
    AuthorizationError. Auth-scope negative test required by NFR8."""
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/cluster/nodes",
        status_code=403,
        json={"error": "admin_required"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            ClusterAPI(client).nodes()


def test_cluster_headroom_get_parses_response(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/cluster/headroom",
        json={"replicas": 3, "cpu": "500m", "memory": "512Mi", "readyReplicas": 2, "enabled": True},
    )
    with AgentTierClient(api_url=API_URL) as client:
        cfg = ClusterAPI(client).headroom_get()
    assert cfg.replicas == 3
    assert cfg.ready_replicas == 2
    assert cfg.enabled is True


def test_cluster_headroom_set_puts_body(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="PUT",
        url=f"{BASE}/cluster/headroom",
        match_json={"replicas": 4, "cpu": "1", "memory": "1Gi"},
        json={"replicas": 4, "cpu": "1", "memory": "1Gi", "readyReplicas": 0, "enabled": True},
    )
    with AgentTierClient(api_url=API_URL) as client:
        cfg = ClusterAPI(client).headroom_set(4, cpu="1", memory="1Gi")
    assert cfg.replicas == 4


def test_cluster_headroom_set_rejects_out_of_range_replicas(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="replicas"):
            ClusterAPI(client).headroom_set(51)


def test_cluster_headroom_set_non_admin_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """cluster/headroom PUT is admin-only on the Router; non-admin -> 403 ->
    AuthorizationError. Auth-scope negative test required by NFR8."""
    httpx_mock.add_response(
        method="PUT",
        url=f"{BASE}/cluster/headroom",
        status_code=403,
        json={"error": "admin_required"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            ClusterAPI(client).headroom_set(1)


@pytest.mark.asyncio
async def test_async_cluster_status_and_headroom(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/cluster/status",
        json={"nodes": 2, "nodesReady": 2, "pods": 4, "sandboxPods": 2, "headroomReady": 0, "autoscalerEnabled": False},
    )
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/cluster/headroom",
        json={"replicas": 0, "readyReplicas": 0, "enabled": False},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        api = AsyncClusterAPI(client)
        status = await api.status()
        headroom = await api.headroom_get()
    assert status.nodes == 2
    assert headroom.enabled is False


@pytest.mark.asyncio
async def test_async_cluster_nodes_non_admin_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """Async twin of the sync 403 negative test above (NFR8)."""
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/cluster/nodes",
        status_code=403,
        json={"error": "admin_required"},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            await AsyncClusterAPI(client).nodes()
