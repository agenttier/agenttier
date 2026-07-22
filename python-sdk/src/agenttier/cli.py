# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

"""AgentTier command-line interface.

Pure-Python CLI built on top of the AgentTier SDK. Installed as a
``console_scripts`` entry point named ``agenttier`` — see ``pyproject.toml``.

Why Python on top of the SDK and not a thin wrapper around the Go binary:
the Go CLI ships only ``configure`` and ``invoke`` as of v0.3.0. The Python
CLI exposes the full SDK surface (lifecycle, exec, files, port forwards,
templates, identity) so ``pip install agenttier`` is a one-stop install for
Python users. The Go CLI continues to ship as a native binary on GitHub
Releases for users who do not want a Python runtime.

Design notes:

* No third-party CLI framework — the CLI is a flat dispatch table over
  argparse subparsers. Keeps the wheel tiny (~25 KB added) and avoids a
  dependency on ``click`` / ``typer``.
* All commands work in two output modes: ``--output text`` (human
  default) and ``--output json`` (scriptable). The text formatter handles
  truncation; JSON is a verbatim dump of the SDK's pydantic models.
* SSE-streaming commands (``configure``, ``invoke``) reuse the SDK's
  generators directly, so any improvement to the streaming logic flows
  into the CLI for free.
"""

from __future__ import annotations

import argparse
import json
import os
import shlex
import sys
from pathlib import Path
from typing import Any, Callable, Optional, Sequence

from agenttier import AgentTierClient
from agenttier._version import __version__
from agenttier.exceptions import AgentTierError


# ----------------------------------------------------------------------
# Output helpers
# ----------------------------------------------------------------------


def _print_json(data: Any) -> None:
    """Dump ``data`` to stdout as pretty JSON, handling pydantic models."""

    def _encode(obj: Any) -> Any:
        if hasattr(obj, "model_dump"):
            return obj.model_dump(mode="json")
        if isinstance(obj, list):
            return [_encode(o) for o in obj]
        if isinstance(obj, dict):
            return {k: _encode(v) for k, v in obj.items()}
        return obj

    json.dump(_encode(data), sys.stdout, indent=2, default=str)
    sys.stdout.write("\n")


def _print_table(rows: Sequence[Sequence[str]], headers: Sequence[str]) -> None:
    """Render a simple aligned text table to stdout."""
    if not rows:
        for h in headers:
            sys.stdout.write(h + "  ")
        sys.stdout.write("\n(no results)\n")
        return
    widths = [len(h) for h in headers]
    for row in rows:
        for i, cell in enumerate(row):
            widths[i] = max(widths[i], len(str(cell)))
    fmt = "  ".join("{:<" + str(w) + "}" for w in widths)
    sys.stdout.write(fmt.format(*headers).rstrip() + "\n")
    for row in rows:
        sys.stdout.write(fmt.format(*[str(c) for c in row]).rstrip() + "\n")


def _err(msg: str) -> None:
    sys.stderr.write(f"agenttier: {msg}\n")


def _parse_key_value_pairs(pairs: Sequence[str], flag: str) -> dict[str, str]:
    """Parse repeated ``key=value`` CLI args (e.g. ``--label``) into a dict."""
    out: dict[str, str] = {}
    for pair in pairs:
        if "=" not in pair:
            _err(f"{flag} {pair!r} must be key=value")
            sys.exit(2)
        k, v = pair.split("=", 1)
        if not k:
            _err(f"{flag} {pair!r} must be key=value")
            sys.exit(2)
        out[k] = v
    return out


# ----------------------------------------------------------------------
# Configuration: CLI flags → SDK env vars
# ----------------------------------------------------------------------


CONFIG_PATH = (
    Path(os.environ["AGENTTIER_CONFIG"])
    if os.environ.get("AGENTTIER_CONFIG")
    else Path.home() / ".config" / "agenttier" / "config.json"
)


def _load_config_file() -> dict[str, str]:
    if not CONFIG_PATH.exists():
        return {}
    try:
        data: dict[str, str] = json.loads(CONFIG_PATH.read_text())
        return data
    except (OSError, json.JSONDecodeError):
        return {}


def _save_config_file(config: dict[str, str]) -> None:
    CONFIG_PATH.parent.mkdir(parents=True, exist_ok=True)
    CONFIG_PATH.write_text(json.dumps(config, indent=2) + "\n")
    # Lock down permissions; the file may carry an API key.
    try:
        CONFIG_PATH.chmod(0o600)
    except OSError:
        pass


def _resolve_endpoint(args: argparse.Namespace) -> tuple[str, dict[str, str]]:
    """Return ``(api_url, env_overrides)`` for the SDK client.

    Precedence: CLI flag > env var > saved config file.
    Saved config file values are turned into env vars so the SDK's
    ``auto_detect_auth`` picks them up.
    """
    config = _load_config_file()
    env: dict[str, str] = {}

    api_url = args.api_url or os.environ.get("AGENTTIER_API_URL") or config.get("api_url", "")
    if not api_url:
        _err(
            "no API URL configured. Pass --api-url, set AGENTTIER_API_URL, "
            "or run `agenttier login --api-url <URL>`."
        )
        sys.exit(2)

    api_key = (
        getattr(args, "api_key", None)
        or os.environ.get("AGENTTIER_API_KEY")
        or config.get("api_key", "")
    )
    token = (
        getattr(args, "token", None)
        or os.environ.get("AGENTTIER_TOKEN")
        or config.get("token", "")
    )
    if api_key:
        env["AGENTTIER_API_KEY"] = api_key
    if token:
        env["AGENTTIER_TOKEN"] = token
    return api_url, env


def _client(args: argparse.Namespace) -> AgentTierClient:
    api_url, overrides = _resolve_endpoint(args)
    # Apply env overrides for the SDK's auto-detect.
    for k, v in overrides.items():
        os.environ[k] = v
    return AgentTierClient(api_url=api_url)


# ----------------------------------------------------------------------
# Top-level commands
# ----------------------------------------------------------------------


def cmd_version(args: argparse.Namespace) -> int:
    if args.output == "json":
        _print_json({"version": __version__})
    else:
        sys.stdout.write(f"agenttier {__version__}\n")
    return 0


def cmd_login(args: argparse.Namespace) -> int:
    if not args.api_url:
        _err("login requires --api-url")
        return 2
    config = _load_config_file()
    config["api_url"] = args.api_url
    if args.api_key:
        config["api_key"] = args.api_key
    if args.token:
        config["token"] = args.token
    _save_config_file(config)
    sys.stdout.write(f"Saved config to {CONFIG_PATH}\n")
    # Verify by hitting /user/me.
    try:
        with _client(args) as client:
            who = client.current_user()
            label = who.email or who.name or who.sub
            sys.stdout.write(f"Authenticated as: {label}\n")
    except AgentTierError as e:
        _err(f"saved config but auth check failed: {e}")
        return 1
    return 0


