# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Tests for the analytics/audit/admin wrappers on AgentTierClient + AsyncAgentTierClient.

``AnalyticsAPI``/``AuditAPI``/``AdminAPI`` (and their async twins) mirror
``FilesAPI``'s constructor contract (``__init__(self, client)`` needing only
``client._http``), so it's usable standalone before the ``client.analytics``
/``client.audit``/``client.admin`` convenience properties are wired on
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
from agenttier.admin import AdminAPI, AsyncAdminAPI
from agenttier.analytics import AnalyticsAPI, AsyncAnalyticsAPI
from agenttier.audit import AsyncAuditAPI, AuditAPI

API_URL = "http://router.test"


# ------- analytics ---------------------------------------------------------


def test_analytics_usage_parses_response(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/analytics/usage",
        json={
            "total_sandboxes": 5,
            "status_breakdown": {"Running": 3, "Stopped": 2},
            "template_breakdown": {"general-coding": 5},
            "avg_startup_ms": 4200,
            "startup_sample_count": 5,
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        usage = AnalyticsAPI(client).usage()
    assert usage.total_sandboxes == 5
    assert usage.status_breakdown == {"Running": 3, "Stopped": 2}
    assert usage.avg_startup_ms == 4200


def test_analytics_costs_parses_response(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/analytics/costs",
        json={
            "running_sandboxes": 2,
            "stopped_sandboxes": 1,
            "total_hourly_compute": 0.18,
            "total_hourly_storage": 0.03,
            "total_estimated_monthly": 151.2,
            "per_template": [{"template": "general-coding", "hourly_cost": 0.18, "count": 2}],
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        costs = AnalyticsAPI(client).costs()
    assert costs.running_sandboxes == 2
    assert costs.per_template[0].template == "general-coding"
    assert costs.per_template[0].count == 2


def test_analytics_usage_requires_admin(httpx_mock: HTTPXMock) -> None:
    """Non-admin callers (or an out-of-scope sandbox-scoped key, which is
    never valid on this global endpoint) get a 403 -> AuthorizationError.
    NFR8 auth-scope negative test."""
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/analytics/usage",
        status_code=403,
        json={"error": "forbidden"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            AnalyticsAPI(client).usage()


@pytest.mark.asyncio
async def test_async_analytics_usage_and_costs(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/analytics/usage",
        json={
            "total_sandboxes": 1,
            "status_breakdown": {},
            "template_breakdown": {},
            "avg_startup_ms": 0,
            "startup_sample_count": 0,
        },
    )
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/analytics/costs",
        json={
            "running_sandboxes": 0,
            "stopped_sandboxes": 0,
            "total_hourly_compute": 0.0,
            "total_hourly_storage": 0.0,
            "total_estimated_monthly": 0.0,
            "per_template": [],
        },
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        api = AsyncAnalyticsAPI(client)
        usage = await api.usage()
        costs = await api.costs()
    assert usage.total_sandboxes == 1
    assert costs.running_sandboxes == 0


@pytest.mark.asyncio
async def test_async_analytics_costs_requires_admin(httpx_mock: HTTPXMock) -> None:
    """Async twin of the sync 403 negative test above (NFR8)."""
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/analytics/costs",
        status_code=403,
        json={"error": "forbidden"},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            await AsyncAnalyticsAPI(client).costs()


# ------- audit ---------------------------------------------------------


def test_audit_list_events_parses_response(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/audit/events",
        json={
            "events": [
                {
                    "timestamp": "2026-07-21T12:00:00Z",
                    "eventType": "Created",
                    "sandboxId": "sb1",
                    "sandboxName": "sb1",
                    "namespace": "default",
                    "userEmail": "",
                    "details": {"reason": "sandbox created"},
                }
            ]
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        events = AuditAPI(client).list_events()
    assert len(events) == 1
    assert events[0].event_type == "Created"
    assert events[0].sandbox_id == "sb1"


def test_audit_list_events_empty(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(method="GET", url=f"{API_URL}/api/v1/audit/events", json={"events": []})
    with AgentTierClient(api_url=API_URL) as client:
        events = AuditAPI(client).list_events()
    assert events == []


def test_audit_list_events_requires_admin(httpx_mock: HTTPXMock) -> None:
    """NFR8 auth-scope negative test — non-admin caller is rejected."""
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/audit/events",
        status_code=403,
        json={"error": "forbidden"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            AuditAPI(client).list_events()


@pytest.mark.asyncio
async def test_async_audit_list_events(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/audit/events",
        json={"events": [{"eventType": "Deleted", "sandboxId": "sb2"}]},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        events = await AsyncAuditAPI(client).list_events()
    assert events[0].event_type == "Deleted"


@pytest.mark.asyncio
async def test_async_audit_list_events_requires_admin(httpx_mock: HTTPXMock) -> None:
    """Async twin of the sync 403 negative test above (NFR8)."""
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/audit/events",
        status_code=403,
        json={"error": "forbidden"},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            await AsyncAuditAPI(client).list_events()


# ------- admin ---------------------------------------------------------


def test_admin_sandboxes_parses_response(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/admin/sandboxes",
        json={
            "sandboxes": [
                {"sandboxId": "sb1", "name": "sb1", "namespace": "default", "status": "Running"},
            ]
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        sandboxes = AdminAPI(client).sandboxes()
    assert len(sandboxes) == 1
    assert sandboxes[0].sandbox_id == "sb1"


def test_admin_sandboxes_empty(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(method="GET", url=f"{API_URL}/api/v1/admin/sandboxes", json={"sandboxes": []})
    with AgentTierClient(api_url=API_URL) as client:
        assert AdminAPI(client).sandboxes() == []


def test_admin_sharing_returns_raw_mapping(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(method="GET", url=f"{API_URL}/api/v1/admin/sharing", json={"note": "unshaped"})
    with AgentTierClient(api_url=API_URL) as client:
        result = AdminAPI(client).sharing()
    assert result == {"note": "unshaped"}


def test_admin_sandboxes_requires_admin(httpx_mock: HTTPXMock) -> None:
    """NFR8 auth-scope negative test — non-admin caller is rejected."""
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/admin/sandboxes",
        status_code=403,
        json={"error": "forbidden"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            AdminAPI(client).sandboxes()


@pytest.mark.asyncio
async def test_async_admin_sandboxes_and_sharing(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(method="GET", url=f"{API_URL}/api/v1/admin/sandboxes", json={"sandboxes": []})
    httpx_mock.add_response(method="GET", url=f"{API_URL}/api/v1/admin/sharing", json={})
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        api = AsyncAdminAPI(client)
        sandboxes = await api.sandboxes()
        sharing = await api.sharing()
    assert sandboxes == []
    assert sharing == {}


@pytest.mark.asyncio
async def test_async_admin_sharing_requires_admin(httpx_mock: HTTPXMock) -> None:
    """Async twin of the sync 403 negative test above (NFR8)."""
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/admin/sharing",
        status_code=403,
        json={"error": "forbidden"},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            await AsyncAdminAPI(client).sharing()
