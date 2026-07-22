# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""MCP tools mirroring :class:`agenttier.sandbox.FilesAPI`.

Intentionally NOT mapped to a tool: ``FilesAPI.download()``/``upload()`` —
both take a *local filesystem path* on the machine running the SDK, which
has no meaning for an MCP client that may be running on a different host
than the MCP server. ``file_read``/``file_write`` cover the same ground
in-band (content travels as the tool argument/result instead of a local
path), and ``sandbox_archive`` below streams a workspace .zip through the
same in-band pattern rather than writing to a local destination.
"""

from __future__ import annotations

from typing import Any

from agenttier.mcp._client import get_client
from agenttier.mcp._serialize import bytes_for_transport, bytes_from_transport, dump_list


def file_list(sandbox_id: str, path: str = "/workspace") -> list[dict[str, Any]]:
    """List a directory inside a sandbox.

    :param sandbox_id: The sandbox's id.
    :param path: Directory path inside the sandbox to list.
    """
    client = get_client()
    return dump_list(client.get_sandbox(sandbox_id).files.list(path))


def file_read(sandbox_id: str, path: str) -> dict[str, Any]:
    """Read a file from a sandbox.

    Returns ``{"encoding": "utf-8"|"base64", "content": <str>}`` — UTF-8 text
    is returned directly; binary files are base64-encoded.

    :param sandbox_id: The sandbox's id.
    :param path: File path inside the sandbox to read.
    """
    client = get_client()
    raw = client.get_sandbox(sandbox_id).files.read(path)
    return bytes_for_transport(raw)


def file_write(sandbox_id: str, path: str, content: str, encoding: str = "utf-8") -> dict[str, Any]:
    """Create or overwrite a file inside a sandbox.

    :param sandbox_id: The sandbox's id.
    :param path: File path inside the sandbox to write.
    :param content: The file content — plain text if encoding is "utf-8",
        base64-encoded bytes if encoding is "base64".
    :param encoding: Either "utf-8" (default, for text files) or "base64" (for binary files).
    """
    client = get_client()
    payload = bytes_from_transport(encoding, content)
    client.get_sandbox(sandbox_id).files.write(path, payload)
    return {"sandbox_id": sandbox_id, "path": path, "bytes": len(payload)}
