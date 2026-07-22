# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""AgentTier Python SDK.

Isolated, persistent Kubernetes sandboxes for humans and AI agents, from Python.

Typical usage::

    from agenttier import AgentTierClient

    with AgentTierClient(api_url="https://agenttier.company.com") as client:
        sandbox = client.create_sandbox(template="general-coding", name="demo")
        sandbox.wait_until_running()
        print(sandbox.exec("uname -a").stdout)
        sandbox.terminate()

See :mod:`agenttier.async_client` for the ``async/await`` variant.
"""

from agenttier._retry import RetryConfig
from agenttier._version import __version__
from agenttier.apikeys import APIKeyCreated, APIKeyMetadata
from agenttier.async_client import AsyncAgentTierClient
from agenttier.async_sandbox import AsyncSandbox
from agenttier.auth import APIKeyAuth, AuthProvider, BearerTokenAuth, KubeconfigAuth
from agenttier.backups import BackupInfo
from agenttier.bulk import BulkActionResultItem, BulkCreateItem, BulkCreateResultItem, PatchResult
from agenttier.client import AgentTierClient
from agenttier.cluster import (
    ClusterStatus,
    HeadroomConfig,
    NodeCapacity,
    NodeCapacityResponse,
    NodeCapacitySummary,
    NodeResources,
)
from agenttier.exceptions import (
    AgentTierError,
    APIError,
    AuthenticationError,
    AuthorizationError,
    ConflictError,
    NotFoundError,
    PolicyViolationError,
    SandboxErrorState,
    SandboxTimeoutError,
)
from agenttier.governance import EffectivePolicy, NamespacePolicy, Policy, PolicyList
from agenttier.models import (
    AuditEvent,
    CommandResult,
    ConfigureResult,
    CreatedBy,
    CurrentUser,
    ForwardedPort,
    InvokeEvent,
    InvokeResult,
    SandboxPhase,
    SandboxSummary,
    Template,
    UsageAnalytics,
)
from agenttier.analytics import CostEstimate, TemplateCost
from agenttier.sandbox import Sandbox
from agenttier.sharing import ShareLinkCreated, ShareLinkInfo, SharePermission, SharingInfo
from agenttier.warmpool import PoolConfig, PoolStatus, WarmPoolStatus
from agenttier.webhooks import WebhookDelivery, WebhookSubscription, WebhookSubscriptionCreated

__all__ = [
    "__version__",
    # Clients
    "AgentTierClient",
    "AsyncAgentTierClient",
    # Handles
    "Sandbox",
    "AsyncSandbox",
    # Auth
    "AuthProvider",
    "APIKeyAuth",
    "BearerTokenAuth",
    "KubeconfigAuth",
    # Retry
    "RetryConfig",
    # Models
    "AuditEvent",
    "CommandResult",
    "ConfigureResult",
    "CreatedBy",
    "CurrentUser",
    "ForwardedPort",
    "InvokeEvent",
    "InvokeResult",
    "SandboxPhase",
    "SandboxSummary",
    "Template",
    "UsageAnalytics",
    # Sharing (FR1.1)
    "SharePermission",
    "ShareLinkInfo",
    "SharingInfo",
    "ShareLinkCreated",
    # Backups (FR3)
    "BackupInfo",
    # Governance (FR1.2)
    "Policy",
    "NamespacePolicy",
    "PolicyList",
    "EffectivePolicy",
    # Analytics (FR1.3)
    "TemplateCost",
    "CostEstimate",
    # API keys (FR1.5 / FR6)
    "APIKeyMetadata",
    "APIKeyCreated",
    # Warm pool (FR1.6)
    "PoolConfig",
    "PoolStatus",
    "WarmPoolStatus",
    # Cluster (FR1.7)
    "ClusterStatus",
    "NodeResources",
    "NodeCapacity",
    "NodeCapacitySummary",
    "NodeCapacityResponse",
    "HeadroomConfig",
    # Webhooks (FR5)
    "WebhookSubscription",
    "WebhookSubscriptionCreated",
    "WebhookDelivery",
    # Bulk + PATCH (FR2 / FR4)
    "BulkCreateItem",
    "BulkCreateResultItem",
    "BulkActionResultItem",
    "PatchResult",
    # Exceptions
    "AgentTierError",
    "APIError",
    "AuthenticationError",
    "AuthorizationError",
    "ConflictError",
    "NotFoundError",
    "PolicyViolationError",
    "SandboxErrorState",
    "SandboxTimeoutError",
]
