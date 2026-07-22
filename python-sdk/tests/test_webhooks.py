# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Tests for the webhooks CRUD wrappers and the verify_signature HMAC helper.

``WebhooksAPI``/``AsyncWebhooksAPI`` mirror ``FilesAPI``'s constructor
contract (``__init__(self, client)`` needing only ``client._http``), so
they're usable standalone before the ``client.webhooks`` convenience
property is wired on ``AgentTierClient``/``AsyncAgentTierClient`` in the
hub-wiring task. These tests construct the API classes directly against a
real client for that reason.

Exercises the public surface only; the Router-side behavior (including the
controller delivery loop) is covered by router/controller integration tests.
Uses pytest-httpx to stub HTTP responses.
"""

from __future__ import annotations

import hashlib
import hmac as hmac_module
import json

import pytest
from pytest_httpx import HTTPXMock

from agenttier import AgentTierClient, AsyncAgentTierClient, AuthorizationError
from agenttier.webhooks import AsyncWebhooksAPI, WebhooksAPI, verify_signature

API_URL = "http://router.test"


# ------- create/list/delete/deliveries -------------------------------------


def test_webhooks_create_shows_secret_once(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/webhooks",
        match_json={"url": "https://example.com/hook", "eventTypes": ["sandbox.running"]},
        status_code=201,
        json={
            "id": "wh1",
            "url": "https://example.com/hook",
            "eventTypes": ["sandbox.running"],
            "secret": "raw-secret-shown-once",
            "disabled": False,
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        sub = WebhooksAPI(client).create("https://example.com/hook", ["sandbox.running"])
    assert sub.secret == "raw-secret-shown-once"
    assert sub.id == "wh1"
    assert sub.event_types == ["sandbox.running"]


def test_webhooks_create_forwards_optional_scope(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/webhooks",
        match_json={
            "url": "https://example.com/hook",
            "eventTypes": ["backup.created"],
            "sandboxId": "sb1",
            "namespace": "default",
        },
        status_code=201,
        json={
            "id": "wh2",
            "url": "https://example.com/hook",
            "eventTypes": ["backup.created"],
            "sandboxId": "sb1",
            "namespace": "default",
            "secret": "shh",
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        sub = WebhooksAPI(client).create(
            "https://example.com/hook", ["backup.created"], sandbox_id="sb1", namespace="default"
        )
    assert sub.sandbox_id == "sb1"
    assert sub.namespace == "default"


def test_webhooks_create_rejects_non_https_url(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="https"):
            WebhooksAPI(client).create("http://example.com/hook", ["sandbox.running"])


def test_webhooks_create_rejects_empty_url(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="url"):
            WebhooksAPI(client).create("", ["sandbox.running"])


def test_webhooks_create_rejects_empty_event_types(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="event_types"):
            WebhooksAPI(client).create("https://example.com/hook", [])


def test_webhooks_create_rejects_unknown_event_type(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="unknown event type"):
            WebhooksAPI(client).create("https://example.com/hook", ["sandbox.exploded"])


def test_webhooks_list_excludes_secret(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/webhooks",
        json={
            "webhooks": [
                {"id": "wh1", "url": "https://example.com/hook", "eventTypes": ["sandbox.running"]},
            ]
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        subs = WebhooksAPI(client).list()
    assert len(subs) == 1
    assert subs[0].id == "wh1"
    assert not hasattr(subs[0], "secret")


def test_webhooks_delete_calls_endpoint(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(method="DELETE", url=f"{API_URL}/api/v1/webhooks/wh1", status_code=204)
    with AgentTierClient(api_url=API_URL) as client:
        WebhooksAPI(client).delete("wh1")


def test_webhooks_delete_rejects_empty_id(httpx_mock: HTTPXMock) -> None:
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(ValueError, match="webhook_id"):
            WebhooksAPI(client).delete("")


def test_webhooks_deliveries_parses_response(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/webhooks/wh1/deliveries",
        json={
            "deliveries": [
                {
                    "eventType": "sandbox.running",
                    "timestamp": "2026-07-21T12:00:00Z",
                    "statusCode": 200,
                    "attempt": 1,
                    "success": True,
                },
                {
                    "eventType": "sandbox.error",
                    "statusCode": 500,
                    "attempt": 3,
                    "success": False,
                },
            ]
        },
    )
    with AgentTierClient(api_url=API_URL) as client:
        deliveries = WebhooksAPI(client).deliveries("wh1")
    assert len(deliveries) == 2
    assert deliveries[0].success is True
    assert deliveries[1].attempt == 3
    assert deliveries[1].success is False


def test_webhooks_create_out_of_scope_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """A sandbox-scoped key must be rejected outright on /webhooks* (DD3 —
    not a sandbox-scoped operation at all). NFR8 auth-scope negative test."""
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/webhooks",
        status_code=403,
        json={"error": "forbidden"},
    )
    with AgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            WebhooksAPI(client).create("https://example.com/hook", ["sandbox.running"])


@pytest.mark.asyncio
async def test_async_webhooks_create_list_delete(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{API_URL}/api/v1/webhooks",
        status_code=201,
        json={
            "id": "wh1",
            "url": "https://example.com/hook",
            "eventTypes": ["sandbox.running"],
            "secret": "s3cr3t",
        },
    )
    httpx_mock.add_response(
        method="GET",
        url=f"{API_URL}/api/v1/webhooks",
        json={"webhooks": [{"id": "wh1", "url": "https://example.com/hook", "eventTypes": ["sandbox.running"]}]},
    )
    httpx_mock.add_response(method="DELETE", url=f"{API_URL}/api/v1/webhooks/wh1", status_code=204)
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        api = AsyncWebhooksAPI(client)
        created = await api.create("https://example.com/hook", ["sandbox.running"])
        subs = await api.list()
        await api.delete("wh1")
    assert created.secret == "s3cr3t"
    assert subs[0].id == "wh1"


@pytest.mark.asyncio
async def test_async_webhooks_out_of_scope_raises_authorization_error(httpx_mock: HTTPXMock) -> None:
    """Async twin of the sync 403 negative test above (NFR8)."""
    httpx_mock.add_response(
        method="DELETE",
        url=f"{API_URL}/api/v1/webhooks/wh1",
        status_code=403,
        json={"error": "forbidden"},
    )
    async with AsyncAgentTierClient(api_url=API_URL) as client:
        with pytest.raises(AuthorizationError):
            await AsyncWebhooksAPI(client).delete("wh1")


# ------- verify_signature (HMAC) --------------------------------------------


def _sign(secret: str, payload: bytes) -> str:
    digest = hmac_module.new(secret.encode("utf-8"), payload, hashlib.sha256).hexdigest()
    return f"sha256={digest}"


def test_verify_signature_accepts_valid_signature() -> None:
    secret = "webhook-signing-secret"
    payload = json.dumps({"event": "sandbox.running", "sandboxId": "sb1"}).encode("utf-8")
    header = _sign(secret, payload)
    assert verify_signature(payload, header, secret) is True


def test_verify_signature_rejects_tampered_payload() -> None:
    """A payload modified after signing must fail verification — proves the
    helper actually checks the digest rather than always returning True."""
    secret = "webhook-signing-secret"
    payload = json.dumps({"event": "sandbox.running", "sandboxId": "sb1"}).encode("utf-8")
    header = _sign(secret, payload)
    tampered_payload = json.dumps({"event": "sandbox.running", "sandboxId": "sb2"}).encode("utf-8")
    assert verify_signature(tampered_payload, header, secret) is False


def test_verify_signature_rejects_bit_flipped_signature() -> None:
    """Flip a single hex character in an otherwise-valid signature — this is
    the specific NFR2/sa-review negative test for HMAC correctness (checklist
    item 10b): a near-miss digest must not verify."""
    secret = "webhook-signing-secret"
    payload = b'{"event":"sandbox.running"}'
    header = _sign(secret, payload)
    digest_hex = header[len("sha256=") :]
    flipped_char = "0" if digest_hex[0] != "0" else "1"
    tampered_header = "sha256=" + flipped_char + digest_hex[1:]
    assert verify_signature(payload, tampered_header, secret) is False


def test_verify_signature_rejects_wrong_secret() -> None:
    payload = b'{"event":"sandbox.running"}'
    header = _sign("correct-secret", payload)
    assert verify_signature(payload, header, "wrong-secret") is False


def test_verify_signature_rejects_missing_algorithm_prefix() -> None:
    secret = "webhook-signing-secret"
    payload = b'{"event":"sandbox.running"}'
    digest = hmac_module.new(secret.encode("utf-8"), payload, hashlib.sha256).hexdigest()
    # No "sha256=" prefix -- malformed header, must be rejected, not raise.
    assert verify_signature(payload, digest, secret) is False


def test_verify_signature_rejects_empty_header() -> None:
    assert verify_signature(b"payload", "", "secret") is False


def test_verify_signature_rejects_non_ascii_header_without_raising() -> None:
    """A malformed inbound header must never crash the receiver. hmac.compare_digest
    raises TypeError on a non-ASCII str argument, and the header value here comes
    straight off an attacker-controllable HTTP header — verify_signature must catch
    that and return False rather than letting the exception propagate."""
    payload = b'{"event":"sandbox.running"}'
    header = "sha256=cafédeadbeef"
    assert verify_signature(payload, header, "secret") is False


def test_verify_signature_uses_constant_time_compare(monkeypatch: pytest.MonkeyPatch) -> None:
    """Confirm the implementation route through hmac.compare_digest rather
    than a manual/`==` comparison — the specific regression the sa-review
    flagged as a timing side-channel risk (checklist item 10b)."""
    import agenttier.webhooks as webhooks_module

    calls = []
    real_compare = hmac_module.compare_digest

    def _spy_compare(a: object, b: object) -> bool:
        calls.append((a, b))
        return real_compare(a, b)  # type: ignore[arg-type]

    monkeypatch.setattr(webhooks_module.hmac, "compare_digest", _spy_compare)

    secret = "webhook-signing-secret"
    payload = b'{"event":"sandbox.running"}'
    header = _sign(secret, payload)
    assert verify_signature(payload, header, secret) is True
    assert len(calls) == 1
