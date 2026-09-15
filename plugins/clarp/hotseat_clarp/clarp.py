"""Clarp agents, and which Claude account each one is spending.

Clarp keeps a registry of named agent sessions and starts a backend process for one
only when it has work. So a session is usually a definition rather than a running
process, and "is it live" is a separate question from "does it exist".

Two things matter here that nothing else reports:

  * Agents on the `claude` backend draw on the same account quota as everything
    else, but they are invisible in a normal process listing until they are busy.
  * A running agent's account is decided by the config directory in its
    environment, so a pinned agent can be attributed exactly.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
import re
import subprocess

CLARP_ADMIN = "clarp-admin"
CLAUDE_BACKEND = "claude"
TIMEOUT_S = 30
#: The session slug an agent process carries in its own system prompt.
SESSION_IN_PROMPT = re.compile(r"session id for this agent is .([A-Za-z0-9._-]+)")
AGENT_BINARIES = {"claude", "codex", "node"}


class ClarpError(RuntimeError):
    """Clarp is not installed here, or its session registry could not be read."""


def available() -> bool:
    from shutil import which
    return which(CLARP_ADMIN) is not None


def sessions() -> list[dict]:
    """The registry: every defined agent, running or not."""
    if not available():
        raise ClarpError(
            "clarp-admin is not on PATH, so there is no Clarp installation to read.")
    try:
        result = subprocess.run([CLARP_ADMIN, "sessions"], capture_output=True,
                                text=True, timeout=TIMEOUT_S, check=False)
    except (OSError, subprocess.SubprocessError) as exc:
        raise ClarpError(f"could not run {CLARP_ADMIN}: {exc}") from exc
    if result.returncode != 0:
        raise ClarpError((result.stderr or "clarp-admin failed").strip()[:200])
    try:
        rows = json.loads(result.stdout or "[]")
    except ValueError as exc:
        raise ClarpError("clarp-admin returned output that was not JSON") from exc
    return [row for row in rows if isinstance(row, dict)]


def _config_dir_of(pid: str) -> str | None:
    try:
        raw = Path(f"/proc/{pid}/environ").read_bytes().decode(errors="replace")
    except OSError:
        return None
    for entry in raw.split("\0"):
        if entry.startswith("CLAUDE_CONFIG_DIR="):
            return entry.split("=", 1)[1]
    return None


def live_agents() -> dict[str, dict]:
    """Running agent processes, keyed by the session slug they carry.

    Only real agent binaries count. A shell whose command line merely mentions an
    agent is not one, and neither is this process.
    """
    try:
        listing = subprocess.run(["ps", "-eo", "pid=,args="], capture_output=True,
                                 text=True, timeout=TIMEOUT_S, check=False).stdout
    except (OSError, subprocess.SubprocessError):
        return {}

    mine = str(os.getpid())
    found: dict[str, dict] = {}
    for line in listing.splitlines():
        parts = line.strip().split(None, 1)
        if len(parts) != 2:
            continue
        pid, args = parts
        if pid == mine:
            continue
        head = args.split()[0] if args.split() else ""
        if os.path.basename(head) not in AGENT_BINARIES:
            continue
        match = SESSION_IN_PROMPT.search(args)
        if not match:
            continue
        config_dir = _config_dir_of(pid)
        found[match.group(1)] = {
            "pid": int(pid),
            "config_dir": config_dir,
            "account": Path(config_dir).name if config_dir else None,
        }
    return found




def overview(default_alias: str | None = None) -> dict:
    """Registry and liveness together, with the account each live agent is using."""
    rows = sessions()
    running = live_agents()

    agents = []
    for row in rows:
        slug = row.get("session") or ""
        live = running.get(slug)
        is_claude = row.get("backend") == CLAUDE_BACKEND
        agents.append({
            "session": slug,
            "persona": row.get("persona"),
            "backend": row.get("backend"),
            "cwd": row.get("cwd"),
            "live": live is not None,
            "pid": (live or {}).get("pid"),
            # Only a running agent is actually spending an account. An idle one
            # would use the machine default, which is a different claim and is
            # reported separately rather than dressed up as current usage.
            "account": (live or {}).get("account") if is_claude else None,
            "would_use": (default_alias if is_claude and not live else None),
            "pinned": bool(live and live.get("config_dir")),
        })

    by_backend: dict[str, int] = {}
    for agent in agents:
        key = agent["backend"] or "unknown"
        by_backend[key] = by_backend.get(key, 0) + 1

    claude_agents = [a for a in agents if a["backend"] == CLAUDE_BACKEND]
    by_account: dict[str, int] = {}
    for agent in claude_agents:
        if agent["live"]:
            # A live agent with no config directory is on the shared default.
            key = agent["account"] or default_alias or "default"
            by_account[key] = by_account.get(key, 0) + 1

    return {
        "agents": sorted(agents, key=lambda a: (not a["live"], a["session"])),
        "total": len(agents),
        "live": sum(1 for a in agents if a["live"]),
        "by_backend": by_backend,
        "claude_backed": len(claude_agents),
        "live_by_account": by_account,
    }
