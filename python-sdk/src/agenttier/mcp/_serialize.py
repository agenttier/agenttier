# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""JSON-serialization helpers shared by every MCP tool function.

MCP tool results must be plain JSON-compatible values. The SDK returns
pydantic models (and occasionally raw ``bytes``); these helpers convert both
into the shapes MCP can transport, so individual tool functions stay one-line
delegations instead of repeating ``.model_dump(...)`` boilerplate everywhere.
"""

from __future__ import annotations

import base64
from typing import Any, Sequence

from pydantic import BaseModel


def dump(model: BaseModel) -> dict[str, Any]:
    """Serialize a single pydantic model to a JSON-compatible dict.

    Uses field names (not the camelCase wire aliases) since MCP tool output
    is consumed by an LLM/agent, not the Router — snake_case matches the
    SDK's own public attribute names.
    """
    return model.model_dump(mode="json", by_alias=False)


def dump_list(models: Sequence[BaseModel]) -> list[dict[str, Any]]:
    """Serialize a sequence of pydantic models. See :func:`dump`."""
    return [dump(m) for m in models]


def bytes_for_transport(raw: bytes) -> dict[str, Any]:
    """Encode raw file bytes for a JSON tool result.

    Tries UTF-8 text first (the common case for source/config files an agent
    reads) and falls back to base64 for anything that isn't valid UTF-8 —
    mirrors the same fallback :mod:`agenttier.agent` uses for configure()
    file uploads, so callers see a consistent convention across tools.
    """
    try:
        return {"encoding": "utf-8", "content": raw.decode("utf-8")}
    except UnicodeDecodeError:
        return {"encoding": "base64", "content": base64.b64encode(raw).decode("ascii")}


def bytes_from_transport(encoding: str, content: str) -> bytes:
    """Inverse of :func:`bytes_for_transport`, for tools that accept file content."""
    if encoding == "base64":
        return base64.b64decode(content)
    if encoding == "utf-8":
        return content.encode("utf-8")
    raise ValueError(f"encoding must be 'utf-8' or 'base64', got {encoding!r}")
