# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Sync + async wrappers around bulk sandbox operations and live PATCH.

Wraps ``POST /sandboxes/bulk`` (batched create), ``POST /sandboxes/bulk-action``
(batched stop/resume/delete), and ``PATCH /sandboxes/{id}`` (live mutation of
idle timeout / resources / labels / annotations).

The bulk operations are exposed as ``client.sandboxes`` (see
:class:`SandboxesAPI` / :class:`AsyncSandboxesAPI`), following the same
"own module, attached by the hub-wiring task" pattern as
``governance.py``/``warmpool.py``.

The PATCH helpers (:func:`build_patch_body`, :func:`patch_sandbox`,
:func:`async_patch_sandbox`) are deliberately **not** methods on
:class:`agenttier.sandbox.Sandbox` — that class is owned by the hub-wiring
task (which also instantiates ``SharingAPI``/``BackupsAPI`` on
``Sandbox.__init__``). These stay as plain functions taking an ``httpx``
client + sandbox id so ``Sandbox.update()`` / ``AsyncSandbox.update()`` can be
thin wrappers that delegate here without this module needing to edit
``sandbox.py``/``async_sandbox.py`` itself.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Any, Mapping, Optional, Sequence

from pydantic import Field

from agenttier._http import raise_for_status
from agenttier.models import _Model

if TYPE_CHECKING:  # pragma: no cover
    import httpx

    from agenttier.async_client import AsyncAgentTierClient
    from agenttier.client import AgentTierClient

_VALID_BULK_ACTIONS = ("stop", "resume", "delete")


# --- models ----------------------------------------------------------------


class BulkCreateItem(_Model):
    """One sandbox spec for :meth:`SandboxesAPI.create_bulk`.

    Mirrors the same parameters as ``AgentTierClient.create_sandbox`` so a
    caller can turn a loop of single creates into one batched call.
    """

    template: str
    name: str
    namespace: str = "default"
    timeout: Optional[str] = None
    idle_timeout: Optional[str] = None
    storage_size: Optional[str] = None


class BulkCreateResultItem(_Model):
    """One entry from ``POST /sandboxes/bulk``'s per-item ``results`` array."""

    index: int
    status: str
    """``"created"`` or ``"error"``."""

    sandbox_id: Optional[str] = Field(default=None, alias="sandboxId")
    error: Optional[str] = None


class BulkActionResultItem(_Model):
    """One entry from ``POST /sandboxes/bulk-action``'s per-item ``results`` array."""

    id: str
    status: str
    error: Optional[str] = None


class PatchResult(_Model):
    """Response body from ``PATCH /sandboxes/{id}``.

    ``applied`` maps each requested field to ``"immediately"`` or
    ``"on-restart"`` — ``idleTimeout``/``labels``/``annotations`` changes take
    effect live, ``resources`` changes require the sandbox to be stopped and
    resumed (the controller has no in-place pod resize).
    """

    sandbox_id: str = Field(alias="sandboxId")
    applied: dict[str, str] = Field(default_factory=dict)
    restart_required: bool = Field(default=False, alias="restartRequired")
    message: Optional[str] = None


# --- validation --------------------------------------------------------------


def _item_to_body(item: BulkCreateItem) -> dict[str, Any]:
    body: dict[str, Any] = {
        "name": item.name,
        "namespace": item.namespace,
        "templateRef": {"name": item.template, "kind": "ClusterSandboxTemplate"},
    }
    if item.timeout:
        body["timeout"] = item.timeout
    if item.idle_timeout:
        body["idleTimeout"] = item.idle_timeout
    if item.storage_size:
        body["storage"] = {"size": item.storage_size}
    return body


def _validate_bulk_create_items(items: Sequence[BulkCreateItem]) -> None:
    if not items:
        raise ValueError("items must be a non-empty sequence of BulkCreateItem")
    for i, item in enumerate(items):
        if not item.template:
            raise ValueError(f"items[{i}].template must be a non-empty string")
        if not item.name:
            raise ValueError(f"items[{i}].name must be a non-empty string")


def _validate_bulk_action_args(ids: Sequence[str], action: str) -> None:
    if not ids:
        raise ValueError("ids must be a non-empty sequence of sandbox IDs")
    if action not in _VALID_BULK_ACTIONS:
        raise ValueError(f"action must be one of {_VALID_BULK_ACTIONS}, got {action!r}")
    for i, sid in enumerate(ids):
        if not sid:
            raise ValueError(f"ids[{i}] must be a non-empty string")


def _validate_patch_args(
    idle_timeout: Optional[str],
    resources: Optional[Mapping[str, Any]],
    labels: Optional[Mapping[str, str]],
    annotations: Optional[Mapping[str, str]],
) -> None:
    if idle_timeout is None and resources is None and labels is None and annotations is None:
        raise ValueError(
            "at least one of idle_timeout, resources, labels, annotations must be provided"
        )


