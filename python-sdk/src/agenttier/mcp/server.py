# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Builds a :class:`mcp.server.fastmcp.FastMCP` instance from the
transport-agnostic tool functions in :mod:`agenttier.mcp.tools`.

FR7.1: one MCP tool per public SDK method, generated (registered) from the
same ``ALL_TOOLS`` mapping that a future stdio-only or HTTP/SSE-only build
would use — the tool set and the SDK never drift independently because
every tool function is a thin delegation into the SDK's own client/sandbox
objects (see :mod:`agenttier.mcp.tools.sandboxes` etc.), not a
reimplementation.

This module only *builds* the server; it does not run it — see
:mod:`agenttier.mcp.stdio_main` and :mod:`agenttier.mcp.http_main` for the
two transport bindings (FR7.5), both thin wrappers around
:func:`build_server`.
"""

from __future__ import annotations

from mcp.server.fastmcp import FastMCP

from agenttier.mcp.tools import ALL_TOOLS

SERVER_NAME = "agenttier"
SERVER_INSTRUCTIONS = (
    "Tools for managing AgentTier sandboxes — isolated, persistent Kubernetes "
    "environments for AI agents and humans. Create a sandbox with "
    "sandbox_create, then use sandbox_run_command/file_read/file_write to "
    "work inside it. Most fleet-wide tools (governance_*, analytics_*, "
    "audit_*, admin_*, warmpool_*, cluster_nodes, cluster_set_headroom) "
    "require the caller's credential to carry admin privileges."
)


def build_server(**fastmcp_kwargs: object) -> FastMCP:
    """Construct a :class:`FastMCP` instance with every AgentTier tool registered.

    ``fastmcp_kwargs`` are forwarded to :class:`FastMCP` (e.g. ``host``/
    ``port`` for the HTTP/SSE binding); callers building the stdio binding
    can omit them entirely.
    """
    server = FastMCP(name=SERVER_NAME, instructions=SERVER_INSTRUCTIONS, **fastmcp_kwargs)  # type: ignore[arg-type]
    for tool_name, fn in ALL_TOOLS.items():
        server.add_tool(fn, name=tool_name)
    return server
