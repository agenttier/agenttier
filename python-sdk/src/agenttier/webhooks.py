# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Sync + async wrappers around the webhook subscription REST surface, plus
an HMAC signature-verification helper for inbound webhook receivers.

Wraps ``POST/GET /webhooks``, ``DELETE /webhooks/{id}``, and
``GET /webhooks/{id}/deliveries``. Use ``client.webhooks`` to obtain a
sync/async instance; don't construct directly.

``verify_signature`` is a free function (not sandbox/client-scoped) since it
runs on the *receiver* side of a webhook delivery — a plain HTTP handler that
never touches the SDK's ``httpx`` clients at all. It exists so consumers
don't hand-roll HMAC comparison and, in particular, don't reach for a
non-constant-time ``==`` string compare (a timing side-channel — see the
threat-model review in ``.claude/specs/dev4-newapis/sa-review.md``).
"""

from __future__ import annotations

import hmac
from datetime import datetime
from typing import TYPE_CHECKING, Optional

from pydantic import Field

from agenttier._http import raise_for_status
from agenttier.models import _Model

if TYPE_CHECKING:  # pragma: no cover
    import httpx

    from agenttier.async_client import AsyncAgentTierClient
    from agenttier.client import AgentTierClient

#: Fixed event-type vocabulary the Router accepts (requirements.md FR5.2).
#: Kept here (rather than an enum) so a Router-side addition doesn't require
#: an SDK release before it can be subscribed to.
_VALID_EVENT_TYPES = frozenset(
    {
        "sandbox.creating",
        "sandbox.running",
        "sandbox.stopped",
        "sandbox.error",
        "sandbox.deleting",
        "backup.created",
        "backup.pruned",
        "share.granted",
        "share.revoked",
        "agent.invoke.started",
        "agent.invoke.completed",
        "agent.invoke.failed",
    }
)

_SIGNATURE_PREFIX = "sha256="


class WebhookSubscription(_Model):
    """A registered webhook subscription, as returned by GET/DELETE.

    Never carries the signing secret — that's only present on the
    :class:`WebhookSubscriptionCreated` response, shown once at creation.
    """

    id: str
    url: str
    event_types: list[str] = Field(default_factory=list, alias="eventTypes")
    sandbox_id: Optional[str] = Field(default=None, alias="sandboxId")
    namespace: Optional[str] = None
    disabled: bool = False


class WebhookSubscriptionCreated(WebhookSubscription):
    """Response from creating a subscription — the raw HMAC secret is shown once."""

    secret: str


class WebhookDelivery(_Model):
    """One delivery attempt, as returned by GET /webhooks/{id}/deliveries."""

    event_type: str = Field(alias="eventType")
    timestamp: Optional[datetime] = None
    status_code: Optional[int] = Field(default=None, alias="statusCode")
    attempt: int = 0
    success: bool = False


def _validate_create_args(url: str, event_types: list[str]) -> None:
    if not url:
        raise ValueError("url must be a non-empty string")
    if not url.startswith("https://"):
        raise ValueError("url must use https:// (webhook deliveries are signed but not encrypted otherwise)")
    if not event_types:
        raise ValueError("event_types must be a non-empty list")
    unknown = set(event_types) - _VALID_EVENT_TYPES
    if unknown:
        raise ValueError(f"unknown event type(s): {sorted(unknown)}; valid types: {sorted(_VALID_EVENT_TYPES)}")


def verify_signature(payload: bytes, header: str, secret: str) -> bool:
    """Verify an inbound webhook delivery's ``X-AgentTier-Signature`` header.

    ``payload`` must be the *raw* request body bytes exactly as received —
    do not re-serialize a parsed JSON object before verifying, since
    re-marshaling can reorder keys or normalize whitespace and produce a
    signature mismatch even for a genuine delivery (the Router signs the
    exact bytes it sends, never a re-marshaled form).

    ``header`` is the full ``X-AgentTier-Signature`` header value, expected
    in the form ``"sha256=<hex-encoded-hmac>"``.

    Uses :func:`hmac.compare_digest` for a constant-time comparison — never
    compare digests with ``==``, which leaks timing information proportional
    to the number of matching prefix bytes and can let a network-positioned
    attacker forge a valid signature via repeated timing probes.

    Returns ``False`` (never raises) for a malformed header, an unsupported
    algorithm prefix, or a mismatched digest — callers should treat any
    ``False`` result as "reject the delivery." ``header`` is attacker-
    controllable (it comes straight off an inbound HTTP request on the
    receiver side), so a non-ASCII or otherwise malformed value must not
    propagate an exception: ``hmac.compare_digest`` raises ``TypeError`` for
    a non-ASCII ``str`` argument, which is caught here and treated as a
    verification failure rather than crashing the caller.
    """
    if not header or not header.startswith(_SIGNATURE_PREFIX):
        return False
    provided_hex = header[len(_SIGNATURE_PREFIX) :]
    expected = hmac.new(secret.encode("utf-8"), payload, "sha256").hexdigest()
    try:
        return hmac.compare_digest(provided_hex, expected)
    except TypeError:
        return False


class WebhooksAPI:
    """Sync wrapper for ``/webhooks*``."""

    def __init__(self, client: "AgentTierClient") -> None:
        self._http: "httpx.Client" = client._http

    def create(
        self,
        url: str,
        event_types: list[str],
        sandbox_id: Optional[str] = None,
        namespace: Optional[str] = None,
    ) -> WebhookSubscriptionCreated:
        """Register a webhook subscription. The signing secret is shown exactly once.

        ``event_types`` must be drawn from the fixed vocabulary the Router
        accepts (sandbox phase transitions, backup/share/invoke events).
        ``sandbox_id``/``namespace`` narrow delivery scope; omit both for a
        subscription that receives matching events across the caller's
        visible sandboxes.
        """
        _validate_create_args(url, event_types)
        body: dict[str, object] = {"url": url, "eventTypes": event_types}
        if sandbox_id is not None:
            body["sandboxId"] = sandbox_id
        if namespace is not None:
            body["namespace"] = namespace
        resp = self._http.post("/webhooks", json=body)
        raise_for_status(resp)
        return WebhookSubscriptionCreated.model_validate(resp.json())

    def delete(self, webhook_id: str) -> None:
        """Delete a webhook subscription."""
        if not webhook_id:
            raise ValueError("webhook_id must be a non-empty string")
        resp = self._http.delete(f"/webhooks/{webhook_id}")
        raise_for_status(resp)

    def deliveries(self, webhook_id: str) -> list[WebhookDelivery]:
        """Return recent delivery attempts for a subscription (debugging aid)."""
        if not webhook_id:
            raise ValueError("webhook_id must be a non-empty string")
        resp = self._http.get(f"/webhooks/{webhook_id}/deliveries")
        raise_for_status(resp)
        return [WebhookDelivery.model_validate(d) for d in (resp.json().get("deliveries") or [])]

    # NOTE: `list` is defined last in this class body. mypy resolves a bare
    # `list[...]` annotation against whatever name `list` is bound to at that
    # point in the class namespace — once this method exists, `list` refers
    # to the method, not the builtin, for any annotation appearing *after*
    # it. Keeping it last avoids "Function ... is not valid as a type".
    def list(self) -> list[WebhookSubscription]:
        """List the caller's own webhook subscriptions."""
        resp = self._http.get("/webhooks")
        raise_for_status(resp)
        return [WebhookSubscription.model_validate(s) for s in (resp.json().get("webhooks") or [])]


