# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""MCP tools mirroring the sandbox-lifecycle surface of the Python SDK.

One tool per public method on :class:`agenttier.client.AgentTierClient`
(sandbox CRUD, templates, identity) and :class:`agenttier.sandbox.Sandbox`
(lifecycle, exec, ports, live update). Every tool re-resolves its
:class:`agenttier.sandbox.Sandbox` handle from ``sandbox_id`` via
``client.get_sandbox()`` on each call — MCP tool calls are stateless, so
there is no persistent handle to reuse between calls the way a script would.

Intentionally NOT mapped to a tool: ``Sandbox.wait_until_running()`` — it's a
client-side polling loop that can block far longer than a single MCP tool
call should; an agent driving AgentTier over MCP polls via repeated
``sandbox_status`` calls instead (FR7.4 permits cutting ergonomics that don't
fit the transport).
"""

from __future__ import annotations

from typing import Any, Mapping, Optional

from agenttier.mcp._client import get_client
from agenttier.mcp._serialize import dump, dump_list


def sandbox_create(
    template: str,
    name: str,
    namespace: str = "default",
    timeout: Optional[str] = None,
    idle_timeout: Optional[str] = None,
    storage_size: Optional[str] = None,
) -> dict[str, Any]:
    """Create a sandbox from a ClusterSandboxTemplate.

    :param template: Name of the ClusterSandboxTemplate to create from.
    :param name: Name for the new sandbox (must be a valid Kubernetes name).
    :param namespace: Kubernetes namespace to create the sandbox in.
    :param timeout: Maximum sandbox lifetime as a Go duration string (e.g. "8h").
    :param idle_timeout: Auto-stop-after-idle duration (e.g. "30m").
    :param storage_size: Workspace PVC size (e.g. "10Gi").
    :returns: The sandbox's id/name/namespace so the caller can act on it next.
    """
    client = get_client()
    sandbox = client.create_sandbox(
        template=template,
        name=name,
        namespace=namespace,
        timeout=timeout,
        idle_timeout=idle_timeout,
        storage_size=storage_size,
    )
    return {"sandbox_id": sandbox.id, "name": sandbox.name, "namespace": sandbox.namespace}


def sandbox_list(
    namespace: Optional[str] = None,
    status: Optional[str] = None,
) -> list[dict[str, Any]]:
    """List sandboxes visible to the caller, optionally filtered.

    :param namespace: Restrict results to this Kubernetes namespace.
    :param status: Restrict results to this phase (e.g. "Running", "Stopped").
    """
    client = get_client()
    return dump_list(client.list_sandboxes(namespace=namespace, status=status))


def sandbox_status(sandbox_id: str) -> dict[str, Any]:
    """Return the latest status of a sandbox (phase, pod/PVC names, timestamps).

    :param sandbox_id: The sandbox's id (as returned by sandbox_create/sandbox_list).
    """
    client = get_client()
    return dump(client.get_sandbox(sandbox_id).status())


def sandbox_stop(sandbox_id: str) -> dict[str, str]:
    """Stop a sandbox: deletes its Pod but preserves the workspace PVC.

    :param sandbox_id: The sandbox's id.
    """
    client = get_client()
    client.get_sandbox(sandbox_id).stop()
    return {"sandbox_id": sandbox_id, "status": "stopped"}


def sandbox_resume(sandbox_id: str) -> dict[str, str]:
    """Resume a stopped sandbox: re-creates its Pod re-using the same workspace PVC.

    :param sandbox_id: The sandbox's id.
    """
    client = get_client()
    client.get_sandbox(sandbox_id).resume()
    return {"sandbox_id": sandbox_id, "status": "resuming"}


def sandbox_terminate(sandbox_id: str) -> dict[str, str]:
    """Permanently delete a sandbox and its workspace. This cannot be undone.

    :param sandbox_id: The sandbox's id.
    """
    client = get_client()
    client.get_sandbox(sandbox_id).terminate()
    return {"sandbox_id": sandbox_id, "status": "terminated"}


def sandbox_clone(
    sandbox_id: str,
    name: Optional[str] = None,
    snapshot_class: Optional[str] = None,
) -> dict[str, Any]:
    """Clone a sandbox via VolumeSnapshot into a new sandbox in Pending state.

    :param sandbox_id: The source sandbox's id.
    :param name: Name for the new sandbox. Auto-generated if omitted.
    :param snapshot_class: Override for the cluster's default VolumeSnapshotClass.
    """
    client = get_client()
    clone = client.get_sandbox(sandbox_id).clone(name=name, snapshot_class=snapshot_class)
    return {"sandbox_id": clone.id, "name": clone.name, "namespace": clone.namespace}


def sandbox_run_command(sandbox_id: str, command: str, timeout: int = 30) -> dict[str, Any]:
    """Run a non-interactive shell command inside a sandbox and wait for it to finish.

    :param sandbox_id: The sandbox's id.
    :param command: Shell command to run inside the sandbox.
    :param timeout: Maximum seconds to wait for the command to complete.
    """
    client = get_client()
    handle = client.get_sandbox(sandbox_id)
    result = handle.exec(command, timeout=timeout)
    return dump(result)


def sandbox_list_ports(sandbox_id: str) -> list[dict[str, Any]]:
    """List ports currently forwarded from a sandbox.

    :param sandbox_id: The sandbox's id.
    """
    client = get_client()
    return dump_list(client.get_sandbox(sandbox_id).list_ports())


def sandbox_forward_port(sandbox_id: str, port: int, protocol: str = "http") -> dict[str, Any]:
    """Expose a container port from a sandbox via a ClusterIP Service (+ Ingress if configured).

    :param sandbox_id: The sandbox's id.
    :param port: Container port to expose, 1-65535.
    :param protocol: Either "http" or "tcp".
    """
    client = get_client()
    return dump(client.get_sandbox(sandbox_id).forward_port(port, protocol=protocol))


def sandbox_remove_port(sandbox_id: str, port: int) -> dict[str, Any]:
    """Tear down a previously-forwarded port.

    :param sandbox_id: The sandbox's id.
    :param port: The container port to stop forwarding.
    """
    client = get_client()
    client.get_sandbox(sandbox_id).remove_port(port)
    return {"sandbox_id": sandbox_id, "port": port, "status": "removed"}


def sandbox_update(
    sandbox_id: str,
    idle_timeout: Optional[str] = None,
    resources: Optional[Mapping[str, Any]] = None,
    labels: Optional[Mapping[str, str]] = None,
    annotations: Optional[Mapping[str, str]] = None,
) -> dict[str, Any]:
    """Live-mutate a running sandbox's idle timeout, resources, labels, or annotations.

    At least one field must be provided. idle_timeout/labels/annotations
    changes take effect immediately; resources changes are persisted but
    only take effect after the sandbox is next stopped and resumed (check
    the returned restart_required flag).

    :param sandbox_id: The sandbox's id.
    :param idle_timeout: New auto-stop-after-idle duration (e.g. "1h").
    :param resources: CPU/memory requests+limits, e.g.
        ``{"requests": {"cpu": "1", "memory": "2Gi"}, "limits": {"cpu": "2", "memory": "4Gi"}}``.
    :param labels: Kubernetes labels to merge onto the sandbox.
    :param annotations: Kubernetes annotations to merge onto the sandbox.
    """
    client = get_client()
    result = client.get_sandbox(sandbox_id).update(
        idle_timeout=idle_timeout, resources=resources, labels=labels, annotations=annotations
    )
    return dump(result)


def template_list() -> list[dict[str, Any]]:
    """List available ClusterSandboxTemplates."""
    client = get_client()
    return dump_list(client.list_templates())


def template_get(name: str) -> dict[str, Any]:
    """Return one ClusterSandboxTemplate's details.

    :param name: The template's name.
    """
    client = get_client()
    return dump(client.get_template(name))


def current_user() -> dict[str, Any]:
    """Return the server's view of the caller's own identity (useful for verifying auth)."""
    client = get_client()
    return dump(client.current_user())
