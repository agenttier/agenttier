# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""MCP (Model Context Protocol) server for AgentTier — FR7, stretch goal.

Mirrors the Python SDK's public method surface 1:1 (one MCP tool per SDK
method, see :mod:`agenttier.mcp.tools`) so any MCP-compatible agent runtime
can drive AgentTier without hand-rolling REST calls. Requires the optional
``mcp`` extra: ``pip install agenttier[mcp]``.

Two transport bindings share the same tool implementations (FR7.5):

* :mod:`agenttier.mcp.stdio_main` — stdio, for locally-spawned subprocesses.
* :mod:`agenttier.mcp.http_main` — HTTP (streamable-http or legacy SSE), for
  remote/hosted agent runtimes.

Both are thin wrappers around :func:`agenttier.mcp.server.build_server`.
"""

from __future__ import annotations
