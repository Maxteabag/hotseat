"""A one-line account indicator for the Claude Code status line.

A pinned session is easy to mistake for the default one, which is the whole
reason the launch behaviour had to change. This makes the answer visible inside
the session itself.

It runs on every status line refresh, so it only reads local files. No network,
no subprocesses, and any failure degrades to printing nothing rather than
breaking the status line.
"""

from __future__ import annotations

import json
import os
from pathlib import Path

from .sessiondirs import accounts_root


def _identity(config_dir: Path) -> dict:
    """Find the identity for this session.

    A session with its own config directory keeps `.claude.json` inside it. The
    default configuration keeps it beside the directory, in the home folder, so
    both locations have to be tried.
    """
    for candidate in (config_dir / ".claude.json", Path.home() / ".claude.json"):
        try:
            data = json.loads(candidate.read_text())
        except (OSError, ValueError):
            continue
        account = data.get("oauthAccount")
        if isinstance(account, dict) and account:
            return account
    return {}


def describe(config_dir: Path | None = None) -> str:
    """Return a short label for the account this session is using."""
    if config_dir is None:
        override = os.environ.get("CLAUDE_CONFIG_DIR")
        config_dir = Path(override) if override else Path.home() / ".claude"

    identity = _identity(config_dir)
    email = identity.get("emailAddress")

    try:
        pinned = config_dir.resolve().parent == accounts_root().resolve()
    except OSError:
        pinned = False
    alias = config_dir.name if pinned else None

    if alias and email:
        return f"{alias} · {email}"
    if alias:
        return alias
    if email:
        # The shared configuration, so this session follows the machine default.
        return f"default · {email}"
    return ""


def render(config_dir: Path | None = None) -> str:
    label = describe(config_dir)
    return f"◆ {label}" if label else ""
