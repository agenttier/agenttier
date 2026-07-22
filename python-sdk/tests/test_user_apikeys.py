# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Tests for the user-preferences and API-key wrappers on the client.

``user.py``/``apikeys.py`` deliberately mirror ``governance.py``'s
constructor contract (``__init__(self, client)`` needing only
``client._http``) so they're usable standalone before the
``client.user``/``client.api_keys`` convenience properties are wired on
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
from agenttier.apikeys import AsyncAPIKeysAPI, APIKeysAPI
from agenttier.user import AsyncUserAPI, UserAPI

API_URL = "http://router.test"
BASE = f"{API_URL}/api/v1"


# --- UserAPI / AsyncUserAPI -------------------------------------------------


def test_user_preferences_get_returns_empty_dict_when_none_saved(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(method="GET", url=f"{BASE}/user/preferences", json={})
    with AgentTierClient(api_url=API_URL) as client:
        prefs = UserAPI(client).preferences_get()
    assert prefs == {}


def test_user_preferences_get_parses_saved_values(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET", url=f"{BASE}/user/preferences", json={"theme": "dark", "editor": "vim"}
    )
    with AgentTierClient(api_url=API_URL) as client:
        prefs = UserAPI(client).preferences_get()
    assert prefs == {"theme": "dark", "editor": "vim"}


def test_user_preferences_set_puts_body_and_returns_stored_value(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="PUT",
        url=f"{BASE}/user/preferences",
        match_json={"theme": "light"},
        json={"theme": "light"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        stored = UserAPI(client).preferences_set({"theme": "light"})
    assert stored == {"theme": "light"}


def test_user_preferences_set_rejects_non_dict(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="dict"):
            UserAPI(client).preferences_set([1, 2, 3])  # type: ignore[arg-type]


def test_user_preferences_get_out_of_scope_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """A sandbox-scoped key has no valid use for global user-preferences
    endpoints; the Router must reject with 403. NFR8 auth-scope negative test."""
    httpx_mock.add_response(method="GET", url=f"{BASE}/user/preferences", status_code=403, json={"error": "forbidden"})
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            UserAPI(client).preferences_get()


@pytest.mark.asyncio
async def test_async_user_preferences_roundtrip(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(method="GET", url=f"{BASE}/user/preferences", json={"theme": "dark"})
    httpx_mock.add_response(
        method="PUT", url=f"{BASE}/user/preferences", json={"theme": "light"}
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        api = AsyncUserAPI(client)
        got = await api.preferences_get()
        stored = await api.preferences_set({"theme": "light"})
    assert got == {"theme": "dark"}
    assert stored == {"theme": "light"}


# --- APIKeysAPI / AsyncAPIKeysAPI ------------------------------------------


def test_apikeys_list_parses_metadata(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/user/api-keys",
        json={
            "keys": [
                {"id": "agenttier-apikey-abc", "userId": "u-1", "name": "ci", "createdAt": "2026-07-01T00:00:00Z"},
                {
                    "id": "agenttier-apikey-def",
                    "userId": "u-1",
                    "name": "sbx-key",
                    "sandboxId": "sbx-1",
                    "actionGroups": ["run-command", "files:read"],
                },
            ]
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        keys = APIKeysAPI(client).list()
    assert keys[0].id == "agenttier-apikey-abc"
    assert keys[0].name == "ci"
    assert keys[1].sandbox_id == "sbx-1"
    assert keys[1].action_groups == ["run-command", "files:read"]


def test_apikeys_create_returns_plaintext_key_once(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/user/api-keys",
        match_json={"name": "ci"},
        json={
            "id": "agenttier-apikey-abc",
            "key": "atk_secretvalue",
            "name": "ci",
            "warning": "Store this key now — it cannot be retrieved again.",
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        created = APIKeysAPI(client).create(name="ci")
    assert created.key == "atk_secretvalue"
    assert created.id == "agenttier-apikey-abc"


def test_apikeys_create_forwards_expires_in(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/user/api-keys",
        match_json={"name": "temp", "expiresIn": "720h"},
        json={"id": "k1", "key": "atk_x", "name": "temp"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        created = APIKeysAPI(client).create(name="temp", expires_in="720h")
    assert created.id == "k1"


def test_apikeys_create_rejects_empty_expires_in(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="expires_in"):
            APIKeysAPI(client).create(expires_in="")


def test_apikeys_create_scoped_key_forwards_sandbox_id_and_action_groups(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/user/api-keys",
        match_json={"name": "agent-key", "sandboxId": "sbx-1", "actionGroups": ["run-command", "resume"]},
        json={
            "id": "k2",
            "key": "atk_scoped",
            "name": "agent-key",
            "sandboxId": "sbx-1",
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        created = APIKeysAPI(client).create(
            name="agent-key", sandbox_id="sbx-1", action_groups=["run-command", "resume"]
        )
    assert created.key == "atk_scoped"


def test_apikeys_create_rejects_action_groups_without_sandbox_id(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="sandbox_id"):
            APIKeysAPI(client).create(action_groups=["run-command"])


def test_apikeys_revoke_calls_delete(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="DELETE",
        url=f"{BASE}/user/api-keys/agenttier-apikey-abc",
        json={"status": "revoked", "id": "agenttier-apikey-abc"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        APIKeysAPI(client).revoke("agenttier-apikey-abc")


def test_apikeys_revoke_rejects_empty_key_id(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="key_id"):
            APIKeysAPI(client).revoke("")


def test_apikeys_revoke_out_of_scope_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """A caller revoking a key they don't own gets a 403. NFR8 auth-scope negative test."""
    httpx_mock.add_response(
        method="DELETE",
        url=f"{BASE}/user/api-keys/not-mine",
        status_code=403,
        json={"error": "forbidden"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            APIKeysAPI(client).revoke("not-mine")


@pytest.mark.asyncio
async def test_async_apikeys_create_and_revoke(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/user/api-keys",
        json={"id": "k3", "key": "atk_async", "name": "async-key"},
    )
    httpx_mock.add_response(
        method="DELETE", url=f"{BASE}/user/api-keys/k3", json={"status": "revoked", "id": "k3"}
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        api = AsyncAPIKeysAPI(client)
        created = await api.create(name="async-key")
        await api.revoke(created.id)
    assert created.key == "atk_async"


@pytest.mark.asyncio
async def test_async_apikeys_revoke_out_of_scope_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """Async twin of the sync 403 negative test above (NFR8)."""
    httpx_mock.add_response(
        method="DELETE",
        url=f"{BASE}/user/api-keys/not-mine",
        status_code=403,
        json={"error": "forbidden"},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            await AsyncAPIKeysAPI(client).revoke("not-mine")
