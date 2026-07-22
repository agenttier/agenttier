# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Sync sandbox handle.

Use :meth:`AgentTierClient.create_sandbox` / :meth:`AgentTierClient.get_sandbox`
to obtain instances — don't construct :class:`Sandbox` directly.
"""

from __future__ import annotations

import time
from typing import TYPE_CHECKING, Any, Dict, Mapping, Optional

from agenttier._http import raise_for_status
from agenttier.bulk import PatchResult, patch_sandbox
from agenttier.exceptions import SandboxErrorState, SandboxTimeoutError
from agenttier.models import CommandResult, FileEntry, ForwardedPort, SandboxPhase, SandboxSummary

if TYPE_CHECKING:  # pragma: no cover
    import httpx

    from agenttier.agent import AgentAPI
    from agenttier.backups import BackupsAPI
    from agenttier.sharing import SharingAPI

_DEFAULT_WAIT_TIMEOUT = 120.0
_DEFAULT_POLL_INTERVAL = 2.0


class Sandbox:
    """Remote handle for a sandbox running in an AgentTier cluster."""

    def __init__(
        self,
        http: "httpx.Client",
        sandbox_id: str,
        name: str,
        namespace: str,
    ) -> None:
        self._http = http
        self.id = sandbox_id
        self.name = name
        self.namespace = namespace

    # ------- state -------------------------------------------------------

    def status(self) -> SandboxSummary:
        """Fetch the latest status from the server."""
        resp = self._http.get(f"/sandboxes/{self.id}")
        raise_for_status(resp)
        return SandboxSummary.model_validate(resp.json())

    @property
    def phase(self) -> SandboxPhase:
        """Shortcut returning the typed phase of the current status."""
        return self.status().phase

    def wait_until_running(
        self,
        timeout: float = _DEFAULT_WAIT_TIMEOUT,
        poll_interval: float = _DEFAULT_POLL_INTERVAL,
    ) -> SandboxSummary:
        """Block until the sandbox reaches ``Running``.

        Returns the final :class:`SandboxSummary` on success.

        Raises :class:`SandboxTimeoutError` on timeout and
        :class:`SandboxErrorState` if the sandbox transitions to Error.
        """
        deadline = time.monotonic() + timeout
        last: Optional[SandboxSummary] = None
        while time.monotonic() < deadline:
            last = self.status()
            if last.phase is SandboxPhase.RUNNING:
                return last
            if last.phase is SandboxPhase.ERROR:
                raise SandboxErrorState(last.message or f"sandbox {self.id} entered Error state")
            time.sleep(poll_interval)
        raise SandboxTimeoutError(
            f"sandbox {self.id} did not reach Running within {timeout:.0f}s "
            f"(last phase: {last.phase.value if last else 'unknown'})"
        )

    # ------- lifecycle ---------------------------------------------------

    def stop(self) -> None:
        """Delete the sandbox Pod while preserving the PVC."""
        resp = self._http.post(f"/sandboxes/{self.id}/stop")
        raise_for_status(resp)

    def resume(self) -> None:
        """Re-create the Pod for a stopped sandbox; re-uses the same PVC."""
        resp = self._http.post(f"/sandboxes/{self.id}/resume")
        raise_for_status(resp)

    def terminate(self) -> None:
        """Permanently delete the sandbox and its workspace."""
        resp = self._http.delete(f"/sandboxes/{self.id}")
        raise_for_status(resp)

    # Alias kept for consistency with the REST name.
    delete = terminate

    def clone(
        self,
        *,
        name: Optional[str] = None,
        snapshot_class: Optional[str] = None,
    ) -> "Sandbox":
        """Clone this sandbox via VolumeSnapshot.

        Returns a new ``Sandbox`` proxy for the cloned sandbox in ``Pending``
        state. The clone enters ``Creating`` while the CSI driver hydrates
        the PVC from the snapshot (typically 30-90s on EBS); poll
        :meth:`refresh` or use :meth:`AgentTierClient.wait_until_running`
        until the phase reaches ``Running``.

        Parameters
        ----------
        name:
            Name for the new sandbox. Auto-generated as
            ``<source>-clone-<unix-ts>`` when omitted. Must satisfy RFC 1123
            label rules (lowercase alphanumeric or ``-``, max 63 chars).
        snapshot_class:
            Override for the cluster's default ``VolumeSnapshotClass``. Most
            callers leave this empty.

        Cluster prerequisites: see ``docs/docs/cloning.md``. The cluster
        must have the external-snapshotter CRDs + controller installed and
        a default ``VolumeSnapshotClass`` configured for the source PVC's
        storage driver. Without those, the request returns 500.
        """
        body: Dict[str, str] = {}
        if name is not None:
            body["name"] = name
        if snapshot_class is not None:
            body["snapshotClass"] = snapshot_class
        resp = self._http.post(f"/sandboxes/{self.id}/clone", json=body)
        raise_for_status(resp)
        payload = resp.json()
        # Build a Sandbox proxy for the new id. The server returns 202
        # Accepted with the snapshot + clone metadata; we only need the
        # name to bind the proxy.
        return Sandbox(
            http=self._http,
            sandbox_id=payload["name"],
            name=payload["name"],
            namespace=payload.get("namespace", self.namespace),
        )

    # ------- execution ---------------------------------------------------

    def exec(self, command: str, timeout: int = 30) -> CommandResult:
        """Run a shell command inside the sandbox and wait for the result.

        ``timeout`` is applied both server-side (to the exec) and on the HTTP
        call (with a small buffer for overhead).
        """
        if not command:
            raise ValueError("command must be a non-empty string")
        if timeout <= 0:
            raise ValueError("timeout must be > 0")
        # Give the HTTP call a small buffer over the server-side exec timeout
        # so the server error bubbles up instead of httpx cutting us off.
        resp = self._http.post(
            f"/sandboxes/{self.id}/exec",
            json={"command": command, "timeout": timeout},
            timeout=timeout + 10,
        )
        raise_for_status(resp)
        return CommandResult.model_validate(resp.json())

    # ------- port forwarding --------------------------------------------

    def list_ports(self) -> list[ForwardedPort]:
        """Return the ports currently forwarded from this sandbox."""
        resp = self._http.get(f"/sandboxes/{self.id}/ports")
        raise_for_status(resp)
        ports = resp.json().get("ports") or []
        return [ForwardedPort.model_validate(p) for p in ports]

    def forward_port(self, port: int, protocol: str = "http") -> ForwardedPort:
        """Expose a container port via a ClusterIP Service (and Ingress if configured)."""
        if not 1 <= port <= 65535:
            raise ValueError("port must be between 1 and 65535")
        resp = self._http.post(
            f"/sandboxes/{self.id}/ports",
            json={"port": port, "protocol": protocol},
        )
        raise_for_status(resp)
        return ForwardedPort.model_validate(resp.json())

    def remove_port(self, port: int) -> None:
        """Tear down a previously-forwarded port."""
        resp = self._http.delete(f"/sandboxes/{self.id}/ports/{port}")
        raise_for_status(resp)

    # ------- files -------------------------------------------------------

    @property
    def files(self) -> "FilesAPI":
        """Namespace for the file-transfer REST surface.

        Returns a small facade so users call ``sandbox.files.list("/workspace")``
        instead of polluting the top-level API. The facade re-uses the parent
        sandbox's ``_http`` client, so it picks up auth, base URL, and
        timeouts automatically.
        """
        return FilesAPI(self)

    # ------- agent mode --------------------------------------------------

    @property
    def agent(self) -> "AgentAPI":
        """Namespace for the agent-mode REST surface.

        Returns a facade for ``/configure``, ``/invoke``, and
        ``/invoke/cancel``. Only valid when the sandbox was created with
        ``mode: agent`` — code-mode sandboxes get a 400 from the Router.
        """
        # Local import keeps a code-mode-only Sandbox from importing the SSE
        # parsing machinery on construction.
        from agenttier.agent import AgentAPI

        return AgentAPI(self)

    # ------- sharing -------------------------------------------------------

    @property
    def sharing(self) -> "SharingAPI":
        """Namespace for the sandbox sharing REST surface.

        Returns a facade for ``GET/POST /share``, ``DELETE /share/{userId}``,
        and ``POST /share-links``. See :class:`agenttier.sharing.SharingAPI`.
        """
        from agenttier.sharing import SharingAPI

        return SharingAPI(self)

    # ------- backups ---------------------------------------------------------

    @property
    def backups(self) -> "BackupsAPI":
        """Namespace for the sandbox backup/restore REST surface.

        Returns a facade for ``GET/POST /backups``,
        ``POST /backups/{snapshotName}/restore``, and
        ``DELETE /backups/{snapshotName}``. See
        :class:`agenttier.backups.BackupsAPI`.
        """
        from agenttier.backups import BackupsAPI

        return BackupsAPI(self)

    # ------- live mutation -------------------------------------------------

    def update(
        self,
        *,
        idle_timeout: Optional[str] = None,
        resources: Optional[Mapping[str, Any]] = None,
        labels: Optional[Mapping[str, str]] = None,
        annotations: Optional[Mapping[str, str]] = None,
    ) -> PatchResult:
        """Live-mutate this sandbox via ``PATCH /sandboxes/{id}``.

        At least one of ``idle_timeout``, ``resources``, ``labels``,
        ``annotations`` must be provided — omitting all four raises
        :class:`ValueError` locally before any network call (FR1.10).
        ``idle_timeout``/``labels``/``annotations`` changes apply
        immediately; ``resources`` changes are persisted but only take
        effect after the sandbox is stopped and resumed (the controller has
        no in-place pod resize) — check the returned
        :class:`~agenttier.bulk.PatchResult.restart_required` flag.

        A PATCH that would exceed the effective governance policy (e.g.
        ``maxCpu``) raises :class:`agenttier.exceptions.PolicyViolationError`,
        the same as ``create_sandbox``.
        """
        return patch_sandbox(
            self._http,
            self.id,
            idle_timeout=idle_timeout,
            resources=resources,
            labels=labels,
            annotations=annotations,
        )

    # ------- misc --------------------------------------------------------

    def __repr__(self) -> str:
        return f"Sandbox(id={self.id!r}, name={self.name!r}, namespace={self.namespace!r})"


class FilesAPI:
    """Sync wrapper around the `/sandboxes/{id}/files/*` REST endpoints.

    The router drives sandbox-side ``ls``/``stat``/``base64`` through the SPDY
    exec bridge, so there's a 32 MiB per-request ceiling enforced on the server
    side. Very large files should be moved with git, rsync over exec, or a
    PVC snapshot instead.
    """

    #: Mirrors the server-side cap in ``pkg/router/handlers.go``. Uploads
    #: larger than this are rejected with a 413.
    MAX_BYTES: int = 32 * 1024 * 1024

    def __init__(self, sandbox: "Sandbox") -> None:
        self._sandbox = sandbox
        self._http = sandbox._http

    def list(self, path: str = "/workspace") -> list[FileEntry]:
        """List a directory inside the sandbox.

        Defaults to ``/workspace`` since that's the template-provided PVC mount.
        Raises :class:`NotFoundError` when the path doesn't exist.
        """
        if not path:
            raise ValueError("path must be a non-empty string")
        resp = self._http.get(
            f"/sandboxes/{self._sandbox.id}/files/",
            params={"path": path},
        )
        raise_for_status(resp)
        body = resp.json() or {}
        entries = body.get("entries") or []
        return [FileEntry.model_validate(e) for e in entries]

    def read(self, path: str) -> bytes:
        """Download a file and return its bytes.

        Use :meth:`download` when you already have an open path on disk; this
        method is a convenience for small files that fit in memory.
        """
        stripped = path.lstrip("/")
        if not stripped:
            raise ValueError("path must include a file name")
        resp = self._http.get(f"/sandboxes/{self._sandbox.id}/files/{stripped}")
        raise_for_status(resp)
        return resp.content

    def download(self, path: str, destination: str) -> int:
        """Stream a file to ``destination`` (a local filesystem path).

        Returns the number of bytes written. Streaming keeps memory bounded on
        large-ish files up to the server-side 32 MiB cap.
        """
        stripped = path.lstrip("/")
        if not stripped:
            raise ValueError("path must include a file name")
        written = 0
        with self._http.stream("GET", f"/sandboxes/{self._sandbox.id}/files/{stripped}") as resp:
            raise_for_status(resp)
            with open(destination, "wb") as fh:
                for chunk in resp.iter_bytes():
                    if chunk:
                        fh.write(chunk)
                        written += len(chunk)
        return written

    def write(self, path: str, data: bytes | str) -> None:
        """Create or overwrite a file from in-memory bytes or a string."""
        if isinstance(data, str):
            payload = data.encode("utf-8")
        else:
            payload = bytes(data)
        self._put_bytes(path, payload)

    def upload(self, path: str, source: str) -> int:
        """Upload a local file at ``source`` to ``path`` inside the sandbox.

        Returns the number of bytes uploaded. Raises :class:`ValueError` when
        the local file exceeds :attr:`MAX_BYTES` to avoid a round-trip only to
        get a 413 back.
        """
        with open(source, "rb") as fh:
            payload = fh.read()
        if len(payload) > self.MAX_BYTES:
            raise ValueError(
                f"{source} is {len(payload)} bytes, max {self.MAX_BYTES} per upload"
            )
        self._put_bytes(path, payload)
        return len(payload)

    def archive(self, destination: str, path: str = "/workspace") -> int:
        """Download a directory tree as a streamed ``.zip`` to ``destination``.

        Mirrors the Web UI's "Download workspace as zip" button. The Router
        builds the zip on the fly by piping ``tar -cf - -C <path> .`` through
        ``archive/zip`` server-side, so this works on every sandbox image
        (no in-pod ``zip`` binary needed). The endpoint is locked to the
        ``/workspace`` subtree.

        Returns the number of compressed bytes written. Streaming keeps memory
        bounded for large workspaces — the file size on disk is the only real
        constraint (default soft cap is 5 GiB; larger archives are truncated
        with a logged warning on the Router).
        """
        if not path:
            raise ValueError("path must be a non-empty string")
        written = 0
        with self._http.stream(
            "GET",
            f"/sandboxes/{self._sandbox.id}/archive",
            params={"path": path},
        ) as resp:
            raise_for_status(resp)
            with open(destination, "wb") as fh:
                for chunk in resp.iter_bytes():
                    if chunk:
                        fh.write(chunk)
                        written += len(chunk)
        return written

    def _put_bytes(self, path: str, payload: bytes) -> None:
        stripped = path.lstrip("/")
        if not stripped:
            raise ValueError("path must include a file name")
        resp = self._http.put(
            f"/sandboxes/{self._sandbox.id}/files/{stripped}",
            content=payload,
            headers={"Content-Type": "application/octet-stream"},
        )
        raise_for_status(resp)
