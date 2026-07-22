# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Tests for the sharing wrappers on Sandbox + AsyncSandbox.

``sharing.py`` deliberately mirrors ``FilesAPI``'s constructor contract
(``__init__(self, sandbox)`` needing only ``sandbox.id`` + ``sandbox._http``)
so it's usable standalone before the ``sandbox.sharing`` convenience property
is wired on ``Sandbox``/``AsyncSandbox`` in the hub-wiring task. These tests
construct ``SharingAPI``/``AsyncSharingAPI`` directly against a real
``Sandbox``/``AsyncSandbox`` handle for that reason.

Exercises the public surface only; the Router-side behavior is covered by
router integration tests. Uses pytest-httpx to stub HTTP responses.
"""

from __future__ import annotations

import pytest
from pytest_httpx import HTTPXMock

from agenttier import AgentTierClient, AsyncAgentTierClient, AuthorizationError
from agenttier.sharing import AsyncSharingAPI, SharingAPI

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


def test_sharing_list_parses_response(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/sandboxes/sb1/share",
        json={
            "users": [{"identity": "alice@example.com", "level": "viewer"}],
            "groups": [],
            "shareLinks": [
                {"id": "lnk1", "level": "viewer", "maxUses": 0, "usedCount": 2},
            ],
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        info = SharingAPI(sb).list()
    assert info.users[0].identity == "alice@example.com"
    assert info.users[0].level == "viewer"
    assert info.share_links[0].id == "lnk1"
    assert info.share_links[0].used_count == 2


def test_sharing_grant_posts_body(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/sandboxes/sb1/share",
        match_json={"identity": "bob@example.com", "level": "collaborator", "kind": "user"},
        json={"users": [{"identity": "bob@example.com", "level": "collaborator"}], "groups": [], "shareLinks": []},
    )
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        info = SharingAPI(sb).grant("bob@example.com", level="collaborator")
    assert info.users[0].level == "collaborator"


def test_sharing_grant_rejects_invalid_level(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        with pytest.raises(ValueError, match="level"):
            SharingAPI(sb).grant("bob@example.com", level="owner")


def test_sharing_grant_rejects_empty_identity(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        with pytest.raises(ValueError, match="identity"):
            SharingAPI(sb).grant("")


def test_sharing_grant_rejects_invalid_kind(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        with pytest.raises(ValueError, match="kind"):
            SharingAPI(sb).grant("bob@example.com", kind="robot")


def test_sharing_revoke_calls_delete(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="DELETE",
        url=f"{API_URL}/api/v1/sandboxes/sb1/share/bob@example.com",
        json={"status": "revoked", "identity": "bob@example.com"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        SharingAPI(sb).revoke("bob@example.com")


def test_sharing_revoke_rejects_empty_identity(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        with pytest.raises(ValueError, match="identity"):
            SharingAPI(sb).revoke("")


def test_sharing_create_link_shows_token_once(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/sandboxes/sb1/share-links",
        match_json={"level": "viewer", "maxUses": 0, "expiresIn": "24h"},
        json={
            "id": "lnk1",
            "token": "raw-token-shown-once",
            "level": "viewer",
            "maxUses": 0,
            "warning": "Store this token now — it is shown only once.",
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        link = SharingAPI(sb).create_link(expires_in="24h")
    assert link.token == "raw-token-shown-once"
    assert link.id == "lnk1"


def test_sharing_create_link_rejects_negative_max_uses(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        with pytest.raises(ValueError, match="max_uses"):
            SharingAPI(sb).create_link(max_uses=-1)


def test_sharing_grant_out_of_scope_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """A sandbox-scoped key attempting to share a sandbox it isn't bound to
    (or lacking the required action group) must surface as a 403 -> AuthorizationError,
    not silently succeed or raise something opaque. NFR8 auth-scope negative test."""
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/sandboxes/sb1/share",
        status_code=403,
        json={"error": "forbidden"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        sb = client.get_sandbox("sb1")
        with pytest.raises(AuthorizationError):
            SharingAPI(sb).grant("bob@example.com")


@pytest.mark.asyncio
async def test_async_sharing_list_and_grant(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/sandboxes/sb1",
        json=_SANDBOX_RESP,
    )
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/sandboxes/sb1/share",
        json={"users": [], "groups": [], "shareLinks": []},
    )
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/sandboxes/sb1/share",
        json={"users": [{"identity": "carol@example.com", "level": "viewer"}], "groups": [], "shareLinks": []},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        sb = await client.get_sandbox("sb1")
        api = AsyncSharingAPI(sb)
        empty = await api.list()
        info = await api.grant("carol@example.com")
    assert empty.users == []
    assert info.users[0].identity == "carol@example.com"


@pytest.mark.asyncio
async def test_async_sharing_out_of_scope_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """Async twin of the sync 403 negative test above (NFR8)."""
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/sandboxes/sb1",
        json=_SANDBOX_RESP,
    )
    httpx_mock.add_response(
        method="DELETE",
        url=f"{API_URL}/api/v1/sandboxes/sb1/share/bob@example.com",
        status_code=403,
        json={"error": "forbidden"},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        sb = await client.get_sandbox("sb1")
        with pytest.raises(AuthorizationError):
            await AsyncSharingAPI(sb).revoke("bob@example.com")
