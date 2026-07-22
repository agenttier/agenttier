# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Async sandbox handle mirroring :mod:`agenttier.sandbox`."""

from __future__ import annotations

import asyncio
from typing import TYPE_CHECKING, Any, Dict, Mapping, Optional

from agenttier._http import raise_for_status
from agenttier.bulk import PatchResult, async_patch_sandbox
from agenttier.exceptions import SandboxErrorState, SandboxTimeoutError
from agenttier.models import CommandResult, FileEntry, ForwardedPort, SandboxPhase, SandboxSummary

if TYPE_CHECKING:  # pragma: no cover
    import httpx

    from agenttier.async_agent import AsyncAgentAPI
    from agenttier.backups import AsyncBackupsAPI
    from agenttier.sharing import AsyncSharingAPI

_DEFAULT_WAIT_TIMEOUT = 120.0
_DEFAULT_POLL_INTERVAL = 2.0


class AsyncSandbox:
    """Async remote handle for a sandbox."""

    def __init__(
        self,
        http: "httpx.AsyncClient",
        sandbox_id: str,
        name: str,
        namespace: str,
    ) -> None:
        self._http = http
        self.id = sandbox_id
        self.name = name
        self.namespace = namespace

    async def status(self) -> SandboxSummary:
        resp = await self._http.get(f"/sandboxes/{self.id}")
        raise_for_status(resp)
        return SandboxSummary.model_validate(resp.json())

    async def wait_until_running(
        self,
        timeout: float = _DEFAULT_WAIT_TIMEOUT,
        poll_interval: float = _DEFAULT_POLL_INTERVAL,
    ) -> SandboxSummary:
        loop = asyncio.get_event_loop()
        deadline = loop.time() + timeout
        last: Optional[SandboxSummary] = None
        while loop.time() < deadline:
            last = await self.status()
            if last.phase is SandboxPhase.RUNNING:
                return last
            if last.phase is SandboxPhase.ERROR:
                raise SandboxErrorState(last.message or f"sandbox {self.id} entered Error state")
            await asyncio.sleep(poll_interval)
        raise SandboxTimeoutError(
            f"sandbox {self.id} did not reach Running within {timeout:.0f}s "
            f"(last phase: {last.phase.value if last else 'unknown'})"
        )

    async def stop(self) -> None:
        resp = await self._http.post(f"/sandboxes/{self.id}/stop")
        raise_for_status(resp)

    async def resume(self) -> None:
        resp = await self._http.post(f"/sandboxes/{self.id}/resume")
        raise_for_status(resp)

    async def terminate(self) -> None:
        resp = await self._http.delete(f"/sandboxes/{self.id}")
        raise_for_status(resp)

    delete = terminate

    async def clone(
        self,
        *,
        name: Optional[str] = None,
        snapshot_class: Optional[str] = None,
    ) -> "AsyncSandbox":
        """Clone this sandbox via VolumeSnapshot. See ``Sandbox.clone``."""
        body: Dict[str, str] = {}
        if name is not None:
            body["name"] = name
        if snapshot_class is not None:
            body["snapshotClass"] = snapshot_class
        resp = await self._http.post(f"/sandboxes/{self.id}/clone", json=body)
        raise_for_status(resp)
        payload = resp.json()
        return AsyncSandbox(
            http=self._http,
            sandbox_id=payload["name"],
            name=payload["name"],
            namespace=payload.get("namespace", self.namespace),
        )

    async def exec(self, command: str, timeout: int = 30) -> CommandResult:
        if not command:
            raise ValueError("command must be a non-empty string")
        if timeout <= 0:
            raise ValueError("timeout must be > 0")
        resp = await self._http.post(
            f"/sandboxes/{self.id}/exec",
            json={"command": command, "timeout": timeout},
            timeout=timeout + 10,
        )
        raise_for_status(resp)
        return CommandResult.model_validate(resp.json())

    async def list_ports(self) -> list[ForwardedPort]:
        resp = await self._http.get(f"/sandboxes/{self.id}/ports")
        raise_for_status(resp)
        ports = resp.json().get("ports") or []
        return [ForwardedPort.model_validate(p) for p in ports]

    async def forward_port(self, port: int, protocol: str = "http") -> ForwardedPort:
        if not 1 <= port <= 65535:
            raise ValueError("port must be between 1 and 65535")
        resp = await self._http.post(
            f"/sandboxes/{self.id}/ports",
            json={"port": port, "protocol": protocol},
        )
        raise_for_status(resp)
        return ForwardedPort.model_validate(resp.json())

    async def remove_port(self, port: int) -> None:
        resp = await self._http.delete(f"/sandboxes/{self.id}/ports/{port}")
        raise_for_status(resp)

    @property
    def files(self) -> "AsyncFilesAPI":
        """Async mirror of :pyattr:`Sandbox.files`."""
        return AsyncFilesAPI(self)

    @property
    def agent(self) -> "AsyncAgentAPI":
        """Async mirror of :pyattr:`Sandbox.agent`."""
        from agenttier.async_agent import AsyncAgentAPI

        return AsyncAgentAPI(self)

    @property
    def sharing(self) -> "AsyncSharingAPI":
        """Async mirror of :pyattr:`agenttier.sandbox.Sandbox.sharing`."""
        from agenttier.sharing import AsyncSharingAPI

        return AsyncSharingAPI(self)

    @property
    def backups(self) -> "AsyncBackupsAPI":
        """Async mirror of :pyattr:`agenttier.sandbox.Sandbox.backups`."""
        from agenttier.backups import AsyncBackupsAPI

        return AsyncBackupsAPI(self)

    async def update(
        self,
        *,
        idle_timeout: Optional[str] = None,
        resources: Optional[Mapping[str, Any]] = None,
        labels: Optional[Mapping[str, str]] = None,
        annotations: Optional[Mapping[str, str]] = None,
    ) -> PatchResult:
        """Async mirror of :meth:`agenttier.sandbox.Sandbox.update`."""
        return await async_patch_sandbox(
            self._http,
            self.id,
            idle_timeout=idle_timeout,
            resources=resources,
            labels=labels,
            annotations=annotations,
        )

    def __repr__(self) -> str:
        return f"AsyncSandbox(id={self.id!r}, name={self.name!r}, namespace={self.namespace!r})"


