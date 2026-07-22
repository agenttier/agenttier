# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""MCP tools mirroring :class:`agenttier.webhooks.WebhooksAPI`.

``agenttier.webhooks.verify_signature`` is intentionally NOT mapped to a
tool: it verifies an *inbound* delivery on the webhook receiver's own HTTP
server, which is a different process from whatever's driving the MCP
server — there's no meaningful "call this tool to verify a signature" flow
since the signing secret and raw request body live on the receiver, not
here. Receivers should import the SDK function directly.
"""

from __future__ import annotations

from typing import Any, Sequence

from agenttier.mcp._client import get_client
from agenttier.mcp._serialize import dump, dump_list


def webhook_create(url: str, event_types: Sequence[str], sandbox_id: str = "", namespace: str = "") -> dict[str, Any]:
    """Register a webhook subscription. The signing secret is returned exactly once.

    :param url: HTTPS URL the Router will POST signed event payloads to.
    :param event_types: Event types to subscribe to (e.g. "sandbox.running", "backup.created").
    :param sandbox_id: Narrow delivery scope to one sandbox. Omit for all visible sandboxes.
    :param namespace: Narrow delivery scope to one namespace. Omit for all visible namespaces.
    """
    client = get_client()
    result = client.webhooks.create(
        url,
        list(event_types),
        sandbox_id=sandbox_id or None,
        namespace=namespace or None,
    )
    return dump(result)


def webhook_delete(webhook_id: str) -> dict[str, str]:
    """Delete a webhook subscription.

    :param webhook_id: The subscription's id.
    """
    client = get_client()
    client.webhooks.delete(webhook_id)
    return {"webhook_id": webhook_id, "status": "deleted"}


def webhook_deliveries(webhook_id: str) -> list[dict[str, Any]]:
    """Return recent delivery attempts for a webhook subscription (debugging aid).

    :param webhook_id: The subscription's id.
    """
    client = get_client()
    return dump_list(client.webhooks.deliveries(webhook_id))


def webhook_list() -> list[dict[str, Any]]:
    """List the caller's own webhook subscriptions."""
    client = get_client()
    return dump_list(client.webhooks.list())
