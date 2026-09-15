"""Per-account config directories, so a pinned session knows who it is.

Passing CLAUDE_CODE_OAUTH_TOKEN authenticates a session correctly but leaves it
anonymous: `claude auth status` reports no email and no organisation, so every
pinned session looks identical to the default one. Identity is only reported when
the session reads credentials from a file in its own config directory.

These directories are per account and persistent, never throwaway. A session
refreshes its own access token, and refresh rotates the refresh token; a
temporary directory would discard the rotated value and leave the stored copy
superseded, which locks the account out until someone signs in through a browser.
One stable directory per account means one writer per account.

Everything that is not identity or credentials is symlinked back to the shared
config, so settings, skills, hooks and project history stay in one place.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
import tempfile

#: Linked rather than copied, so edits in the shared config apply everywhere.
SHARED_ENTRIES = (
    "settings.json", "settings.local.json", "CLAUDE.md", "commands", "hooks",
    "skills", "plugins", "agents", "projects", "sessions", "history.jsonl",
    "file-history", "statsig", "ide", "todos",
)


def accounts_root() -> Path:
    return Path.home() / ".claude-accounts"


def shared_root() -> Path:
    override = os.environ.get("CLAUDE_CONFIG_DIR")
    return Path(override) if override else Path.home() / ".claude"


def _read_json(path: Path) -> dict:
    try:
        data = json.loads(path.read_text())
        return data if isinstance(data, dict) else {}
    except (OSError, ValueError):
        return {}


def _write_private(path: Path, payload: str) -> None:
    """Replace a file atomically, never leaving a readable partial behind."""
    tmp = None
    try:
        with tempfile.NamedTemporaryFile("w", dir=path.parent, delete=False) as handle:
            tmp = Path(handle.name)
            handle.write(payload)
            handle.flush()
            os.fsync(handle.fileno())
        tmp.chmod(0o600)
        os.replace(tmp, path)
        tmp = None
    finally:
        if tmp is not None:
            tmp.unlink(missing_ok=True)


def _link_shared(target: Path, shared: Path) -> None:
    for name in SHARED_ENTRIES:
        source = shared / name
        link = target / name
        if not source.exists() or link.is_symlink():
            continue
        if link.exists():
            continue  # a real file here was put there deliberately; leave it
        try:
            link.symlink_to(source)
        except OSError as exc:
            print(f"hotseat: could not link {name} into the session directory: {exc}")


def _expires_at(credentials: dict) -> int:
    try:
        return int(credentials.get("claudeAiOauth", {}).get("expiresAt") or 0)
    except (TypeError, ValueError):
        return 0


def ensure(account, credentials: dict, identity: dict | None = None,
           shared: Path | None = None, root: Path | None = None) -> Path:
    """Create or update the config directory for one account and return it."""
    shared = shared or shared_root()
    root = root or accounts_root()
    root.mkdir(parents=True, exist_ok=True)
    root.chmod(0o700)

    target = root / account.alias
    target.mkdir(parents=True, exist_ok=True)
    target.chmod(0o700)

    # Check the old identity before changing either the metadata or credentials.
    # A future expiry on an unrelated/dummy token must not win over this account.
    old_config = _read_json(target / ".claude.json")
    old_identity = old_config.get("oauthAccount") or {}
    old_credentials = _read_json(target / ".credentials.json")
    if old_credentials and identity:
        for field in ("emailAddress", "organizationUuid"):
            old, new = old_identity.get(field), identity.get(field)
            if old and new and old != new:
                raise ValueError("Pinned session directory belongs to another identity; restore it before launching")

    _link_shared(target, shared)

    # Start from the shared configuration so servers and project settings carry
    # over, then replace only the identity.
    config_path = target / ".claude.json"
    shared_config = (Path.home() / ".claude.json" if shared == Path.home() / ".claude"
                     else shared / ".claude.json")
    config = _read_json(config_path) or _read_json(shared_config)
    if identity:
        config["oauthAccount"] = identity
    _write_private(config_path, json.dumps(config, indent=2))

    # Never move an account backwards: the copy already here may have been
    # rotated by a running session, which supersedes whatever the caller holds.
    creds_path = target / ".credentials.json"
    existing = _read_json(creds_path)
    if _expires_at(existing) <= _expires_at(credentials):
        _write_private(creds_path, json.dumps(credentials, indent=2))
    return target