class AsyncFilesAPI:
    """Async wrapper around ``/sandboxes/{id}/files/*`` REST endpoints.

    Protocol and size cap are identical to :class:`agenttier.sandbox.FilesAPI`
    — the two classes just differ in sync vs async. Read that docstring for the
    full contract.
    """

    MAX_BYTES: int = 32 * 1024 * 1024

    def __init__(self, sandbox: "AsyncSandbox") -> None:
        self._sandbox = sandbox
        self._http = sandbox._http

    async def list(self, path: str = "/workspace") -> list[FileEntry]:
        if not path:
            raise ValueError("path must be a non-empty string")
        resp = await self._http.get(
            f"/sandboxes/{self._sandbox.id}/files/",
            params={"path": path},
        )
        raise_for_status(resp)
        body = resp.json() or {}
        entries = body.get("entries") or []
        return [FileEntry.model_validate(e) for e in entries]

    async def read(self, path: str) -> bytes:
        stripped = path.lstrip("/")
        if not stripped:
            raise ValueError("path must include a file name")
        resp = await self._http.get(f"/sandboxes/{self._sandbox.id}/files/{stripped}")
        raise_for_status(resp)
        return resp.content

    async def download(self, path: str, destination: str) -> int:
        stripped = path.lstrip("/")
        if not stripped:
            raise ValueError("path must include a file name")
        written = 0
        async with self._http.stream("GET", f"/sandboxes/{self._sandbox.id}/files/{stripped}") as resp:
            raise_for_status(resp)
            with open(destination, "wb") as fh:
                async for chunk in resp.aiter_bytes():
                    if chunk:
                        fh.write(chunk)
                        written += len(chunk)
        return written

    async def write(self, path: str, data: bytes | str) -> None:
        if isinstance(data, str):
            payload = data.encode("utf-8")
        else:
            payload = bytes(data)
        await self._put_bytes(path, payload)

    async def upload(self, path: str, source: str) -> int:
        with open(source, "rb") as fh:
            payload = fh.read()
        if len(payload) > self.MAX_BYTES:
            raise ValueError(
                f"{source} is {len(payload)} bytes, max {self.MAX_BYTES} per upload"
            )
        await self._put_bytes(path, payload)
        return len(payload)

    async def archive(self, destination: str, path: str = "/workspace") -> int:
        """Async twin of :meth:`agenttier.sandbox.FilesAPI.archive`.

        Streams a ``.zip`` of the directory tree at ``path`` to ``destination``.
        See the sync docstring for the full contract.
        """
        if not path:
            raise ValueError("path must be a non-empty string")
        written = 0
        async with self._http.stream(
            "GET",
            f"/sandboxes/{self._sandbox.id}/archive",
            params={"path": path},
        ) as resp:
            raise_for_status(resp)
            with open(destination, "wb") as fh:
                async for chunk in resp.aiter_bytes():
                    if chunk:
                        fh.write(chunk)
                        written += len(chunk)
        return written

    async def _put_bytes(self, path: str, payload: bytes) -> None:
        stripped = path.lstrip("/")
        if not stripped:
            raise ValueError("path must include a file name")
        resp = await self._http.put(
            f"/sandboxes/{self._sandbox.id}/files/{stripped}",
            content=payload,
            headers={"Content-Type": "application/octet-stream"},
        )
        raise_for_status(resp)
