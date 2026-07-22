# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Tests for the governance policy wrappers on the client.

``governance.py`` deliberately mirrors ``sharing.py``'s constructor contract
(``__init__(self, client)`` needing only ``client._http``) so it's usable
standalone before the ``client.governance`` convenience property is wired on
``AgentTierClient``/``AsyncAgentTierClient`` in the hub-wiring task. These
tests construct ``GovernanceAPI``/``AsyncGovernanceAPI`` directly against a
real client for that reason.

Exercises the public surface only; the Router-side behavior is covered by
router integration tests. Uses pytest-httpx to stub HTTP responses.
"""

from __future__ import annotations

import pytest
from pytest_httpx import HTTPXMock

from agenttier import AgentTierClient, AsyncAgentTierClient, AuthorizationError
from agenttier.governance import AsyncGovernanceAPI, GovernanceAPI, Policy

API_URL = "http://router.test"
BASE = f"{API_URL}/api/v1"


def test_governance_list_parses_cluster_and_namespaces(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/governance/policies",
        json={
            "cluster": {"maxSandboxesTotal": 100, "maxCpu": "4"},
            "namespaces": [
                {"namespace": "team-a", "policy": {"maxSandboxesPerUser": 5}},
            ],
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        result = GovernanceAPI(client).list()
    assert result.cluster is not None
    assert result.cluster.max_sandboxes_total == 100
    assert result.cluster.max_cpu == "4"
    assert result.namespaces[0].namespace == "team-a"
    assert result.namespaces[0].policy.max_sandboxes_per_user == 5


def test_governance_get_parses_single_namespace_policy(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/governance/policies/team-a",
        json={"namespace": "team-a", "policy": {"maxSandboxesPerUser": 3, "maxIdleTimeout": "1h"}},
    )
    with AgentTierClient(api_url=API_URL) as client:
        policy = GovernanceAPI(client).get("team-a")
    assert policy.max_sandboxes_per_user == 3
    assert policy.max_idle_timeout == "1h"


def test_governance_get_rejects_empty_namespace(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="namespace"):
            GovernanceAPI(client).get("")


def test_governance_set_cluster_default_puts_to_policies(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="PUT",
        url=f"{BASE}/governance/policies",
        match_json={"maxSandboxesTotal": 50, "maxCpu": "8"},
        json={"scope": "cluster", "policy": {"maxSandboxesTotal": 50, "maxCpu": "8"}},
    )
    with AgentTierClient(api_url=API_URL) as client:
        result = GovernanceAPI(client).set(Policy(max_sandboxes_total=50, max_cpu="8"))
    assert result.max_sandboxes_total == 50
    assert result.max_cpu == "8"


def test_governance_set_namespace_puts_to_namespace_route(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="PUT",
        url=f"{BASE}/governance/policies/team-a",
        match_json={"maxSandboxesPerUser": 2},
        json={"namespace": "team-a", "policy": {"maxSandboxesPerUser": 2}},
    )
    with AgentTierClient(api_url=API_URL) as client:
        result = GovernanceAPI(client).set(Policy(max_sandboxes_per_user=2), namespace="team-a")
    assert result.max_sandboxes_per_user == 2


def test_governance_delete_calls_delete_on_namespace_route(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(method="DELETE", url=f"{BASE}/governance/policies/team-a", status_code=204)
    with AgentTierClient(api_url=API_URL) as client:
        GovernanceAPI(client).delete("team-a")


def test_governance_delete_rejects_empty_namespace(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="namespace"):
            GovernanceAPI(client).delete("")


def test_governance_effective_without_namespace_omits_query_param(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/governance/effective",
        json={"namespace": "default", "policy": {"maxSandboxesTotal": 10}},
    )
    with AgentTierClient(api_url=API_URL) as client:
        result = GovernanceAPI(client).effective()
    assert result.namespace == "default"
    assert result.policy.max_sandboxes_total == 10


def test_governance_effective_with_namespace_sends_query_param(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/governance/effective?namespace=team-a",
        json={"namespace": "team-a", "policy": {"maxSandboxesTotal": 20}},
    )
    with AgentTierClient(api_url=API_URL) as client:
        result = GovernanceAPI(client).effective("team-a")
    assert result.namespace == "team-a"
    assert result.policy.max_sandboxes_total == 20


def test_governance_set_non_admin_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """Governance writes are admin-only on the Router; a non-admin caller
    (or a sandbox-scoped key, which is never authorized for global routes
    per NFR2) must surface as a 403 -> AuthorizationError. Auth-scope
    negative test required by NFR8."""
    httpx_mock.add_response(
        method="PUT",
        url=f"{BASE}/governance/policies",
        status_code=403,
        json={"error": "admin_required"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            GovernanceAPI(client).set(Policy(max_sandboxes_total=1))


@pytest.mark.asyncio
async def test_async_governance_list_and_effective(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/governance/policies",
        json={"cluster": {"maxSandboxesTotal": 100}, "namespaces": []},
    )
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/governance/effective",
        json={"namespace": "default", "policy": {"maxSandboxesTotal": 100}},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        api = AsyncGovernanceAPI(client)
        listed = await api.list()
        eff = await api.effective()
    assert listed.cluster is not None
    assert listed.cluster.max_sandboxes_total == 100
    assert eff.policy.max_sandboxes_total == 100


@pytest.mark.asyncio
async def test_async_governance_delete_non_admin_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """Async twin of the sync 403 negative test above (NFR8)."""
    httpx_mock.add_response(
        method="DELETE",
        url=f"{BASE}/governance/policies/team-a",
        status_code=403,
        json={"error": "admin_required"},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            await AsyncGovernanceAPI(client).delete("team-a")
