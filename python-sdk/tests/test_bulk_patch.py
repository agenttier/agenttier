# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Tests for bulk sandbox operations and PATCH wrappers.

``bulk.py`` deliberately mirrors ``governance.py``'s constructor contract
(``__init__(self, client)`` needing only ``client._http``) so it's usable
standalone before the ``client.sandboxes`` convenience property is wired on
``AgentTierClient``/``AsyncAgentTierClient`` in the hub-wiring task. The PATCH
helpers are plain functions (not Sandbox methods) for the same reason — see
the module docstring in ``bulk.py``.

Exercises the public surface only; the Router-side behavior is covered by
router integration tests. Uses pytest-httpx to stub HTTP responses.
"""

from __future__ import annotations

import pytest
from pytest_httpx import HTTPXMock

from agenttier import AgentTierClient, AsyncAgentTierClient, AuthorizationError, ConflictError
from agenttier.bulk import (
    AsyncSandboxesAPI,
    BulkCreateItem,
    SandboxesAPI,
    async_patch_sandbox,
    build_patch_body,
    patch_sandbox,
)

API_URL = "http://router.test"
BASE = f"{API_URL}/api/v1"


# --- build_patch_body / guard clauses --------------------------------------


def test_build_patch_body_rejects_all_fields_omitted() -> None:
    with pytest.raises(ValueError, match="idle_timeout"):
        build_patch_body()


def test_build_patch_body_includes_only_provided_fields() -> None:
    body = build_patch_body(idle_timeout="30m", labels={"team": "x"})
    assert body == {"idleTimeout": "30m", "labels": {"team": "x"}}


def test_build_patch_body_includes_resources() -> None:
    resources = {"requests": {"cpu": "1", "memory": "2Gi"}, "limits": {"cpu": "2", "memory": "4Gi"}}
    body = build_patch_body(resources=resources)
    assert body == {"resources": resources}


# --- create_bulk local validation ------------------------------------------


def test_create_bulk_rejects_empty_items() -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="items"):
            SandboxesAPI(client).create_bulk([])


def test_create_bulk_rejects_item_missing_template() -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="template"):
            SandboxesAPI(client).create_bulk([BulkCreateItem(template="", name="a")])


def test_create_bulk_rejects_item_missing_name() -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="name"):
            SandboxesAPI(client).create_bulk([BulkCreateItem(template="t", name="")])


# --- create_bulk happy path -------------------------------------------------


def test_create_bulk_posts_items_and_parses_per_item_results(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/bulk",
        match_json={
            "items": [
                {
                    "name": "a",
                    "namespace": "default",
                    "templateRef": {"name": "general-coding", "kind": "ClusterSandboxTemplate"},
                },
                {
                    "name": "b",
                    "namespace": "default",
                    "templateRef": {"name": "bogus", "kind": "ClusterSandboxTemplate"},
                },
            ]
        },
        json={
            "results": [
                {"index": 0, "status": "created", "sandboxId": "a"},
                {"index": 1, "status": "error", "error": "template not found"},
            ]
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        results = SandboxesAPI(client).create_bulk(
            [
                BulkCreateItem(template="general-coding", name="a"),
                BulkCreateItem(template="bogus", name="b"),
            ]
        )
    assert results[0].status == "created"
    assert results[0].sandbox_id == "a"
    assert results[1].status == "error"
    assert results[1].error == "template not found"


def test_create_bulk_forwards_optional_fields(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/bulk",
        match_json={
            "items": [
                {
                    "name": "a",
                    "namespace": "team-a",
                    "templateRef": {"name": "t", "kind": "ClusterSandboxTemplate"},
                    "timeout": "8h",
                    "idleTimeout": "30m",
                    "storage": {"size": "10Gi"},
                }
            ]
        },
        json={"results": [{"index": 0, "status": "created", "sandboxId": "a"}]},
    )
    with AgentTierClient(api_url=API_URL) as client:
        results = SandboxesAPI(client).create_bulk(
            [
                BulkCreateItem(
                    template="t",
                    name="a",
                    namespace="team-a",
                    timeout="8h",
                    idle_timeout="30m",
                    storage_size="10Gi",
                )
            ]
        )
    assert results[0].sandbox_id == "a"


def test_create_bulk_cap_fail_fast_raises_conflict_error(httpx_mock: HTTPXMock) -> None:
    """DD4: a bulk create that would exceed a governance cap is rejected as a
    whole (409), no partial success."""
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/bulk",
        status_code=409,
        json={"error": "quota_would_exceed", "detail": {"maxSandboxesTotal": 10}},
    )
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ConflictError):
            SandboxesAPI(client).create_bulk([BulkCreateItem(template="t", name="a")])


# --- bulk_action local validation ------------------------------------------


def test_bulk_action_rejects_empty_ids() -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="ids"):
            SandboxesAPI(client).bulk_action([], "stop")


def test_bulk_action_rejects_invalid_action() -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="action"):
            SandboxesAPI(client).bulk_action(["a"], "reboot")


def test_bulk_action_rejects_empty_id_entry() -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="ids\\[1\\]"):
            SandboxesAPI(client).bulk_action(["a", ""], "stop")


# --- bulk_action happy path -------------------------------------------------


def test_bulk_action_posts_ids_and_parses_per_item_results(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/bulk-action",
        match_json={"action": "stop", "ids": ["a", "missing"]},
        json={
            "results": [
                {"id": "a", "status": "stopped"},
                {"id": "missing", "status": "error", "error": "sandbox not found"},
            ]
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        results = SandboxesAPI(client).bulk_action(["a", "missing"], "stop")
    assert results[0].id == "a"
    assert results[0].status == "stopped"
    assert results[1].status == "error"
    assert results[1].error == "sandbox not found"


# --- PATCH -------------------------------------------------------------------


def test_patch_sandbox_sends_body_and_parses_response(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="PATCH",
        url=f"{BASE}/sandboxes/sb1",
        match_json={"idleTimeout": "30m", "labels": {"team": "x"}},
        json={
            "sandboxId": "sb1",
            "applied": {"idleTimeout": "immediately", "labels": "immediately"},
            "restartRequired": False,
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        result = patch_sandbox(client._http, "sb1", idle_timeout="30m", labels={"team": "x"})
    assert result.sandbox_id == "sb1"
    assert result.applied["idleTimeout"] == "immediately"
    assert result.restart_required is False


def test_patch_sandbox_resources_reports_restart_required(httpx_mock: HTTPXMock) -> None:
    resources = {"limits": {"cpu": "2", "memory": "4Gi"}}
    httpx_mock.add_response(
        method="PATCH",
        url=f"{BASE}/sandboxes/sb1",
        match_json={"resources": resources},
        json={
            "sandboxId": "sb1",
            "applied": {"resources": "on-restart"},
            "restartRequired": True,
            "message": "resource changes take effect after the sandbox is stopped and resumed",
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        result = patch_sandbox(client._http, "sb1", resources=resources)
    assert result.restart_required is True
    assert result.applied["resources"] == "on-restart"


def test_patch_sandbox_rejects_no_fields() -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="idle_timeout"):
            patch_sandbox(client._http, "sb1")


def test_patch_sandbox_governance_reject_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """FR2.4/NFR3: a PATCH that would exceed governance limits is rejected the
    same way create is."""
    httpx_mock.add_response(
        method="PATCH",
        url=f"{BASE}/sandboxes/sb1",
        status_code=403,
        json={"error": "policy_violation", "violations": [{"rule": "maxCpu"}]},
    )
    with AgentTierClient(api_url=API_URL) as client:
        from agenttier import PolicyViolationError

        with pytest.raises(PolicyViolationError):
            patch_sandbox(client._http, "sb1", resources={"limits": {"cpu": "100"}})


def test_patch_sandbox_out_of_scope_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """A sandbox-scoped key attempting to PATCH a sandbox it isn't bound to
    (or lacking the required action group) must surface as a 403 ->
    AuthorizationError. NFR8 auth-scope negative test."""
    httpx_mock.add_response(
        method="PATCH",
        url=f"{BASE}/sandboxes/sb1",
        status_code=403,
        json={"error": "forbidden"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            patch_sandbox(client._http, "sb1", idle_timeout="30m")


# --- async twins -------------------------------------------------------------


@pytest.mark.asyncio
async def test_async_create_bulk_and_bulk_action(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/bulk",
        json={"results": [{"index": 0, "status": "created", "sandboxId": "a"}]},
    )
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/bulk-action",
        json={"results": [{"id": "a", "status": "deleted"}]},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        api = AsyncSandboxesAPI(client)
        created = await api.create_bulk([BulkCreateItem(template="t", name="a")])
        acted = await api.bulk_action(["a"], "delete")
    assert created[0].sandbox_id == "a"
    assert acted[0].status == "deleted"


@pytest.mark.asyncio
async def test_async_create_bulk_cap_fail_fast_raises_conflict_error(httpx_mock: HTTPXMock) -> None:
    """Async twin of the sync cap fail-fast test above (NFR8)."""
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/bulk",
        status_code=409,
        json={"error": "quota_would_exceed"},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ConflictError):
            await AsyncSandboxesAPI(client).create_bulk([BulkCreateItem(template="t", name="a")])


@pytest.mark.asyncio
async def test_async_patch_sandbox_happy_path(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="PATCH",
        url=f"{BASE}/sandboxes/sb1",
        match_json={"annotations": {"note": "y"}},
        json={
            "sandboxId": "sb1",
            "applied": {"annotations": "immediately"},
            "restartRequired": False,
        },
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        result = await async_patch_sandbox(client._http, "sb1", annotations={"note": "y"})
    assert result.sandbox_id == "sb1"
    assert result.restart_required is False


@pytest.mark.asyncio
async def test_async_patch_sandbox_out_of_scope_raises_authorization_error(
    httpx_mock: HTTPXMock,
) -> None:
    """Async twin of the sync 403 negative test above (NFR8)."""
    httpx_mock.add_response(
        method="PATCH",
        url=f"{BASE}/sandboxes/sb1",
        status_code=403,
        json={"error": "forbidden"},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            await async_patch_sandbox(client._http, "sb1", idle_timeout="30m")
