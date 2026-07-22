# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Shared :class:`agenttier.AgentTierClient` accessor for the MCP tool layer.

MCP tool calls are stateless per-call — there is no notion of a persistent
Python object surviving between calls the way a script holding a `Sandbox`
handle would. Every tool therefore goes through one process-wide client
built from environment configuration (the same ``AGENTTIER_API_URL`` /
``AGENTTIER_API_KEY`` / ``AGENTTIER_TOKEN`` variables the CLI uses), and
sandbox-scoped tools re-resolve the handle via ``client.get_sandbox(id)`` on
every call.
"""

from __future__ import annotations

import os

from agenttier.client import AgentTierClient

_client: AgentTierClient | None = None


def get_client() -> AgentTierClient:
    """Return the process-wide :class:`AgentTierClient`, building it on first use.

    Raises :class:`ValueError` (surfaced by the MCP framework as a clean tool
    error, not a stack trace) if ``AGENTTIER_API_URL`` is unset — the MCP
    server authenticates with the same credential types as the SDK (FR7.2),
    so there is no separate MCP-specific configuration to set up.
    """
    global _client
    if _client is None:
        api_url = os.environ.get("AGENTTIER_API_URL", "")
        if not api_url:
            raise ValueError(
                "AGENTTIER_API_URL is not set. Configure it (and AGENTTIER_API_KEY "
                "or AGENTTIER_TOKEN for auth) before starting the MCP server."
            )
        _client = AgentTierClient(api_url=api_url)
    return _client
