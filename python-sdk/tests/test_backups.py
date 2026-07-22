# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Tests for the backup/restore wrappers on Sandbox + AsyncSandbox.

``backups.py`` deliberately mirrors ``FilesAPI``/``SharingAPI``'s constructor
contract (``__init__(self, sandbox)`` needing only ``sandbox.id`` +
``sandbox._http``) so it's usable standalone before the ``sandbox.backups``
convenience property is wired on ``Sandbox``/``AsyncSandbox`` in the
hub-wiring task. These tests construct ``BackupsAPI``/``AsyncBackupsAPI``
directly against a real ``Sandbox``/``AsyncSandbox`` handle for that reason.

Exercises the public surface only; the Router-side behavior is covered by
router integration tests. Uses pytest-httpx to stub HTTP responses.
"""

from __future__ import annotations

import pytest
from pytest_httpx import HTTPXMock

from agenttier import AgentTierClient, AsyncAgentTierClient, AuthorizationError, NotFoundError
from agenttier.backups import AsyncBackupsAPI, BackupsAPI

API_URL = "http://router.test"

_SANDBOX_RESP = {
    "sandboxId": "sb1",
    "name": "sb1",
    "namespace": "default",
    "status": "Running",
}


def _register_get_sandbox(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/sandboxes/sb1",
        json=_SANDBOX_RESP,
    )


def test_backups_list_parses_response(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/sandboxes/sb1/backups",
        json={
            "backups": [
                {
                    "name": "sb1-snap-100",
                    "createdAt": "2026-07-01T00:00:00Z",
                    "kind": "scheduled-backup",
                    "readyToUse": True,
                    "restoreSize": "5Gi",
                },
            ]
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        backups = BackupsAPI(sb).list()
    assert backups[0].name == "sb1-snap-100"
    assert backups[0].kind == "scheduled-backup"
    assert backups[0].ready_to_use is True
    assert backups[0].restore_size == "5Gi"


def test_backups_list_accepts_bare_array_response(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/sandboxes/sb1/backups",
        json=[{"name": "sb1-snap-1", "kind": "scheduled-backup", "readyToUse": False}],
    )
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        backups = BackupsAPI(sb).list()
    assert len(backups) == 1
    assert backups[0].ready_to_use is False


def test_backups_create_posts_optional_snapshot_class(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/sandboxes/sb1/backups",
        match_json={"snapshotClass": "csi-hostpath-snapclass"},
        json={"name": "sb1-snap-200", "kind": "scheduled-backup", "readyToUse": False},
    )
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        info = BackupsAPI(sb).create(snapshot_class="csi-hostpath-snapclass")
    assert info.name == "sb1-snap-200"
    assert info.ready_to_use is False


def test_backups_create_omits_body_when_no_snapshot_class(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/sandboxes/sb1/backups",
        match_json={},
        json={"name": "sb1-snap-201", "kind": "scheduled-backup", "readyToUse": False},
    )
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        info = BackupsAPI(sb).create()
    assert info.name == "sb1-snap-201"


def test_backups_restore_returns_sandbox_proxy(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/sandboxes/sb1/backups/sb1-snap-100/restore",
        match_json={},
        json={"name": "sb1-restored", "namespace": "default"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        restored = BackupsAPI(sb).restore("sb1-snap-100")
    assert restored.id == "sb1-restored"
    assert restored.namespace == "default"


def test_backups_restore_forwards_name(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/sandboxes/sb1/backups/sb1-snap-100/restore",
        match_json={"name": "my-restore"},
        json={"name": "my-restore", "namespace": "default"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        restored = BackupsAPI(sb).restore("sb1-snap-100", name="my-restore")
    assert restored.id == "my-restore"


def test_backups_restore_rejects_empty_snapshot_name(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        with pytest.raises(ValueError, match="snapshot_name"):
            BackupsAPI(sb).restore("")


def test_backups_restore_pruned_snapshot_raises_not_found(httpx_mock: HTTPXMock) -> None:
    """Race between list and restore: a pruned snapshot must surface a clear
    404, not a silent no-op."""
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/sandboxes/sb1/backups/gone/restore",
        status_code=404,
        json={"error": "snapshot not found"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        with pytest.raises(NotFoundError):
            BackupsAPI(sb).restore("gone")


def test_backups_delete_calls_delete(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="DELETE",
        url=f"{API_URL}/api/v1/sandboxes/sb1/backups/sb1-snap-100",
        json={"status": "deleted"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        BackupsAPI(sb).delete("sb1-snap-100")


def test_backups_delete_rejects_empty_snapshot_name(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        with pytest.raises(ValueError, match="snapshot_name"):
            BackupsAPI(sb).delete("")


def test_backups_create_out_of_scope_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """A sandbox-scoped key attempting a backup op it isn't bound/scoped to
    must surface as a 403 -> AuthorizationError. NFR8 auth-scope negative test."""
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/sandboxes/sb1/backups",
        status_code=403,
        json={"error": "forbidden"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        with pytest.raises(AuthorizationError):
            BackupsAPI(sb).create()


@pytest.mark.asyncio
async def test_async_backups_list_and_create(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/sandboxes/sb1",
        json=_SANDBOX_RESP,
    )
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/sandboxes/sb1/backups",
        json={"backups": []},
    )
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/sandboxes/sb1/backups",
        json={"name": "sb1-snap-300", "kind": "scheduled-backup", "readyToUse": False},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        sb = await client.get_sandbox("sb1")
        api = AsyncBackupsAPI(sb)
        empty = await api.list()
        info = await api.create()
    assert empty == []
    assert info.name == "sb1-snap-300"


@pytest.mark.asyncio
async def test_async_backups_restore_out_of_scope_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """Async twin of the sync 403 negative test above (NFR8)."""
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/sandboxes/sb1",
        json=_SANDBOX_RESP,
    )
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/sandboxes/sb1/backups/sb1-snap-100/restore",
        status_code=403,
        json={"error": "forbidden"},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        sb = await client.get_sandbox("sb1")
        with pytest.raises(AuthorizationError):
            await AsyncBackupsAPI(sb).restore("sb1-snap-100")