class AsyncWebhooksAPI:
    """Async mirror of :class:`WebhooksAPI`. See its docstrings for the full contract."""

    def __init__(self, client: "AsyncAgentTierClient") -> None:
        self._http: "httpx.AsyncClient" = client._http

    async def create(
        self,
        url: str,
        event_types: list[str],
        sandbox_id: Optional[str] = None,
        namespace: Optional[str] = None,
    ) -> WebhookSubscriptionCreated:
        _validate_create_args(url, event_types)
        body: dict[str, object] = {"url": url, "eventTypes": event_types}
        if sandbox_id is not None:
            body["sandboxId"] = sandbox_id
        if namespace is not None:
            body["namespace"] = namespace
        resp = await self._http.post("/webhooks", json=body)
        raise_for_status(resp)
        return WebhookSubscriptionCreated.model_validate(resp.json())

    async def delete(self, webhook_id: str) -> None:
        if not webhook_id:
            raise ValueError("webhook_id must be a non-empty string")
        resp = await self._http.delete(f"/webhooks/{webhook_id}")
        raise_for_status(resp)

    async def deliveries(self, webhook_id: str) -> list[WebhookDelivery]:
        if not webhook_id:
            raise ValueError("webhook_id must be a non-empty string")
        resp = await self._http.get(f"/webhooks/{webhook_id}/deliveries")
        raise_for_status(resp)
        return [WebhookDelivery.model_validate(d) for d in (resp.json().get("deliveries") or [])]

    # See the comment on WebhooksAPI.list — kept last for the same mypy reason.
    async def list(self) -> list[WebhookSubscription]:
        resp = await self._http.get("/webhooks")
        raise_for_status(resp)
        return [WebhookSubscription.model_validate(s) for s in (resp.json().get("webhooks") or [])]
