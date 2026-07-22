# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Sync client for the AgentTier REST API."""

from __future__ import annotations

from types import TracebackType
from typing import Optional

import httpx

from agenttier._http import default_user_agent, raise_for_status
from agenttier._retry import RetryConfig, wrap_transport
from agenttier._version import __version__
from agenttier.admin import AdminAPI
from agenttier.analytics import AnalyticsAPI
from agenttier.apikeys import APIKeysAPI
from agenttier.audit import AuditAPI
from agenttier.auth import AuthProvider, auto_detect_auth
from agenttier.bulk import SandboxesAPI
from agenttier.cluster import ClusterAPI
from agenttier.governance import GovernanceAPI
from agenttier.models import CurrentUser, SandboxSummary, Template
from agenttier.sandbox import Sandbox
from agenttier.user import UserAPI
from agenttier.warmpool import WarmPoolAPI
from agenttier.webhooks import WebhooksAPI

_API_PREFIX = "/api/v1"
_DEFAULT_TIMEOUT = 30.0


class AgentTierClient:
    """High-level sync client for the AgentTier REST API.

    Example:

    .. code-block:: python

        with AgentTierClient(api_url="https://agenttier.company.com") as client:
            sandbox = client.create_sandbox(template="general-coding", name="demo")
            sandbox.wait_until_running()
            result = sandbox.exec("uname -a")
            print(result.stdout)
            sandbox.terminate()

    Beyond sandbox CRUD, the client exposes one sub-client attribute per
    resource group: :attr:`governance`, :attr:`analytics`, :attr:`audit`,
    :attr:`admin`, :attr:`user`, :attr:`api_keys`, :attr:`warmpool`,
    :attr:`cluster`, :attr:`webhooks`, and :attr:`sandboxes` (bulk create/
    action). Each is documented in its own module (e.g.
    :mod:`agenttier.governance`); most admin-scoped calls require the
    caller's identity to carry the admin group/claim the Router is
    configured with.
    """

    def __init__(
        self,
        api_url: str,
        auth: Optional[AuthProvider] = None,
        timeout: float = _DEFAULT_TIMEOUT,
        *,
        verify: bool | str = True,
        retry: Optional[RetryConfig] = None,
    ) -> None:
        if not api_url:
            raise ValueError("api_url must be a non-empty string")
        self._api_url = api_url.rstrip("/")
        self._auth = auth or auto_detect_auth()
        # Wrap the default transport in a retry layer when the caller asked
        # for one. Default is no retries — failing fast is the safer
        # behavior for code that already runs against a healthy local
        # cluster.
        transport: httpx.BaseTransport = httpx.HTTPTransport(verify=verify)
        transport = wrap_transport(transport, retry)
        self._http = httpx.Client(
            base_url=f"{self._api_url}{_API_PREFIX}",
            timeout=timeout,
            transport=transport,
            headers={"User-Agent": default_user_agent(__version__)},
            event_hooks={"request": [self._apply_auth]},
        )

        # Sub-clients for the newer resource groups (FR1). Each takes only
        # ``self._http`` (via this client), so constructing them here is
        # cheap and side-effect-free — no extra network calls at __init__.
        self.governance = GovernanceAPI(self)
        self.analytics = AnalyticsAPI(self)
        self.audit = AuditAPI(self)
        self.admin = AdminAPI(self)
        self.user = UserAPI(self)
        self.api_keys = APIKeysAPI(self)
        self.warmpool = WarmPoolAPI(self)
        self.cluster = ClusterAPI(self)
        self.webhooks = WebhooksAPI(self)
        self.sandboxes = SandboxesAPI(self)

    # ------- context manager --------------------------------------------

    def __enter__(self) -> "AgentTierClient":
        return self

    def __exit__(
        self,
        exc_type: Optional[type[BaseException]],
        exc: Optional[BaseException],
        tb: Optional[TracebackType],
    ) -> None:
        self.close()

    def close(self) -> None:
        self._http.close()

    # ------- sandboxes --------------------------------------------------

    def create_sandbox(
        self,
        template: str,
        name: str,
        namespace: str = "default",
        timeout: Optional[str] = None,
        idle_timeout: Optional[str] = None,
        storage_size: Optional[str] = None,
    ) -> Sandbox:
        """Create a sandbox from a ``ClusterSandboxTemplate``.

        ``timeout`` and ``idle_timeout`` take Go-style duration strings
        (``"8h"``, ``"30m"``).
        """
        if not template:
            raise ValueError("template must be a non-empty string")
        if not name:
            raise ValueError("name must be a non-empty string")

        body: dict[str, object] = {
            "name": name,
            "namespace": namespace,
            "templateRef": {"name": template, "kind": "ClusterSandboxTemplate"},
        }
        if timeout:
            body["timeout"] = timeout
        if idle_timeout:
            body["idleTimeout"] = idle_timeout
        if storage_size:
            body["storage"] = {"size": storage_size}

        resp = self._http.post("/sandboxes", json=body)
        raise_for_status(resp)
        data = resp.json()
        return Sandbox(
            self._http,
            data["sandboxId"],
            data.get("name", name),
            data.get("namespace", namespace),
        )

    def list_sandboxes(
        self,
        namespace: Optional[str] = None,
        status: Optional[str] = None,
    ) -> list[SandboxSummary]:
        """List sandboxes visible to the caller."""
        params: dict[str, str] = {}
        if namespace:
            params["namespace"] = namespace
        if status:
            params["status"] = status

        resp = self._http.get("/sandboxes", params=params)
        raise_for_status(resp)
        return [SandboxSummary.model_validate(s) for s in (resp.json().get("sandboxes") or [])]

    def get_sandbox(self, sandbox_id: str) -> Sandbox:
        """Return a handle to an existing sandbox."""
        if not sandbox_id:
            raise ValueError("sandbox_id must be a non-empty string")
        resp = self._http.get(f"/sandboxes/{sandbox_id}")
        raise_for_status(resp)
        data = resp.json()
        return Sandbox(
            self._http,
            data["sandboxId"],
            data.get("name", sandbox_id),
            data.get("namespace", "default"),
        )

    # ------- templates --------------------------------------------------

    def list_templates(self) -> list[Template]:
        resp = self._http.get("/templates")
        raise_for_status(resp)
        return [Template.model_validate(t) for t in (resp.json().get("templates") or [])]

    def get_template(self, name: str) -> Template:
        if not name:
            raise ValueError("name must be a non-empty string")
        resp = self._http.get(f"/templates/{name}")
        raise_for_status(resp)
        return Template.model_validate(resp.json())

    # ------- identity ---------------------------------------------------

    def current_user(self) -> CurrentUser:
        """Return the server's view of the caller's identity.

        Uses the same logic as the Web UI — handy for verifying auth is wired
        up correctly.
        """
        resp = self._http.get("/user/me")
        raise_for_status(resp)
        return CurrentUser.model_validate(resp.json())

    # ------- internals --------------------------------------------------

    def _apply_auth(self, request: httpx.Request) -> None:
        self._auth.apply(request)
