# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Sync + async wrappers around the governance policy REST surface.

Wraps ``GET /governance/policies``, ``PUT /governance/policies`` (cluster
default), ``GET/PUT/DELETE /governance/policies/{namespace}``, and
``GET /governance/effective``. Lives in its own module, attached as
``client.governance``, so :class:`agenttier.client.AgentTierClient` /
:class:`agenttier.async_client.AsyncAgentTierClient` don't grow a
governance-specific import surface — same pattern as ``sharing.py``.

All write operations (``set``/``delete``) are admin-only on the Router side;
a non-admin caller gets :class:`agenttier.exceptions.AuthorizationError`.
"""

from __future__ import annotations

from typing import TYPE_CHECKING, Optional

from pydantic import Field

from agenttier._http import raise_for_status
from agenttier.models import _Model

if TYPE_CHECKING:  # pragma: no cover
    import httpx

    from agenttier.async_client import AsyncAgentTierClient
    from agenttier.client import AgentTierClient


class Policy(_Model):
    """Governance limits for a scope (cluster-wide or one namespace).

    Zero/empty fields mean "no limit" on the Router side. Mirrors
    ``pkg/governance.Policy``.
    """

    max_sandboxes_per_user: int = Field(default=0, alias="maxSandboxesPerUser")
    max_sandboxes_total: int = Field(default=0, alias="maxSandboxesTotal")
    max_cpu: Optional[str] = Field(default=None, alias="maxCpu")
    max_memory: Optional[str] = Field(default=None, alias="maxMemory")
    max_storage: Optional[str] = Field(default=None, alias="maxStorage")
    max_timeout: Optional[str] = Field(default=None, alias="maxTimeout")
    max_idle_timeout: Optional[str] = Field(default=None, alias="maxIdleTimeout")
    allowed_templates: list[str] = Field(default_factory=list, alias="allowedTemplates")
    approved_registries: list[str] = Field(default_factory=list, alias="approvedRegistries")
    max_agent_sandboxes: int = Field(default=0, alias="maxAgentSandboxes")
    allowed_agent_images: list[str] = Field(default_factory=list, alias="allowedAgentImages")
    max_concurrent_invokes_per_sandbox: int = Field(
        default=0, alias="maxConcurrentInvokesPerSandbox"
    )
    description: Optional[str] = None


class NamespacePolicy(_Model):
    """One namespace-scoped policy entry, as returned by ``list()``."""

    namespace: str
    policy: Policy


class PolicyList(_Model):
    """Full policy set returned by ``GET /governance/policies``."""

    cluster: Optional[Policy] = None
    namespaces: list[NamespacePolicy] = Field(default_factory=list)


class EffectivePolicy(_Model):
    """Resolved policy for a namespace, as returned by ``GET /governance/effective``."""

    namespace: str
    policy: Policy


class GovernanceAPI:
    """Sync wrapper for ``/governance/*``.

    Use ``client.governance`` to obtain an instance; don't construct directly.
    """

    def __init__(self, client: "AgentTierClient") -> None:
        self._http: "httpx.Client" = client._http

    def list(self) -> PolicyList:
        """Return the cluster default policy plus every namespace override."""
        resp = self._http.get("/governance/policies")
        raise_for_status(resp)
        return PolicyList.model_validate(resp.json())

    def get(self, namespace: str) -> Policy:
        """Return the raw (not-yet-resolved) policy stored for ``namespace``.

        Use :meth:`effective` instead if you want the fully-resolved policy
        that actually applies (namespace override merged over cluster default).
        """
        if not namespace:
            raise ValueError("namespace must be a non-empty string")
        resp = self._http.get(f"/governance/policies/{namespace}")
        raise_for_status(resp)
        data = resp.json()
        return Policy.model_validate(data.get("policy", data))

    def set(self, policy: Policy, namespace: Optional[str] = None) -> Policy:
        """Create or replace a policy.

        Omit ``namespace`` (or pass ``None``) to set the cluster-wide
        default (``PUT /governance/policies``); pass a namespace to set a
        namespace-specific override (``PUT /governance/policies/{namespace}``).
        """
        body = policy.model_dump(by_alias=True, exclude_defaults=True)
        if namespace:
            resp = self._http.put(f"/governance/policies/{namespace}", json=body)
        else:
            resp = self._http.put("/governance/policies", json=body)
        raise_for_status(resp)
        data = resp.json()
        return Policy.model_validate(data.get("policy", data))

    def delete(self, namespace: str) -> None:
        """Delete a namespace-specific policy override.

        The cluster-wide default cannot be deleted through this call — the
        Router only exposes ``DELETE`` on the per-namespace route.
        """
        if not namespace:
            raise ValueError("namespace must be a non-empty string")
        resp = self._http.delete(f"/governance/policies/{namespace}")
        raise_for_status(resp)

    def effective(self, namespace: Optional[str] = None) -> EffectivePolicy:
        """Return the fully-resolved policy that applies to ``namespace``.

        Omit ``namespace`` to use the Router's configured default namespace.
        """
        params = {"namespace": namespace} if namespace else {}
        resp = self._http.get("/governance/effective", params=params)
        raise_for_status(resp)
        return EffectivePolicy.model_validate(resp.json())


class AsyncGovernanceAPI:
    """Async mirror of :class:`GovernanceAPI`. See its docstrings for the full contract."""

    def __init__(self, client: "AsyncAgentTierClient") -> None:
        self._http: "httpx.AsyncClient" = client._http

    async def list(self) -> PolicyList:
        resp = await self._http.get("/governance/policies")
        raise_for_status(resp)
        return PolicyList.model_validate(resp.json())

    async def get(self, namespace: str) -> Policy:
        if not namespace:
            raise ValueError("namespace must be a non-empty string")
        resp = await self._http.get(f"/governance/policies/{namespace}")
        raise_for_status(resp)
        data = resp.json()
        return Policy.model_validate(data.get("policy", data))

    async def set(self, policy: Policy, namespace: Optional[str] = None) -> Policy:
        body = policy.model_dump(by_alias=True, exclude_defaults=True)
        if namespace:
            resp = await self._http.put(f"/governance/policies/{namespace}", json=body)
        else:
            resp = await self._http.put("/governance/policies", json=body)
        raise_for_status(resp)
        data = resp.json()
        return Policy.model_validate(data.get("policy", data))

    async def delete(self, namespace: str) -> None:
        if not namespace:
            raise ValueError("namespace must be a non-empty string")
        resp = await self._http.delete(f"/governance/policies/{namespace}")
        raise_for_status(resp)

    async def effective(self, namespace: Optional[str] = None) -> EffectivePolicy:
        params = {"namespace": namespace} if namespace else {}
        resp = await self._http.get("/governance/effective", params=params)
        raise_for_status(resp)
        return EffectivePolicy.model_validate(resp.json())
