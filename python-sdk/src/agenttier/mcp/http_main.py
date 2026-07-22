# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""HTTP/SSE transport binding (FR7.5) — for remote/hosted agent runtimes
that connect over the network instead of spawning a subprocess.

Run with::

    python -m agenttier.mcp.http_main --host 0.0.0.0 --port 8765

or, once installed, via ``agenttier-mcp --transport sse`` (see
:mod:`agenttier.mcp.stdio_main` for the console script's stdio default).

Supports both of the underlying MCP SDK's HTTP-family transports:
``streamable-http`` (the modern replacement, default here) and the legacy
``sse``. Both bind the exact same tool set built by
:func:`agenttier.mcp.server.build_server` — only the wire transport differs.
"""

from __future__ import annotations

import argparse

from agenttier.mcp.server import build_server


def main(argv: list[str] | None = None) -> None:
    parser = argparse.ArgumentParser(prog="agenttier-mcp-http", description=__doc__)
    parser.add_argument("--host", default="127.0.0.1", help="Bind address (default: 127.0.0.1)")
    parser.add_argument("--port", type=int, default=8765, help="Bind port (default: 8765)")
    parser.add_argument(
        "--transport",
        choices=("streamable-http", "sse"),
        default="streamable-http",
        help="HTTP-family MCP transport (default: streamable-http)",
    )
    args = parser.parse_args(argv)

    server = build_server(host=args.host, port=args.port)
    server.run(transport=args.transport)


if __name__ == "__main__":
    main()