def cmd_whoami(args: argparse.Namespace) -> int:
    with _client(args) as client:
        who = client.current_user()
    if args.output == "json":
        _print_json(who)
    else:
        label = who.email or who.name or who.sub
        sys.stdout.write(label)
        if who.is_admin:
            sys.stdout.write(" (admin)")
        sys.stdout.write("\n")
    return 0


# ----------------------------------------------------------------------
# Sandbox commands
# ----------------------------------------------------------------------


def cmd_sandbox_list(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandboxes = client.list_sandboxes(namespace=args.namespace, status=args.status)
    if args.output == "json":
        _print_json(sandboxes)
        return 0
    rows = [
        (
            s.sandbox_id,
            s.name,
            s.status,
            s.template_ref or "",
            s.namespace,
        )
        for s in sandboxes
    ]
    _print_table(rows, headers=["ID", "NAME", "STATUS", "TEMPLATE", "NAMESPACE"])
    return 0


def cmd_sandbox_get(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        status = sandbox.status()
    if args.output == "json":
        _print_json(status)
    else:
        sys.stdout.write(f"id:        {status.sandbox_id}\n")
        sys.stdout.write(f"name:      {status.name}\n")
        sys.stdout.write(f"status:    {status.status}\n")
        sys.stdout.write(f"template:  {status.template_ref or '-'}\n")
        sys.stdout.write(f"namespace: {status.namespace}\n")
        if status.pod_name:
            sys.stdout.write(f"pod:       {status.pod_name}\n")
    return 0


def cmd_sandbox_create(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.create_sandbox(
            template=args.template,
            name=args.name,
            namespace=args.namespace,
            timeout=args.timeout,
            idle_timeout=args.idle_timeout,
            storage_size=args.storage_size,
        )
        if args.wait:
            sandbox.wait_until_running(timeout=args.wait_timeout)
        status = sandbox.status()
    if args.output == "json":
        _print_json(status)
    else:
        sys.stdout.write(f"created {status.sandbox_id} (status: {status.status})\n")
    return 0


def cmd_sandbox_stop(args: argparse.Namespace) -> int:
    with _client(args) as client:
        client.get_sandbox(args.sandbox_id).stop()
    sys.stdout.write(f"stopped {args.sandbox_id}\n")
    return 0


def cmd_sandbox_resume(args: argparse.Namespace) -> int:
    with _client(args) as client:
        client.get_sandbox(args.sandbox_id).resume()
    sys.stdout.write(f"resumed {args.sandbox_id}\n")
    return 0


def cmd_sandbox_delete(args: argparse.Namespace) -> int:
    with _client(args) as client:
        client.get_sandbox(args.sandbox_id).terminate()
    sys.stdout.write(f"deleted {args.sandbox_id}\n")
    return 0


def cmd_sandbox_clone(args: argparse.Namespace) -> int:
    """Clone an existing sandbox via VolumeSnapshot."""
    with _client(args) as client:
        source = client.get_sandbox(args.sandbox_id)
        clone = source.clone(
            name=args.name,
            snapshot_class=args.snapshot_class,
        )
        if args.output == "json":
            _print_json({
                "name": clone.name,
                "namespace": clone.namespace,
                "clonedFrom": source.id,
            })
        else:
            sys.stdout.write(f"clone {clone.name} created from {source.id}\n")
            sys.stdout.write(f"poll: agenttier sandbox get {clone.name}\n")
    return 0


def cmd_sandbox_exec(args: argparse.Namespace) -> int:
    if not args.command:
        _err("exec requires a command after `--`")
        return 2
    cmd = " ".join(shlex.quote(c) for c in args.command)
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        result = sandbox.exec(cmd, timeout=args.timeout)
    if args.output == "json":
        _print_json(result)
        return result.exit_code
    sys.stdout.write(result.stdout)
    if not result.stdout.endswith("\n") and result.stdout:
        sys.stdout.write("\n")
    if result.stderr:
        sys.stderr.write(result.stderr)
        if not result.stderr.endswith("\n"):
            sys.stderr.write("\n")
    return result.exit_code


def cmd_sandbox_wait(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        sandbox.wait_until_running(timeout=args.timeout)
    sys.stdout.write(f"{args.sandbox_id} is running\n")
    return 0


def cmd_sandbox_patch(args: argparse.Namespace) -> int:
    labels = _parse_key_value_pairs(args.label, "--label") if args.label else None
    annotations = _parse_key_value_pairs(args.annotation, "--annotation") if args.annotation else None
    resources = None
    if args.cpu_request or args.cpu_limit or args.memory_request or args.memory_limit:
        requests = {}
        if args.cpu_request:
            requests["cpu"] = args.cpu_request
        if args.memory_request:
            requests["memory"] = args.memory_request
        limits = {}
        if args.cpu_limit:
            limits["cpu"] = args.cpu_limit
        if args.memory_limit:
            limits["memory"] = args.memory_limit
        resources = {}
        if requests:
            resources["requests"] = requests
        if limits:
            resources["limits"] = limits
    if not (args.idle_timeout or resources or labels or annotations):
        _err("patch requires at least one of --idle-timeout, --cpu-request, --cpu-limit, --memory-request, --memory-limit, --label, --annotation")
        return 2
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        result = sandbox.update(
            idle_timeout=args.idle_timeout,
            resources=resources,
            labels=labels,
            annotations=annotations,
        )
    if args.output == "json":
        _print_json(result)
    else:
        sys.stdout.write(f"patched {result.sandbox_id}: applied={result.applied}\n")
        if result.restart_required:
            sys.stdout.write(f"note: {result.message or 'restart required to take effect'}\n")
    return 0


# ----------------------------------------------------------------------
# Sandbox bulk operations
# ----------------------------------------------------------------------


def cmd_sandbox_bulk_create(args: argparse.Namespace) -> int:
    raw = json.loads(Path(args.file).read_text()) if args.file != "-" else json.loads(sys.stdin.read())
    if not isinstance(raw, list):
        _err("bulk-create input must be a JSON array of sandbox specs")
        return 2
    from agenttier.bulk import BulkCreateItem

    items = [BulkCreateItem.model_validate(item) for item in raw]
    with _client(args) as client:
        results = client.sandboxes.create_bulk(items)
    if args.output == "json":
        _print_json(results)
        return 0
    rows = [(str(r.index), r.status, r.sandbox_id or "", r.error or "") for r in results]
    _print_table(rows, headers=["INDEX", "STATUS", "SANDBOX_ID", "ERROR"])
    return 0


def cmd_sandbox_bulk_action(args: argparse.Namespace) -> int:
    with _client(args) as client:
        results = client.sandboxes.bulk_action(args.ids, args.action)
    if args.output == "json":
        _print_json(results)
        return 0
    rows = [(r.id, r.status, r.error or "") for r in results]
    _print_table(rows, headers=["ID", "STATUS", "ERROR"])
    return 0


# ----------------------------------------------------------------------
# Files
# ----------------------------------------------------------------------


def cmd_files_ls(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        entries = sandbox.files.list(args.path)
    if args.output == "json":
        _print_json(entries)
        return 0
    rows = [
        (
            e.name + ("/" if e.is_dir else ""),
            "dir" if e.is_dir else "file",
            str(e.size),
            str(e.modified_at) if e.modified_at else "",
        )
        for e in entries
    ]
    _print_table(rows, headers=["NAME", "TYPE", "SIZE", "MODIFIED"])
    return 0


def cmd_files_cat(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        data = sandbox.files.read(args.path)
    sys.stdout.buffer.write(data)
    return 0


def cmd_files_upload(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        n = sandbox.files.upload(args.remote, args.local)
    sys.stdout.write(f"uploaded {n} bytes to {args.remote}\n")
    return 0


def cmd_files_download(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        n = sandbox.files.download(args.remote, args.local)
    sys.stdout.write(f"downloaded {n} bytes to {args.local}\n")
    return 0


def cmd_files_archive(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        n = sandbox.files.archive(args.dest, args.path)
    sys.stdout.write(f"archived {args.path} to {args.dest} ({n} bytes)\n")
    return 0


def cmd_files_write(args: argparse.Namespace) -> int:
    if args.data is not None:
        payload: bytes | str = args.data
    elif args.file == "-":
        payload = sys.stdin.buffer.read()
    elif args.file:
        payload = Path(args.file).read_bytes()
    else:
        _err("write requires --data or --file")
        return 2
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        sandbox.files.write(args.path, payload)
    sys.stdout.write(f"wrote {args.path}\n")
    return 0


# ----------------------------------------------------------------------
# Ports
# ----------------------------------------------------------------------


def cmd_ports_list(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        ports = sandbox.list_ports()
    if args.output == "json":
        _print_json(ports)
        return 0
    rows = [
        (str(p.port), p.protocol or "http", p.preview_url or p.internal_url or "")
        for p in ports
    ]
    _print_table(rows, headers=["PORT", "PROTOCOL", "URL"])
    return 0


def cmd_ports_forward(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        port = sandbox.forward_port(args.port, protocol=args.protocol)
    if args.output == "json":
        _print_json(port)
    else:
        target = port.preview_url or port.internal_url or "(no URL)"
        sys.stdout.write(f"forwarded port {port.port} → {target}\n")
    return 0


def cmd_ports_remove(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        sandbox.remove_port(args.port)
    sys.stdout.write(f"removed forward for port {args.port}\n")
    return 0


# ----------------------------------------------------------------------
# Sharing
# ----------------------------------------------------------------------


def cmd_sharing_list(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        info = sandbox.sharing.list()
    if args.output == "json":
        _print_json(info)
        return 0
    rows = [(p.identity, p.level, "user") for p in info.users] + [
        (p.identity, p.level, "group") for p in info.groups
    ]
    _print_table(rows, headers=["IDENTITY", "LEVEL", "KIND"])
    return 0


def cmd_sharing_grant(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        info = sandbox.sharing.grant(args.identity, level=args.level, kind=args.kind)
    if args.output == "json":
        _print_json(info)
    else:
        sys.stdout.write(f"granted {args.level} to {args.identity} ({args.kind})\n")
    return 0


def cmd_sharing_revoke(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        sandbox.sharing.revoke(args.identity)
    sys.stdout.write(f"revoked access for {args.identity}\n")
    return 0


def cmd_sharing_create_link(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        link = sandbox.sharing.create_link(
            level=args.level, expires_in=args.expires_in, max_uses=args.max_uses
        )
    if args.output == "json":
        _print_json(link)
    else:
        sys.stdout.write(f"share link created: {link.token}\n")
        if link.warning:
            sys.stdout.write(f"{link.warning}\n")
    return 0


# ----------------------------------------------------------------------
# Backups
# ----------------------------------------------------------------------


def cmd_backups_list(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        backups = sandbox.backups.list()
    if args.output == "json":
        _print_json(backups)
        return 0
    rows = [(b.name, b.kind, str(b.ready_to_use), b.restore_size or "") for b in backups]
    _print_table(rows, headers=["NAME", "KIND", "READY", "RESTORE_SIZE"])
    return 0


def cmd_backups_create(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        backup = sandbox.backups.create(snapshot_class=args.snapshot_class)
    if args.output == "json":
        _print_json(backup)
    else:
        sys.stdout.write(f"created backup {backup.name}\n")
    return 0


def cmd_backups_restore(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        restored = sandbox.backups.restore(args.snapshot_name, name=args.name)
    if args.output == "json":
        _print_json({"name": restored.name, "namespace": restored.namespace})
    else:
        sys.stdout.write(f"restored {args.snapshot_name} to {restored.name}\n")
    return 0


def cmd_backups_delete(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        sandbox.backups.delete(args.snapshot_name)
    sys.stdout.write(f"deleted backup {args.snapshot_name}\n")
    return 0


# ----------------------------------------------------------------------
# Templates
# ----------------------------------------------------------------------


def cmd_template_list(args: argparse.Namespace) -> int:
    with _client(args) as client:
        templates = client.list_templates()
    if args.output == "json":
        _print_json(templates)
        return 0
    rows = [
        (t.name, getattr(t, "image", "") or "-", getattr(t, "description", "") or "")
        for t in templates
    ]
    _print_table(rows, headers=["NAME", "IMAGE", "DESCRIPTION"])
    return 0


def cmd_template_get(args: argparse.Namespace) -> int:
    with _client(args) as client:
        template = client.get_template(args.name)
    _print_json(template)
    return 0


# ----------------------------------------------------------------------
# Agent: configure + invoke + cancel (mirrors Go CLI)
# ----------------------------------------------------------------------


def cmd_configure(args: argparse.Namespace) -> int:
    files: list[dict[str, Any] | tuple[str, str | Path | bytes]] = []
    for f in args.file or []:
        if "=" not in f:
            _err(f"--file {f!r} must be path=local-path")
            return 2
        remote, local = f.split("=", 1)
        if not remote or not local:
            _err(f"--file {f!r} must be path=local-path")
            return 2
        files.append((remote, local))

    install = shlex.split(args.install) if args.install else None
    entrypoint = shlex.split(args.entrypoint) if args.entrypoint else None

    if not files and install is None and entrypoint is None:
        _err("nothing to do; pass at least one of --file, --install, --entrypoint")
        return 2

    def on_log(stream: str, line: str) -> None:
        out = sys.stderr if stream == "stderr" else sys.stdout
        out.write(line)
        if not line.endswith("\n"):
            out.write("\n")

    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        result = sandbox.agent.configure(
            files=files or None,
            install_command=install,
            entrypoint=entrypoint,
            on_log=on_log,
        )
    if args.output == "json":
        _print_json(result)
    else:
        sys.stdout.write(
            f"configure: install exit {result.install_exit_code}, "
            f"hash={result.install_command_hash}, skipped={result.skipped}\n"
        )
    return 0 if result.install_exit_code == 0 else 1


def cmd_invoke(args: argparse.Namespace) -> int:
    if args.cancel:
        with _client(args) as client:
            sandbox = client.get_sandbox(args.sandbox_id)
            sandbox.agent.invoke_cancel(args.cancel)
        sys.stdout.write(f"canceled {args.cancel}\n")
        return 0

    payload: Any = None
    if args.input == "-":
        payload = sys.stdin.buffer.read()
    elif args.input and args.input.startswith("@"):
        payload = Path(args.input[1:]).read_bytes()
    elif args.input:
        payload = args.input

    with _client(args) as client:
        sandbox = client.get_sandbox(args.sandbox_id)
        exit_code = 0
        for event in sandbox.agent.invoke_stream(
            payload,
            prompt=args.prompt,
            invoke_timeout=args.timeout,
        ):
            if event.event == "start":
                invoke_id = event.data.get("invokeId", "")
                if invoke_id:
                    sys.stderr.write(f"invoke started: {invoke_id}\n")
            elif event.event == "log":
                stream = event.data.get("stream", "stdout")
                line = event.data.get("data", "")
                out = sys.stderr if stream == "stderr" else sys.stdout
                out.write(line)
                if line and not line.endswith("\n"):
                    out.write("\n")
            elif event.event == "exit":
                exit_code = int(event.data.get("exitCode", 0))
                duration = event.data.get("durationMs", 0)
                reason = event.data.get("reason", "")
                sys.stderr.write(
                    f"invoke {reason}: exit {exit_code} (duration {duration}ms)\n"
                )
            elif event.event == "error":
                sys.stderr.write(f"invoke error: {event.data.get('message', '')}\n")
                exit_code = 1
    return exit_code


# ----------------------------------------------------------------------
# Governance
# ----------------------------------------------------------------------


def cmd_governance_list(args: argparse.Namespace) -> int:
    with _client(args) as client:
        policies = client.governance.list()
    _print_json(policies)
    return 0


def cmd_governance_get(args: argparse.Namespace) -> int:
    with _client(args) as client:
        policy = client.governance.get(args.namespace)
    _print_json(policy)
    return 0


def cmd_governance_set(args: argparse.Namespace) -> int:
    from agenttier.governance import Policy

    policy_dict = json.loads(Path(args.file).read_text()) if args.file != "-" else json.loads(sys.stdin.read())
    policy = Policy.model_validate(policy_dict)
    with _client(args) as client:
        result = client.governance.set(policy, namespace=args.namespace)
    _print_json(result)
    return 0


def cmd_governance_delete(args: argparse.Namespace) -> int:
    with _client(args) as client:
        client.governance.delete(args.namespace)
    sys.stdout.write(f"deleted policy override for namespace {args.namespace}\n")
    return 0


def cmd_governance_effective(args: argparse.Namespace) -> int:
    with _client(args) as client:
        eff = client.governance.effective(args.namespace)
    _print_json(eff)
    return 0


# ----------------------------------------------------------------------
# Audit
# ----------------------------------------------------------------------


def cmd_audit_list(args: argparse.Namespace) -> int:
    with _client(args) as client:
        events = client.audit.list_events()
    if args.output == "json":
        _print_json(events)
        return 0
    rows = [
        (str(e.timestamp or ""), e.event_type or "", e.sandbox_name or e.sandbox_id or "", e.user_email or "")
        for e in events
    ]
    _print_table(rows, headers=["TIMESTAMP", "EVENT", "SANDBOX", "USER"])
    return 0


# ----------------------------------------------------------------------
# Analytics
# ----------------------------------------------------------------------


def cmd_analytics_usage(args: argparse.Namespace) -> int:
    with _client(args) as client:
        usage = client.analytics.usage()
    _print_json(usage)
    return 0


def cmd_analytics_costs(args: argparse.Namespace) -> int:
    with _client(args) as client:
        costs = client.analytics.costs()
    _print_json(costs)
    return 0


# ----------------------------------------------------------------------
# Admin
# ----------------------------------------------------------------------


def cmd_admin_sandboxes(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sandboxes = client.admin.sandboxes()
    if args.output == "json":
        _print_json(sandboxes)
        return 0
    rows = [(s.sandbox_id, s.name, s.status, s.namespace) for s in sandboxes]
    _print_table(rows, headers=["ID", "NAME", "STATUS", "NAMESPACE"])
    return 0


def cmd_admin_sharing(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sharing = client.admin.sharing()
    _print_json(sharing)
    return 0


# ----------------------------------------------------------------------
# User preferences
# ----------------------------------------------------------------------


def cmd_user_preferences_get(args: argparse.Namespace) -> int:
    with _client(args) as client:
        prefs = client.user.preferences_get()
    _print_json(prefs)
    return 0


def cmd_user_preferences_set(args: argparse.Namespace) -> int:
    prefs = json.loads(Path(args.file).read_text()) if args.file != "-" else json.loads(sys.stdin.read())
    with _client(args) as client:
        stored = client.user.preferences_set(prefs)
    _print_json(stored)
    return 0


# ----------------------------------------------------------------------
# API keys
# ----------------------------------------------------------------------


def cmd_apikeys_list(args: argparse.Namespace) -> int:
    with _client(args) as client:
        keys = client.api_keys.list()
    if args.output == "json":
        _print_json(keys)
        return 0
    rows = [(k.id, k.name or "", k.sandbox_id or "", ",".join(k.action_groups)) for k in keys]
    _print_table(rows, headers=["ID", "NAME", "SANDBOX_ID", "ACTION_GROUPS"])
    return 0


def cmd_apikeys_create(args: argparse.Namespace) -> int:
    action_groups = args.action_group if args.action_group else None
    with _client(args) as client:
        created = client.api_keys.create(
            name=args.name,
            expires_in=args.expires_in,
            sandbox_id=args.sandbox_id,
            action_groups=action_groups,
        )
    if args.output == "json":
        _print_json(created)
    else:
        sys.stdout.write(f"created api key {created.id}: {created.key}\n")
        if created.warning:
            sys.stdout.write(f"{created.warning}\n")
    return 0


def cmd_apikeys_revoke(args: argparse.Namespace) -> int:
    with _client(args) as client:
        client.api_keys.revoke(args.key_id)
    sys.stdout.write(f"revoked api key {args.key_id}\n")
    return 0


# ----------------------------------------------------------------------
# Warm pool
# ----------------------------------------------------------------------


def cmd_warmpool_status(args: argparse.Namespace) -> int:
    with _client(args) as client:
        status = client.warmpool.status()
    _print_json(status)
    return 0


def cmd_warmpool_config(args: argparse.Namespace) -> int:
    from agenttier.warmpool import PoolConfig

    raw = json.loads(Path(args.file).read_text()) if args.file != "-" else json.loads(sys.stdin.read())
    if not isinstance(raw, list):
        _err("warmpool config input must be a JSON array of pool configs")
        return 2
    pools = [PoolConfig.model_validate(p) for p in raw]
    with _client(args) as client:
        result = client.warmpool.set_config(pools)
    _print_json(result)
    return 0


# ----------------------------------------------------------------------
# Cluster
# ----------------------------------------------------------------------


def cmd_cluster_status(args: argparse.Namespace) -> int:
    with _client(args) as client:
        status = client.cluster.status()
    _print_json(status)
    return 0


def cmd_cluster_nodes(args: argparse.Namespace) -> int:
    with _client(args) as client:
        nodes = client.cluster.nodes()
    _print_json(nodes)
    return 0


def cmd_cluster_headroom_get(args: argparse.Namespace) -> int:
    with _client(args) as client:
        cfg = client.cluster.headroom_get()
    _print_json(cfg)
    return 0


def cmd_cluster_headroom_set(args: argparse.Namespace) -> int:
    with _client(args) as client:
        cfg = client.cluster.headroom_set(args.replicas, cpu=args.cpu, memory=args.memory)
    _print_json(cfg)
    return 0


# ----------------------------------------------------------------------
# Webhooks
# ----------------------------------------------------------------------


def cmd_webhooks_create(args: argparse.Namespace) -> int:
    with _client(args) as client:
        sub = client.webhooks.create(
            args.url,
            args.event_type,
            sandbox_id=args.sandbox_id,
            namespace=args.namespace,
        )
    if args.output == "json":
        _print_json(sub)
    else:
        sys.stdout.write(f"created webhook {sub.id}: secret={sub.secret}\n")
        sys.stdout.write("store this secret now — it cannot be retrieved again.\n")
    return 0


def cmd_webhooks_list(args: argparse.Namespace) -> int:
    with _client(args) as client:
        subs = client.webhooks.list()
    if args.output == "json":
        _print_json(subs)
        return 0
    rows = [(s.id, s.url, ",".join(s.event_types), str(s.disabled)) for s in subs]
    _print_table(rows, headers=["ID", "URL", "EVENT_TYPES", "DISABLED"])
    return 0


def cmd_webhooks_delete(args: argparse.Namespace) -> int:
    with _client(args) as client:
        client.webhooks.delete(args.webhook_id)
    sys.stdout.write(f"deleted webhook {args.webhook_id}\n")
    return 0


def cmd_webhooks_deliveries(args: argparse.Namespace) -> int:
    with _client(args) as client:
        deliveries = client.webhooks.deliveries(args.webhook_id)
    if args.output == "json":
        _print_json(deliveries)
        return 0
    rows = [
        (str(d.timestamp or ""), d.event_type, str(d.status_code or ""), str(d.attempt), str(d.success))
        for d in deliveries
    ]
    _print_table(rows, headers=["TIMESTAMP", "EVENT", "STATUS", "ATTEMPT", "SUCCESS"])
    return 0


# ----------------------------------------------------------------------
# Argparse wiring
# ----------------------------------------------------------------------


def _add_global_flags(p: argparse.ArgumentParser) -> None:
    p.add_argument("--api-url", help="Router base URL (env: AGENTTIER_API_URL)")
    p.add_argument("--api-key", help="API key (env: AGENTTIER_API_KEY)")
    p.add_argument("--token", help="Bearer token / OIDC JWT (env: AGENTTIER_TOKEN)")
    p.add_argument(
        "--output",
        choices=["text", "json"],
        default="text",
        help="Output format. Default: text.",
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="agenttier",
        description="Control AgentTier sandboxes from the command line.",
    )
    parser.add_argument("--version", action="version", version=f"agenttier {__version__}")
    sub = parser.add_subparsers(dest="command", required=True, metavar="<command>")

    # version
    p_version = sub.add_parser("version", help="Print the CLI version")
    _add_global_flags(p_version)
    p_version.set_defaults(func=cmd_version)

    # login
    p_login = sub.add_parser("login", help="Save API URL and credentials to ~/.config/agenttier/config.json")
    _add_global_flags(p_login)
    p_login.set_defaults(func=cmd_login)

    # whoami
    p_whoami = sub.add_parser("whoami", help="Print the server's view of the caller's identity")
    _add_global_flags(p_whoami)
    p_whoami.set_defaults(func=cmd_whoami)

    # sandbox <subcommand>
    p_sandbox = sub.add_parser("sandbox", help="Manage sandboxes")
    sub_sandbox = p_sandbox.add_subparsers(dest="subcommand", required=True, metavar="<subcommand>")

    p_list = sub_sandbox.add_parser("list", help="List sandboxes")
    _add_global_flags(p_list)
    p_list.add_argument("--namespace", help="Filter by namespace")
    p_list.add_argument("--status", help="Filter by status (Running, Stopped, ...)")
    p_list.set_defaults(func=cmd_sandbox_list)

    p_get = sub_sandbox.add_parser("get", help="Show one sandbox")
    _add_global_flags(p_get)
    p_get.add_argument("sandbox_id")
    p_get.set_defaults(func=cmd_sandbox_get)

    p_create = sub_sandbox.add_parser("create", help="Create a sandbox from a template")
    _add_global_flags(p_create)
    p_create.add_argument("name", help="Sandbox name (DNS-friendly)")
    p_create.add_argument("--template", required=True, help="ClusterSandboxTemplate name")
    p_create.add_argument("--namespace", default="default")
    p_create.add_argument("--timeout", help='Max-runtime duration (e.g. "8h")')
    p_create.add_argument("--idle-timeout", dest="idle_timeout", help='Idle timeout (e.g. "30m")')
    p_create.add_argument("--storage-size", dest="storage_size", help='PVC size (e.g. "10Gi")')
    p_create.add_argument("--wait", action="store_true", help="Block until Running")
    p_create.add_argument(
        "--wait-timeout",
        dest="wait_timeout",
        type=int,
        default=180,
        help="Wait timeout in seconds (default: 180)",
    )
    p_create.set_defaults(func=cmd_sandbox_create)

    p_stop = sub_sandbox.add_parser("stop", help="Stop a sandbox (preserve PVC)")
    _add_global_flags(p_stop)
    p_stop.add_argument("sandbox_id")
    p_stop.set_defaults(func=cmd_sandbox_stop)

    p_resume = sub_sandbox.add_parser("resume", help="Resume a stopped sandbox")
    _add_global_flags(p_resume)
    p_resume.add_argument("sandbox_id")
    p_resume.set_defaults(func=cmd_sandbox_resume)

    p_delete = sub_sandbox.add_parser("delete", help="Delete a sandbox (permanent)")
    _add_global_flags(p_delete)
    p_delete.add_argument("sandbox_id")
    p_delete.set_defaults(func=cmd_sandbox_delete)

    p_clone = sub_sandbox.add_parser("clone", help="Clone a sandbox via VolumeSnapshot")
    _add_global_flags(p_clone)
    p_clone.add_argument("sandbox_id", help="ID of the source sandbox")
    p_clone.add_argument("--name", help="Name for the cloned sandbox (default: <source>-clone-<ts>)")
    p_clone.add_argument(
        "--snapshot-class",
        dest="snapshot_class",
        help="Override the cluster's default VolumeSnapshotClass (advanced)",
    )
    p_clone.set_defaults(func=cmd_sandbox_clone)

    p_exec = sub_sandbox.add_parser(
        "exec",
        help="Run a one-shot command in a sandbox",
    )
    _add_global_flags(p_exec)
    p_exec.add_argument("sandbox_id")
    p_exec.add_argument("--timeout", type=int, default=30, help="Command timeout (seconds)")
    p_exec.add_argument("command", nargs=argparse.REMAINDER, help="Command and args (after --)")
    p_exec.set_defaults(func=cmd_sandbox_exec)

    p_wait = sub_sandbox.add_parser("wait", help="Block until sandbox is Running")
    _add_global_flags(p_wait)
    p_wait.add_argument("sandbox_id")
    p_wait.add_argument("--timeout", type=int, default=180)
    p_wait.set_defaults(func=cmd_sandbox_wait)

    p_patch = sub_sandbox.add_parser("patch", help="Live-mutate a running sandbox (FR2)")
    _add_global_flags(p_patch)
    p_patch.add_argument("sandbox_id")
    p_patch.add_argument("--idle-timeout", dest="idle_timeout", help='New idle timeout (e.g. "30m")')
    p_patch.add_argument("--cpu-request", dest="cpu_request", help='Resource request CPU (e.g. "1")')
    p_patch.add_argument("--cpu-limit", dest="cpu_limit", help='Resource limit CPU (e.g. "2")')
    p_patch.add_argument("--memory-request", dest="memory_request", help='Resource request memory (e.g. "2Gi")')
    p_patch.add_argument("--memory-limit", dest="memory_limit", help='Resource limit memory (e.g. "4Gi")')
    p_patch.add_argument(
        "--label", action="append", default=[], help='Set a label: "key=value". Repeatable.'
    )
    p_patch.add_argument(
        "--annotation", action="append", default=[], help='Set an annotation: "key=value". Repeatable.'
    )
    p_patch.set_defaults(func=cmd_sandbox_patch)

    p_bc = sub_sandbox.add_parser(
        "bulk-create", help="Create multiple sandboxes from a JSON array of specs (FR4)"
    )
    _add_global_flags(p_bc)
    p_bc.add_argument("--file", required=True, help='JSON array of create specs; "-" reads stdin')
    p_bc.set_defaults(func=cmd_sandbox_bulk_create)

    p_ba = sub_sandbox.add_parser(
        "bulk-action", help="Apply stop/resume/delete to multiple sandbox IDs (FR4)"
    )
    _add_global_flags(p_ba)
    p_ba.add_argument("--action", required=True, choices=["stop", "resume", "delete"])
    p_ba.add_argument("ids", nargs="+", help="Sandbox IDs")
    p_ba.set_defaults(func=cmd_sandbox_bulk_action)

    # sandbox sharing
    p_sharing = sub_sandbox.add_parser("sharing", help="Manage sandbox sharing")
    sub_sharing = p_sharing.add_subparsers(dest="sharing_subcommand", required=True, metavar="<subcommand>")

    p_shl = sub_sharing.add_parser("list", help="List sharing grants for a sandbox")
    _add_global_flags(p_shl)
    p_shl.add_argument("sandbox_id")
    p_shl.set_defaults(func=cmd_sharing_list)

    p_shg = sub_sharing.add_parser("grant", help="Grant access to a user or group")
    _add_global_flags(p_shg)
    p_shg.add_argument("sandbox_id")
    p_shg.add_argument("identity")
    p_shg.add_argument("--level", default="viewer", choices=["viewer", "collaborator"])
    p_shg.add_argument("--kind", default="user", choices=["user", "group"])
    p_shg.set_defaults(func=cmd_sharing_grant)

    p_shr = sub_sharing.add_parser("revoke", help="Revoke a previously granted identity")
    _add_global_flags(p_shr)
    p_shr.add_argument("sandbox_id")
    p_shr.add_argument("identity")
    p_shr.set_defaults(func=cmd_sharing_revoke)

    p_shc = sub_sharing.add_parser("create-link", help="Mint an expiring share link")
    _add_global_flags(p_shc)
    p_shc.add_argument("sandbox_id")
    p_shc.add_argument("--level", default="viewer", choices=["viewer", "collaborator"])
    p_shc.add_argument("--expires-in", dest="expires_in", help='Go duration (e.g. "24h")')
    p_shc.add_argument("--max-uses", dest="max_uses", type=int, default=0, help="0 = unlimited")
    p_shc.set_defaults(func=cmd_sharing_create_link)

    # sandbox backups
    p_backups = sub_sandbox.add_parser("backups", help="Manage sandbox backups (FR3)")
    sub_backups = p_backups.add_subparsers(dest="backups_subcommand", required=True, metavar="<subcommand>")

    p_bl = sub_backups.add_parser("list", help="List backup + clone snapshots")
    _add_global_flags(p_bl)
    p_bl.add_argument("sandbox_id")
    p_bl.set_defaults(func=cmd_backups_list)

    p_bcr = sub_backups.add_parser("create", help="Trigger an on-demand backup")
    _add_global_flags(p_bcr)
    p_bcr.add_argument("sandbox_id")
    p_bcr.add_argument("--snapshot-class", dest="snapshot_class", help="Override default VolumeSnapshotClass")
    p_bcr.set_defaults(func=cmd_backups_create)

    p_bres = sub_backups.add_parser("restore", help="Restore a sandbox from a backup snapshot")
    _add_global_flags(p_bres)
    p_bres.add_argument("sandbox_id")
    p_bres.add_argument("snapshot_name")
    p_bres.add_argument("--name", help="Name for the restored sandbox")
    p_bres.set_defaults(func=cmd_backups_restore)

    p_bdel = sub_backups.add_parser("delete", help="Prune a backup snapshot")
    _add_global_flags(p_bdel)
    p_bdel.add_argument("sandbox_id")
    p_bdel.add_argument("snapshot_name")
    p_bdel.set_defaults(func=cmd_backups_delete)

    # sandbox files
    p_files = sub_sandbox.add_parser("files", help="File operations on a sandbox")
    sub_files = p_files.add_subparsers(dest="files_subcommand", required=True, metavar="<subcommand>")

    p_ls = sub_files.add_parser("ls", help="List files in a sandbox directory")
    _add_global_flags(p_ls)
    p_ls.add_argument("sandbox_id")
    p_ls.add_argument("--path", default="/workspace")
    p_ls.set_defaults(func=cmd_files_ls)

    p_cat = sub_files.add_parser("cat", help="Stream a sandbox file to stdout")
    _add_global_flags(p_cat)
    p_cat.add_argument("sandbox_id")
    p_cat.add_argument("path")
    p_cat.set_defaults(func=cmd_files_cat)

    p_up = sub_files.add_parser("upload", help="Upload a local file into a sandbox")
    _add_global_flags(p_up)
    p_up.add_argument("sandbox_id")
    p_up.add_argument("local", help="Local path to upload")
    p_up.add_argument("remote", help="Sandbox destination path (e.g. /workspace/script.py)")
    p_up.set_defaults(func=cmd_files_upload)

    p_dn = sub_files.add_parser("download", help="Download a sandbox file to a local path")
    _add_global_flags(p_dn)
    p_dn.add_argument("sandbox_id")
    p_dn.add_argument("remote", help="Sandbox source path")
    p_dn.add_argument("local", help="Local destination path")
    p_dn.set_defaults(func=cmd_files_download)

    p_arc = sub_files.add_parser(
        "archive",
        help="Download a sandbox directory as a streamed .zip (default /workspace)",
    )
    _add_global_flags(p_arc)
    p_arc.add_argument("sandbox_id")
    p_arc.add_argument(
        "-o", "--dest", dest="dest", required=True, help="Local path for the .zip output"
    )
    p_arc.add_argument(
        "--path",
        default="/workspace",
        help="Sandbox directory to archive (must live under /workspace)",
    )
    p_arc.set_defaults(func=cmd_files_archive)

    p_wr = sub_files.add_parser("write", help="Write inline content into a sandbox file")
    _add_global_flags(p_wr)
    p_wr.add_argument("sandbox_id")
    p_wr.add_argument("path", help="Sandbox destination path")
    p_wr.add_argument("--data", help="Inline content (utf-8)")
    p_wr.add_argument("--file", help='Local file path; "-" reads stdin')
    p_wr.set_defaults(func=cmd_files_write)

    # sandbox ports
    p_ports = sub_sandbox.add_parser("ports", help="Port forwarding")
    sub_ports = p_ports.add_subparsers(dest="ports_subcommand", required=True, metavar="<subcommand>")

    p_pl = sub_ports.add_parser("list", help="List forwarded ports")
    _add_global_flags(p_pl)
    p_pl.add_argument("sandbox_id")
    p_pl.set_defaults(func=cmd_ports_list)

    p_pf = sub_ports.add_parser("forward", help="Forward a port out of the sandbox")
    _add_global_flags(p_pf)
    p_pf.add_argument("sandbox_id")
    p_pf.add_argument("--port", type=int, required=True)
    p_pf.add_argument("--protocol", default="http", choices=["http", "tcp"])
    p_pf.set_defaults(func=cmd_ports_forward)

    p_pr = sub_ports.add_parser("remove", help="Remove a port forward")
    _add_global_flags(p_pr)
    p_pr.add_argument("sandbox_id")
    p_pr.add_argument("--port", type=int, required=True)
    p_pr.set_defaults(func=cmd_ports_remove)

    # template <subcommand>
    p_tmpl = sub.add_parser("template", help="Manage sandbox templates")
    sub_tmpl = p_tmpl.add_subparsers(dest="subcommand", required=True, metavar="<subcommand>")

    p_tl = sub_tmpl.add_parser("list", help="List ClusterSandboxTemplates")
    _add_global_flags(p_tl)
    p_tl.set_defaults(func=cmd_template_list)

    p_tg = sub_tmpl.add_parser("get", help="Show one ClusterSandboxTemplate")
    _add_global_flags(p_tg)
    p_tg.add_argument("name")
    p_tg.set_defaults(func=cmd_template_get)

    # configure / invoke (top-level so they mirror the Go CLI)
    p_cfg = sub.add_parser(
        "configure",
        help="Configure an agent-mode sandbox (upload files, run install, set entrypoint)",
    )
    _add_global_flags(p_cfg)
    p_cfg.add_argument("sandbox_id")
    p_cfg.add_argument(
        "--file",
        action="append",
        help='Upload a file: "remote=local". Repeatable.',
    )
    p_cfg.add_argument(
        "--install",
        help='Install command (e.g. "pip install -r requirements.txt")',
    )
    p_cfg.add_argument(
        "--entrypoint",
        help='Entrypoint command (e.g. "python /workspace/agent.py")',
    )
    p_cfg.set_defaults(func=cmd_configure)

    p_inv = sub.add_parser(
        "invoke",
        help="Invoke an agent-mode sandbox and stream output",
    )
    _add_global_flags(p_inv)
    p_inv.add_argument("sandbox_id")
    p_inv.add_argument("--prompt", help="Convenience prompt forwarded as --prompt query param")
    p_inv.add_argument(
        "--input",
        help='Body forwarded to entrypoint stdin. Inline string, "@/path/to/file", or "-" for stdin.',
    )
    p_inv.add_argument(
        "--timeout",
        help='Server-side per-invoke timeout (e.g. "5m"). Defaults to template setting.',
    )
    p_inv.add_argument("--cancel", help="Cancel an in-flight invoke by ID")
    p_inv.set_defaults(func=cmd_invoke)

    # governance <subcommand>
    p_gov = sub.add_parser("governance", help="Manage governance policies")
    sub_gov = p_gov.add_subparsers(dest="subcommand", required=True, metavar="<subcommand>")

    p_gl = sub_gov.add_parser("list", help="List the cluster default policy + namespace overrides")
    _add_global_flags(p_gl)
    p_gl.set_defaults(func=cmd_governance_list)

    p_gg = sub_gov.add_parser("get", help="Show the raw policy stored for a namespace")
    _add_global_flags(p_gg)
    p_gg.add_argument("namespace")
    p_gg.set_defaults(func=cmd_governance_get)

    p_gs = sub_gov.add_parser("set", help="Create or replace a policy from a JSON file")
    _add_global_flags(p_gs)
    p_gs.add_argument("--file", required=True, help='Policy JSON; "-" reads stdin')
    p_gs.add_argument("--namespace", help="Omit to set the cluster-wide default")
    p_gs.set_defaults(func=cmd_governance_set)

    p_gd = sub_gov.add_parser("delete", help="Delete a namespace-specific policy override")
    _add_global_flags(p_gd)
    p_gd.add_argument("namespace")
    p_gd.set_defaults(func=cmd_governance_delete)

    p_ge = sub_gov.add_parser("effective", help="Show the fully-resolved policy for a namespace")
    _add_global_flags(p_ge)
    p_ge.add_argument("namespace", nargs="?", help="Defaults to the Router's configured default namespace")
    p_ge.set_defaults(func=cmd_governance_effective)

    # audit <subcommand>
    p_audit = sub.add_parser("audit", help="View the activity log (admin)")
    sub_audit = p_audit.add_subparsers(dest="subcommand", required=True, metavar="<subcommand>")

    p_al = sub_audit.add_parser("list", help="List recent audit events")
    _add_global_flags(p_al)
    p_al.set_defaults(func=cmd_audit_list)

    # analytics <subcommand>
    p_an = sub.add_parser("analytics", help="Fleet-wide usage/cost analytics (admin)")
    sub_an = p_an.add_subparsers(dest="subcommand", required=True, metavar="<subcommand>")

    p_anu = sub_an.add_parser("usage", help="Fleet-wide usage summary")
    _add_global_flags(p_anu)
    p_anu.set_defaults(func=cmd_analytics_usage)

    p_anc = sub_an.add_parser("costs", help="Fleet-wide cost estimate")
    _add_global_flags(p_anc)
    p_anc.set_defaults(func=cmd_analytics_costs)

    # admin <subcommand>
    p_admin = sub.add_parser("admin", help="Fleet-wide admin views")
    sub_admin = p_admin.add_subparsers(dest="subcommand", required=True, metavar="<subcommand>")

    p_ads = sub_admin.add_parser("sandboxes", help="Fleet-wide sandbox list (admin)")
    _add_global_flags(p_ads)
    p_ads.set_defaults(func=cmd_admin_sandboxes)

    p_adsh = sub_admin.add_parser("sharing", help="Fleet-wide sharing overview (admin)")
    _add_global_flags(p_adsh)
    p_adsh.set_defaults(func=cmd_admin_sharing)

    # user <subcommand>
    p_user = sub.add_parser("user", help="Manage user preferences")
    sub_user = p_user.add_subparsers(dest="subcommand", required=True, metavar="<subcommand>")

    p_upg = sub_user.add_parser("preferences-get", help="Show the caller's saved preferences")
    _add_global_flags(p_upg)
    p_upg.set_defaults(func=cmd_user_preferences_get)

    p_ups = sub_user.add_parser("preferences-set", help="Replace the caller's preferences")
    _add_global_flags(p_ups)
    p_ups.add_argument("--file", required=True, help='Preferences JSON; "-" reads stdin')
    p_ups.set_defaults(func=cmd_user_preferences_set)

    # apikeys <subcommand>
    p_ak = sub.add_parser("apikeys", help="Manage API keys")
    sub_ak = p_ak.add_subparsers(dest="subcommand", required=True, metavar="<subcommand>")

    p_akl = sub_ak.add_parser("list", help="List the caller's API keys")
    _add_global_flags(p_akl)
    p_akl.set_defaults(func=cmd_apikeys_list)

    p_akc = sub_ak.add_parser("create", help="Mint a new API key (plaintext shown once)")
    _add_global_flags(p_akc)
    p_akc.add_argument("--name", help="Human-readable label")
    p_akc.add_argument("--expires-in", dest="expires_in", help='Go duration (e.g. "720h")')
    p_akc.add_argument(
        "--sandbox-id",
        dest="sandbox_id",
        help="Mint a sandbox-scoped key bound to this sandbox instead of a full-access key",
    )
    p_akc.add_argument(
        "--action-group",
        dest="action_group",
        action="append",
        help="Action group for a scoped key (requires --sandbox-id). Repeatable.",
    )
    p_akc.set_defaults(func=cmd_apikeys_create)

    p_akr = sub_ak.add_parser("revoke", help="Revoke an API key by its ID")
    _add_global_flags(p_akr)
    p_akr.add_argument("key_id")
    p_akr.set_defaults(func=cmd_apikeys_revoke)

    # warmpool <subcommand>
    p_wp = sub.add_parser("warmpool", help="Manage the warm pool (admin write)")
    sub_wp = p_wp.add_subparsers(dest="subcommand", required=True, metavar="<subcommand>")

    p_wps = sub_wp.add_parser("status", help="Show warm-pool status across all templates")
    _add_global_flags(p_wps)
    p_wps.set_defaults(func=cmd_warmpool_status)

    p_wpc = sub_wp.add_parser("config", help="Replace the warm-pool configuration")
    _add_global_flags(p_wpc)
    p_wpc.add_argument("--file", required=True, help='JSON array of pool configs; "-" reads stdin')
    p_wpc.set_defaults(func=cmd_warmpool_config)

    # cluster <subcommand>
    p_cl = sub.add_parser("cluster", help="Cluster status and headroom")
    sub_cl = p_cl.add_subparsers(dest="subcommand", required=True, metavar="<subcommand>")

    p_cls = sub_cl.add_parser("status", help="Node + pod headcount glance")
    _add_global_flags(p_cls)
    p_cls.set_defaults(func=cmd_cluster_status)

    p_cln = sub_cl.add_parser("nodes", help="Per-node capacity/usage detail (admin)")
    _add_global_flags(p_cln)
    p_cln.set_defaults(func=cmd_cluster_nodes)

    p_clhg = sub_cl.add_parser("headroom-get", help="Show the spare-capacity headroom config")
    _add_global_flags(p_clhg)
    p_clhg.set_defaults(func=cmd_cluster_headroom_get)

    p_clhs = sub_cl.add_parser("headroom-set", help="Update headroom replicas/cpu/memory (admin)")
    _add_global_flags(p_clhs)
    p_clhs.add_argument("--replicas", type=int, required=True)
    p_clhs.add_argument("--cpu", help="Per-replica CPU (leave unset to keep current)")
    p_clhs.add_argument("--memory", help="Per-replica memory (leave unset to keep current)")
    p_clhs.set_defaults(func=cmd_cluster_headroom_set)

    # webhooks <subcommand>
    p_wh = sub.add_parser("webhooks", help="Manage webhook subscriptions")
    sub_wh = p_wh.add_subparsers(dest="subcommand", required=True, metavar="<subcommand>")

    p_whc = sub_wh.add_parser("create", help="Register a webhook subscription (secret shown once)")
    _add_global_flags(p_whc)
    p_whc.add_argument("--url", required=True, help="Receiver URL (must be https://)")
    p_whc.add_argument(
        "--event-type",
        dest="event_type",
        action="append",
        required=True,
        help="Event type to subscribe to. Repeatable.",
    )
    p_whc.add_argument("--sandbox-id", dest="sandbox_id", help="Scope delivery to one sandbox")
    p_whc.add_argument("--namespace", help="Scope delivery to one namespace")
    p_whc.set_defaults(func=cmd_webhooks_create)

    p_whl = sub_wh.add_parser("list", help="List the caller's own webhook subscriptions")
    _add_global_flags(p_whl)
    p_whl.set_defaults(func=cmd_webhooks_list)

    p_whd = sub_wh.add_parser("delete", help="Delete a webhook subscription")
    _add_global_flags(p_whd)
    p_whd.add_argument("webhook_id")
    p_whd.set_defaults(func=cmd_webhooks_delete)

    p_whdl = sub_wh.add_parser("deliveries", help="Show recent delivery attempts (debugging)")
    _add_global_flags(p_whdl)
    p_whdl.add_argument("webhook_id")
    p_whdl.set_defaults(func=cmd_webhooks_deliveries)

    return parser


# ----------------------------------------------------------------------
# Entry point
# ----------------------------------------------------------------------


def main(argv: Optional[Sequence[str]] = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    func: Callable[[argparse.Namespace], int] = args.func
    try:
        return func(args)
    except KeyboardInterrupt:
        sys.stderr.write("\nagenttier: interrupted\n")
        return 130
    except AgentTierError as e:
        _err(str(e))
        return 1


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
