# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""stdio transport binding (FR7.5) — for local agent runtimes that spawn the
MCP server as a subprocess.

Run with::

    python -m agenttier.mcp.stdio_main

or, once installed, via the ``agenttier-mcp`` console script (stdio is the
default transport since it's the one every MCP-compatible runtime supports
out of the box).
"""

from __future__ import annotations

from agenttier.mcp.server import build_server


def main() -> None:
    server = build_server()
    server.run(transport="stdio")


if __name__ == "__main__":
    main()