def build_patch_body(
    *,
    idle_timeout: Optional[str] = None,
    resources: Optional[Mapping[str, Any]] = None,
    labels: Optional[Mapping[str, str]] = None,
    annotations: Optional[Mapping[str, str]] = None,
) -> dict[str, Any]:
    """Build the JSON body for ``PATCH /sandboxes/{id}``, validating first.

    Exposed (not underscore-prefixed) so ``Sandbox.update()`` /
    ``AsyncSandbox.update()`` can reuse the guard-clause + body-shape logic
    without duplicating it.
    """
    _validate_patch_args(idle_timeout, resources, labels, annotations)
    body: dict[str, Any] = {}
    if idle_timeout is not None:
        body["idleTimeout"] = idle_timeout
    if resources is not None:
        body["resources"] = dict(resources)
    if labels is not None:
        body["labels"] = dict(labels)
    if annotations is not None:
        body["annotations"] = dict(annotations)
    return body


# --- PATCH (sync + async) ---------------------------------------------------


def patch_sandbox(
    http: "httpx.Client",
    sandbox_id: str,
    *,
    idle_timeout: Optional[str] = None,
    resources: Optional[Mapping[str, Any]] = None,
    labels: Optional[Mapping[str, str]] = None,
    annotations: Optional[Mapping[str, str]] = None,
) -> PatchResult:
    """Sync ``PATCH /sandboxes/{id}``. Backs ``Sandbox.update()``."""
    body = build_patch_body(
        idle_timeout=idle_timeout, resources=resources, labels=labels, annotations=annotations
    )
    resp = http.patch(f"/sandboxes/{sandbox_id}", json=body)
    raise_for_status(resp)
    return PatchResult.model_validate(resp.json())


async def async_patch_sandbox(
    http: "httpx.AsyncClient",
    sandbox_id: str,
    *,
    idle_timeout: Optional[str] = None,
    resources: Optional[Mapping[str, Any]] = None,
    labels: Optional[Mapping[str, str]] = None,
    annotations: Optional[Mapping[str, str]] = None,
) -> PatchResult:
    """Async ``PATCH /sandboxes/{id}``. Backs ``AsyncSandbox.update()``."""
    body = build_patch_body(
        idle_timeout=idle_timeout, resources=resources, labels=labels, annotations=annotations
    )
    resp = await http.patch(f"/sandboxes/{sandbox_id}", json=body)
    raise_for_status(resp)
    return PatchResult.model_validate(resp.json())


# --- bulk (sync + async) ----------------------------------------------------


class SandboxesAPI:
    """Sync wrapper for ``POST /sandboxes/bulk`` and ``/sandboxes/bulk-action``.

    Use ``client.sandboxes`` to obtain an instance; don't construct directly.
    """

    def __init__(self, client: "AgentTierClient") -> None:
        self._http: "httpx.Client" = client._http

    def create_bulk(self, items: Sequence[BulkCreateItem]) -> list[BulkCreateResultItem]:
        """Create multiple sandboxes in one call.

        A governance cap violation (e.g. exceeding ``maxSandboxesTotal``) fails
        the *entire* batch fail-fast — nothing is created — and surfaces as
        :class:`agenttier.exceptions.ConflictError`. Otherwise each item is
        independent: a bad template on one item doesn't block the others, and
        the per-item ``results`` array reports which succeeded/failed.
        """
        _validate_bulk_create_items(items)
        body = {"items": [_item_to_body(item) for item in items]}
        resp = self._http.post("/sandboxes/bulk", json=body)
        raise_for_status(resp)
        results = resp.json().get("results") or []
        return [BulkCreateResultItem.model_validate(r) for r in results]

    def bulk_action(self, ids: Sequence[str], action: str) -> list[BulkActionResultItem]:
        """Apply ``"stop"``, ``"resume"``, or ``"delete"`` to multiple sandboxes.

        Per-item RBAC/existence failures (unknown id, another user's sandbox)
        are reported in the per-item ``results`` array, not a whole-batch abort.
        """
        _validate_bulk_action_args(ids, action)
        resp = self._http.post(
            "/sandboxes/bulk-action", json={"action": action, "ids": list(ids)}
        )
        raise_for_status(resp)
        results = resp.json().get("results") or []
        return [BulkActionResultItem.model_validate(r) for r in results]


class AsyncSandboxesAPI:
    """Async mirror of :class:`SandboxesAPI`. See its docstrings for the full contract."""

    def __init__(self, client: "AsyncAgentTierClient") -> None:
        self._http: "httpx.AsyncClient" = client._http

    async def create_bulk(self, items: Sequence[BulkCreateItem]) -> list[BulkCreateResultItem]:
        _validate_bulk_create_items(items)
        body = {"items": [_item_to_body(item) for item in items]}
        resp = await self._http.post("/sandboxes/bulk", json=body)
        raise_for_status(resp)
        results = resp.json().get("results") or []
        return [BulkCreateResultItem.model_validate(r) for r in results]

    async def bulk_action(self, ids: Sequence[str], action: str) -> list[BulkActionResultItem]:
        _validate_bulk_action_args(ids, action)
        resp = await self._http.post(
            "/sandboxes/bulk-action", json={"action": action, "ids": list(ids)}
        )
        raise_for_status(resp)
        results = resp.json().get("results") or []
        return [BulkActionResultItem.model_validate(r) for r in results]
