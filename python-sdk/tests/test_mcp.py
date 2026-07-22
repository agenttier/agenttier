# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Tests for the MCP tool layer (FR7, stretch goal).

Exercises the tool functions through the actual FastMCP ``call_tool`` path
(not just as plain Python calls) so the wiring between :mod:`agenttier.mcp.tools`
and the transport-facing ``mcp`` package is genuinely covered, per NFR8.
Uses pytest-httpx to stub Router responses the same way the rest of the SDK
test suite does.

Skips the whole module if the optional ``mcp`` dependency isn't installed
(``pip install agenttier[mcp]``) — this is the stretch goal's own
``[skip-verify] if MCP dep unavailable`` escape hatch from tasks.md, made
executable rather than just documented.
"""

from __future__ import annotations

from typing import Any, Iterator

import pytest

mcp = pytest.importorskip("mcp", reason="optional [mcp] extra not installed")

from pytest_httpx import HTTPXMock  # noqa: E402

import agenttier.mcp._client as mcp_client_module  # noqa: E402
from agenttier.mcp.server import build_server  # noqa: E402
from agenttier.mcp.tools import ALL_TOOLS  # noqa: E402

API_URL = "http://router.test"
BASE = f"{API_URL}/api/v1"

_SANDBOX_RESP = {
    "sandboxId": "sb1",
    "name": "sb1",
    "namespace": "default",
    "status": "Running",
}


def _structured(result: object) -> Any:
    """Extract the structured (JSON-able) payload from a FastMCP ``call_tool``
    result.

    At runtime, ``call_tool`` returns a ``(content_blocks, structured_dict)``
    tuple when the tool's return type isn't itself a plain content block —
    every tool here returns a dict/list, so it's always the tuple form. The
    declared return type (``Sequence[ContentBlock] | dict``) doesn't capture
    that tuple shape, so this helper isolates the ``# type: ignore`` to one
    place instead of scattering ``isinstance`` checks through every test.
    """
    return result[1]  # type: ignore[index]


@pytest.fixture(autouse=True)
def _reset_client_singleton(monkeypatch: pytest.MonkeyPatch) -> Iterator[None]:
    """Every test gets a fresh module-level client bound to the stubbed API_URL.

    ``get_client()`` caches a singleton so tool calls don't rebuild an
    httpx.Client per call; tests must reset it or state leaks across tests.
    """
    monkeypatch.setenv("AGENTTIER_API_URL", API_URL)
    monkeypatch.delenv("AGENTTIER_API_KEY", raising=False)
    monkeypatch.delenv("AGENTTIER_TOKEN", raising=False)
    mcp_client_module._client = None
    yield
    mcp_client_module._client = None


def _register_get_sandbox(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(method="GET", url=f"{BASE}/sandboxes/sb1", json=_SANDBOX_RESP)


# --- tool registry -----------------------------------------------------------


def test_all_tools_are_registered_on_the_server() -> None:
    """The tool set the server exposes must exactly match ALL_TOOLS (FR7.1:
    tool set and SDK method set never drift independently)."""
    import asyncio

    server = build_server()
    registered = asyncio.run(server.list_tools())
    registered_names = {t.name for t in registered}
    assert registered_names == set(ALL_TOOLS.keys())
    assert len(registered_names) > 0


def test_every_tool_has_a_docstring_description() -> None:
    """FR7.3: tool descriptions must be written for LLM consumption — at minimum,
    every tool needs a non-empty description (its docstring)."""
    import asyncio

    server = build_server()
    registered = asyncio.run(server.list_tools())
    for tool in registered:
        assert tool.description, f"tool {tool.name!r} has no description"


# --- error surfacing (FR7.4 edge case) ---------------------------------------


def test_missing_required_argument_raises_clean_tool_error() -> None:
    """A malformed/missing required-arg tool call must surface as a clear MCP
    tool error, not a raw stack trace (edge case in requirements.md)."""
    import asyncio

    from mcp.server.fastmcp.exceptions import ToolError

    server = build_server()
    with pytest.raises(ToolError):
        asyncio.run(server.call_tool("sandbox_create", {}))


def test_sdk_validation_error_raises_clean_tool_error(monkeypatch: pytest.MonkeyPatch) -> None:
    """An SDK-level guard-clause error (not just a missing-arg schema error)
    must also surface as a ToolError, per FR7.4."""
    import asyncio

    from mcp.server.fastmcp.exceptions import ToolError

    monkeypatch.delenv("AGENTTIER_API_URL", raising=False)
    mcp_client_module._client = None

    server = build_server()
    with pytest.raises(ToolError, match="AGENTTIER_API_URL"):
        asyncio.run(server.call_tool("sandbox_status", {"sandbox_id": "sb1"}))


# --- representative tool calls, end-to-end through call_tool ----------------


@pytest.mark.asyncio
async def test_sandbox_create_tool(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes",
        json={"sandboxId": "sb1", "name": "sb1", "namespace": "default"},
    )
    server = build_server()
    result = await server.call_tool(
        "sandbox_create", {"template": "general-coding", "name": "sb1"}
    )
    payload = _structured(result)
    assert payload["sandbox_id"] == "sb1"


@pytest.mark.asyncio
async def test_sandbox_status_tool(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(method="GET", url=f"{BASE}/sandboxes/sb1", json=_SANDBOX_RESP)
    server = build_server()
    result = await server.call_tool("sandbox_status", {"sandbox_id": "sb1"})
    payload = _structured(result)
    assert payload["status"] == "Running"


@pytest.mark.asyncio
async def test_sandbox_run_command_tool(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/sb1/exec",
        json={"stdout": "hello\n", "stderr": "", "exitCode": 0},
    )
    server = build_server()
    result = await server.call_tool(
        "sandbox_run_command", {"sandbox_id": "sb1", "command": "echo hello"}
    )
    payload = _structured(result)
    assert payload["stdout"] == "hello\n"
    assert payload["exit_code"] == 0


@pytest.mark.asyncio
async def test_file_read_tool_returns_utf8_text(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="GET", url=f"{BASE}/sandboxes/sb1/files/workspace/hello.txt", content=b"hello"
    )
    server = build_server()
    result = await server.call_tool(
        "file_read", {"sandbox_id": "sb1", "path": "/workspace/hello.txt"}
    )
    payload = _structured(result)
    assert payload["encoding"] == "utf-8"
    assert payload["content"] == "hello"


@pytest.mark.asyncio
async def test_file_write_tool(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="PUT",
        url=f"{BASE}/sandboxes/sb1/files/workspace/hello.txt",
        status_code=201,
        json={"path": "/workspace/hello.txt", "bytes": 5},
    )
    server = build_server()
    result = await server.call_tool(
        "file_write",
        {"sandbox_id": "sb1", "path": "/workspace/hello.txt", "content": "hello"},
    )
    payload = _structured(result)
    assert payload["bytes"] == 5


@pytest.mark.asyncio
async def test_governance_effective_policy_tool(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/governance/effective",
        json={"namespace": "default", "policy": {"maxSandboxesTotal": 10}},
    )
    server = build_server()
    result = await server.call_tool("governance_effective_policy", {})
    payload = _structured(result)
    assert payload["namespace"] == "default"
    assert payload["policy"]["max_sandboxes_total"] == 10


@pytest.mark.asyncio
async def test_admin_tool_authorization_error_surfaces_as_tool_error(
    httpx_mock: HTTPXMock,
) -> None:
    """A 403 from an admin-only endpoint (e.g. a non-admin credential) must
    surface as a clean ToolError, not an unhandled AuthorizationError."""
    from mcp.server.fastmcp.exceptions import ToolError

    httpx_mock.add_response(
        method="GET", url=f"{BASE}/admin/sandboxes", status_code=403, json={"error": "forbidden"}
    )
    server = build_server()
    with pytest.raises(ToolError):
        await server.call_tool("admin_list_sandboxes", {})


@pytest.mark.asyncio
async def test_webhook_create_tool(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/webhooks",
        json={
            "id": "wh1",
            "url": "https://example.com/hook",
            "eventTypes": ["sandbox.running"],
            "secret": "shown-once",
        },
    )
    server = build_server()
    result = await server.call_tool(
        "webhook_create",
        {"url": "https://example.com/hook", "event_types": ["sandbox.running"]},
    )
    payload = _structured(result)
    assert payload["secret"] == "shown-once"


@pytest.mark.asyncio
async def test_sandbox_create_bulk_tool(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/bulk",
        json={"results": [{"index": 0, "status": "created", "sandboxId": "sb1"}]},
    )
    server = build_server()
    result = await server.call_tool(
        "sandbox_create_bulk",
        {"items": [{"template": "general-coding", "name": "sb1"}]},
    )
    payload = _structured(result)
    # FastMCP wraps list-returning tools' structured output as {"result": [...]}.
    assert payload["result"][0]["sandbox_id"] == "sb1"
