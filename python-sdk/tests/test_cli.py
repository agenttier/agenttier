# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""Tests for the Python CLI's newly-registered command families (G4).

Exercises argparse wiring + dispatch for the resource groups added in this
task: sandbox patch/bulk-create/bulk-action/sharing/backups, and the
top-level governance/audit/analytics/admin/user/apikeys/warmpool/cluster/
webhooks families. Uses ``pytest-httpx`` to stub the Router the same way the
SDK's own tests do; every command is invoked through ``main()`` with an
explicit ``--api-url`` so no on-disk config file or env var is required.
"""

from __future__ import annotations

import json

import pytest
from pytest_httpx import HTTPXMock

from agenttier.cli import build_parser, main

API_URL = "http://router.test"
BASE = f"{API_URL}/api/v1"

_SANDBOX_RESP = {
    "sandboxId": "sb1",
    "name": "sb1",
    "namespace": "default",
    "status": "Running",
}


def _register_get_sandbox(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(method="GET", url=f"{BASE}/sandboxes/sb1", json=_SANDBOX_RESP)


def test_build_parser_lists_all_new_families() -> None:
    """``agenttier --help`` must list every new command family (acceptance criterion)."""
    help_text = build_parser().format_help()
    for family in (
        "sandbox",
        "template",
        "configure",
        "invoke",
        "governance",
        "audit",
        "analytics",
        "admin",
        "user",
        "apikeys",
        "warmpool",
        "cluster",
        "webhooks",
    ):
        assert family in help_text, f"{family!r} missing from top-level --help"


def test_sandbox_help_lists_patch_bulk_sharing_backups(capsys: pytest.CaptureFixture[str]) -> None:
    with pytest.raises(SystemExit):
        main(["sandbox", "--help"])
    out = capsys.readouterr().out
    for sub in ("patch", "bulk-create", "bulk-action", "sharing", "backups"):
        assert sub in out, f"{sub!r} missing from `agenttier sandbox --help`"


# --- sandbox patch -----------------------------------------------------


def test_sandbox_patch_sends_idle_timeout(httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="PATCH",
        url=f"{BASE}/sandboxes/sb1",
        match_json={"idleTimeout": "30m"},
        json={"sandboxId": "sb1", "applied": {"idleTimeout": "immediately"}, "restartRequired": False},
    )
    rc = main(["sandbox", "patch", "sb1", "--idle-timeout", "30m", "--api-url", API_URL])
    assert rc == 0
    assert "patched sb1" in capsys.readouterr().out


def test_sandbox_patch_sends_resources_and_reports_restart(
    httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]
) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="PATCH",
        url=f"{BASE}/sandboxes/sb1",
        match_json={"resources": {"limits": {"cpu": "2", "memory": "4Gi"}}},
        json={
            "sandboxId": "sb1",
            "applied": {"resources": "on-restart"},
            "restartRequired": True,
            "message": "resource changes take effect after the sandbox is stopped and resumed",
        },
    )
    rc = main(
        ["sandbox", "patch", "sb1", "--cpu-limit", "2", "--memory-limit", "4Gi", "--api-url", API_URL]
    )
    assert rc == 0
    out = capsys.readouterr().out
    assert "note:" in out


def test_sandbox_patch_sends_labels_and_annotations(httpx_mock: HTTPXMock) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="PATCH",
        url=f"{BASE}/sandboxes/sb1",
        match_json={"labels": {"team": "x"}, "annotations": {"note": "y"}},
        json={"sandboxId": "sb1", "applied": {}, "restartRequired": False},
    )
    rc = main(
        [
            "sandbox",
            "patch",
            "sb1",
            "--label",
            "team=x",
            "--annotation",
            "note=y",
            "--api-url",
            API_URL,
        ]
    )
    assert rc == 0


def test_sandbox_patch_rejects_no_fields(capsys: pytest.CaptureFixture[str]) -> None:
    rc = main(["sandbox", "patch", "sb1", "--api-url", API_URL])
    assert rc == 2
    assert "at least one of" in capsys.readouterr().err


# --- sandbox bulk-create / bulk-action ----------------------------------


def test_sandbox_bulk_create_reads_json_file(
    tmp_path, httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]
) -> None:
    spec_file = tmp_path / "specs.json"
    spec_file.write_text(json.dumps([{"template": "general-coding", "name": "a"}]))
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/bulk",
        json={"results": [{"index": 0, "status": "created", "sandboxId": "a"}]},
    )
    rc = main(["sandbox", "bulk-create", "--file", str(spec_file), "--api-url", API_URL])
    assert rc == 0
    out = capsys.readouterr().out
    assert "created" in out
    assert "a" in out


def test_sandbox_bulk_create_rejects_non_array_json(
    tmp_path, capsys: pytest.CaptureFixture[str]
) -> None:
    spec_file = tmp_path / "specs.json"
    spec_file.write_text(json.dumps({"template": "t", "name": "a"}))
    rc = main(["sandbox", "bulk-create", "--file", str(spec_file), "--api-url", API_URL])
    assert rc == 2
    assert "JSON array" in capsys.readouterr().err


def test_sandbox_bulk_action_posts_ids(httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/bulk-action",
        match_json={"action": "stop", "ids": ["a", "b"]},
        json={"results": [{"id": "a", "status": "stopped"}, {"id": "b", "status": "stopped"}]},
    )
    rc = main(["sandbox", "bulk-action", "--action", "stop", "a", "b", "--api-url", API_URL])
    assert rc == 0
    out = capsys.readouterr().out
    assert "stopped" in out


# --- sandbox sharing -----------------------------------------------------


def test_sharing_grant_and_list(httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/sb1/share",
        match_json={"identity": "alice", "level": "viewer", "kind": "user"},
        json={"users": [{"identity": "alice", "level": "viewer"}], "groups": [], "shareLinks": []},
    )
    rc = main(["sandbox", "sharing", "grant", "sb1", "alice", "--api-url", API_URL])
    assert rc == 0
    assert "granted viewer to alice" in capsys.readouterr().out

    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/sandboxes/sb1/share",
        json={"users": [{"identity": "alice", "level": "viewer"}], "groups": [], "shareLinks": []},
    )
    rc = main(["sandbox", "sharing", "list", "sb1", "--api-url", API_URL])
    assert rc == 0
    assert "alice" in capsys.readouterr().out


def test_sharing_revoke(httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(method="DELETE", url=f"{BASE}/sandboxes/sb1/share/alice", status_code=204)
    rc = main(["sandbox", "sharing", "revoke", "sb1", "alice", "--api-url", API_URL])
    assert rc == 0
    assert "revoked" in capsys.readouterr().out


def test_sharing_create_link(httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/sb1/share-links",
        json={"id": "link1", "token": "shhh", "level": "viewer", "maxUses": 0},
    )
    rc = main(["sandbox", "sharing", "create-link", "sb1", "--api-url", API_URL])
    assert rc == 0
    assert "shhh" in capsys.readouterr().out


# --- sandbox backups -----------------------------------------------------


def test_backups_create_and_list(httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/sb1/backups",
        json={"name": "sb1-snap-1", "kind": "scheduled-backup", "readyToUse": False},
    )
    rc = main(["sandbox", "backups", "create", "sb1", "--api-url", API_URL])
    assert rc == 0
    assert "sb1-snap-1" in capsys.readouterr().out

    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/sandboxes/sb1/backups",
        json={"backups": [{"name": "sb1-snap-1", "kind": "scheduled-backup", "readyToUse": True}]},
    )
    rc = main(["sandbox", "backups", "list", "sb1", "--api-url", API_URL])
    assert rc == 0
    assert "sb1-snap-1" in capsys.readouterr().out


def test_backups_restore_and_delete(httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]) -> None:
    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/sandboxes/sb1/backups/sb1-snap-1/restore",
        json={"name": "restored", "namespace": "default"},
    )
    rc = main(["sandbox", "backups", "restore", "sb1", "sb1-snap-1", "--api-url", API_URL])
    assert rc == 0
    assert "restored" in capsys.readouterr().out

    _register_get_sandbox(httpx_mock)
    httpx_mock.add_response(method="DELETE", url=f"{BASE}/sandboxes/sb1/backups/sb1-snap-1")
    rc = main(["sandbox", "backups", "delete", "sb1", "sb1-snap-1", "--api-url", API_URL])
    assert rc == 0
    assert "deleted backup sb1-snap-1" in capsys.readouterr().out


# --- governance ----------------------------------------------------------


def test_governance_list_get_effective(httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]) -> None:
    httpx_mock.add_response(
        method="GET", url=f"{BASE}/governance/policies", json={"cluster": {"maxSandboxesTotal": 5}, "namespaces": []}
    )
    rc = main(["governance", "list", "--api-url", API_URL])
    assert rc == 0
    assert "max_sandboxes_total" in capsys.readouterr().out

    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/governance/policies/team-a",
        json={"namespace": "team-a", "policy": {"maxSandboxesPerUser": 3}},
    )
    rc = main(["governance", "get", "team-a", "--api-url", API_URL])
    assert rc == 0

    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/governance/effective?namespace=team-a",
        json={"namespace": "team-a", "policy": {"maxSandboxesTotal": 5}},
    )
    rc = main(["governance", "effective", "team-a", "--api-url", API_URL])
    assert rc == 0


def test_governance_set_from_file(tmp_path, httpx_mock: HTTPXMock) -> None:
    policy_file = tmp_path / "policy.json"
    policy_file.write_text(json.dumps({"maxSandboxesTotal": 10}))
    httpx_mock.add_response(
        method="PUT",
        url=f"{BASE}/governance/policies",
        json={"policy": {"maxSandboxesTotal": 10}},
    )
    rc = main(["governance", "set", "--file", str(policy_file), "--api-url", API_URL])
    assert rc == 0


def test_governance_delete(httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]) -> None:
    httpx_mock.add_response(method="DELETE", url=f"{BASE}/governance/policies/team-a", status_code=204)
    rc = main(["governance", "delete", "team-a", "--api-url", API_URL])
    assert rc == 0
    assert "deleted" in capsys.readouterr().out


# --- audit / analytics / admin --------------------------------------------


def test_audit_list(httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/audit/events",
        json={"events": [{"eventType": "sandbox.created", "sandboxId": "sb1"}]},
    )
    rc = main(["audit", "list", "--api-url", API_URL])
    assert rc == 0
    assert "sandbox.created" in capsys.readouterr().out


def test_analytics_usage_and_costs(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/analytics/usage",
        json={
            "total_sandboxes": 3,
            "status_breakdown": {},
            "template_breakdown": {},
            "avg_startup_ms": 100,
            "startup_sample_count": 3,
        },
    )
    rc = main(["analytics", "usage", "--api-url", API_URL])
    assert rc == 0

    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/analytics/costs",
        json={
            "running_sandboxes": 1,
            "stopped_sandboxes": 0,
            "total_hourly_compute": 0.1,
            "total_hourly_storage": 0.01,
            "total_estimated_monthly": 80.0,
            "per_template": [],
        },
    )
    rc = main(["analytics", "costs", "--api-url", API_URL])
    assert rc == 0


def test_admin_sandboxes_and_sharing(httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]) -> None:
    httpx_mock.add_response(method="GET", url=f"{BASE}/admin/sandboxes", json={"sandboxes": [_SANDBOX_RESP]})
    rc = main(["admin", "sandboxes", "--api-url", API_URL])
    assert rc == 0
    assert "sb1" in capsys.readouterr().out

    httpx_mock.add_response(method="GET", url=f"{BASE}/admin/sharing", json={})
    rc = main(["admin", "sharing", "--api-url", API_URL])
    assert rc == 0


# --- user / apikeys --------------------------------------------------------


def test_user_preferences_get_and_set(tmp_path, httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(method="GET", url=f"{BASE}/user/preferences", json={"theme": "dark"})
    rc = main(["user", "preferences-get", "--api-url", API_URL])
    assert rc == 0

    prefs_file = tmp_path / "prefs.json"
    prefs_file.write_text(json.dumps({"theme": "light"}))
    httpx_mock.add_response(method="PUT", url=f"{BASE}/user/preferences", json={"theme": "light"})
    rc = main(["user", "preferences-set", "--file", str(prefs_file), "--api-url", API_URL])
    assert rc == 0


def test_apikeys_list_create_revoke(httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]) -> None:
    httpx_mock.add_response(method="GET", url=f"{BASE}/user/api-keys", json={"keys": []})
    rc = main(["apikeys", "list", "--api-url", API_URL])
    assert rc == 0

    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/user/api-keys",
        json={"id": "key1", "key": "atk_abc", "warning": "store this now"},
    )
    rc = main(["apikeys", "create", "--name", "ci", "--api-url", API_URL])
    assert rc == 0
    out = capsys.readouterr().out
    assert "atk_abc" in out

    httpx_mock.add_response(method="DELETE", url=f"{BASE}/user/api-keys/key1", status_code=200)
    rc = main(["apikeys", "revoke", "key1", "--api-url", API_URL])
    assert rc == 0


def test_apikeys_create_scoped(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/user/api-keys",
        match_json={"sandboxId": "sb1", "actionGroups": ["run-command"]},
        json={"id": "key2", "key": "atk_scoped"},
    )
    rc = main(
        [
            "apikeys",
            "create",
            "--sandbox-id",
            "sb1",
            "--action-group",
            "run-command",
            "--api-url",
            API_URL,
        ]
    )
    assert rc == 0


# --- warmpool / cluster ----------------------------------------------------


def test_warmpool_status_and_config(tmp_path, httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/warmpool/status",
        json={"pools": [], "desiredCount": 0, "readyCount": 0, "pendingCount": 0, "template": ""},
    )
    rc = main(["warmpool", "status", "--api-url", API_URL])
    assert rc == 0

    pools_file = tmp_path / "pools.json"
    pools_file.write_text(json.dumps([{"template": "general-coding", "desiredCount": 2}]))
    httpx_mock.add_response(
        method="PUT",
        url=f"{BASE}/warmpool/config",
        json={"pools": [{"template": "general-coding", "desiredCount": 2}]},
    )
    rc = main(["warmpool", "config", "--file", str(pools_file), "--api-url", API_URL])
    assert rc == 0


def test_cluster_status_nodes_headroom(httpx_mock: HTTPXMock) -> None:
    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/cluster/status",
        json={"nodes": 3, "nodesReady": 3, "pods": 10, "sandboxPods": 5, "headroomReady": 1, "autoscalerEnabled": True},
    )
    rc = main(["cluster", "status", "--api-url", API_URL])
    assert rc == 0

    httpx_mock.add_response(method="GET", url=f"{BASE}/cluster/nodes", json={"nodes": [], "summary": {}})
    rc = main(["cluster", "nodes", "--api-url", API_URL])
    assert rc == 0

    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/cluster/headroom",
        json={"replicas": 2, "readyReplicas": 2, "enabled": True},
    )
    rc = main(["cluster", "headroom-get", "--api-url", API_URL])
    assert rc == 0

    httpx_mock.add_response(
        method="PUT",
        url=f"{BASE}/cluster/headroom",
        match_json={"replicas": 3, "cpu": "1", "memory": "1Gi"},
        json={"replicas": 3, "readyReplicas": 0, "enabled": True},
    )
    rc = main(
        ["cluster", "headroom-set", "--replicas", "3", "--cpu", "1", "--memory", "1Gi", "--api-url", API_URL]
    )
    assert rc == 0


# --- webhooks ---------------------------------------------------------------


def test_webhooks_create_list_delete_deliveries(
    httpx_mock: HTTPXMock, capsys: pytest.CaptureFixture[str]
) -> None:
    httpx_mock.add_response(
        method="POST",
        url=f"{BASE}/webhooks",
        json={
            "id": "wh1",
            "url": "https://example.com/hook",
            "eventTypes": ["sandbox.running"],
            "secret": "topsecret",
        },
    )
    rc = main(
        [
            "webhooks",
            "create",
            "--url",
            "https://example.com/hook",
            "--event-type",
            "sandbox.running",
            "--api-url",
            API_URL,
        ]
    )
    assert rc == 0
    assert "topsecret" in capsys.readouterr().out

    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/webhooks",
        json={"webhooks": [{"id": "wh1", "url": "https://example.com/hook", "eventTypes": ["sandbox.running"]}]},
    )
    rc = main(["webhooks", "list", "--api-url", API_URL])
    assert rc == 0
    assert "wh1" in capsys.readouterr().out

    httpx_mock.add_response(
        method="GET",
        url=f"{BASE}/webhooks/wh1/deliveries",
        json={"deliveries": [{"eventType": "sandbox.running", "attempt": 1, "success": True}]},
    )
    rc = main(["webhooks", "deliveries", "wh1", "--api-url", API_URL])
    assert rc == 0

    httpx_mock.add_response(method="DELETE", url=f"{BASE}/webhooks/wh1", status_code=204)
    rc = main(["webhooks", "delete", "wh1", "--api-url", API_URL])
    assert rc == 0


def test_webhooks_create_rejects_invalid_event_type() -> None:
    """Local validation (FR1.10) must reject before any network call.

    ``main()`` only special-cases ``AgentTierError``/``KeyboardInterrupt``
    (see its except clauses); a guard-clause ``ValueError`` from the SDK
    layer propagates to the caller uncaught, same as every other local
    validation error in this CLI (e.g. ``cmd_sandbox_exec``'s empty-command
    check raises via ``_err`` + explicit return, but SDK-level guard clauses
    like this one raise directly).
    """
    with pytest.raises(ValueError, match="unknown event type"):
        main(
            [
                "webhooks",
                "create",
                "--url",
                "https://example.com/hook",
                "--event-type",
                "sandbox.exploded",
                "--api-url",
                API_URL,
            ]
        )
