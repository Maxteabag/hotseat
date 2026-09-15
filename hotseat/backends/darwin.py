"""macOS: Claude Code keeps OAuth credentials in the login Keychain.

There is no profile store to enumerate, so this backend reports the one signed-in
account and declares itself read-only. Writing Keychain items from a dashboard is
deliberately out of scope: a mistake there costs the user their login.

The Keychain item is addressed exactly as the CLI addresses it. With the default
config directory the service is "Claude Code-credentials"; a custom
CLAUDE_CONFIG_DIR appends a short hash of that path.
"""

from __future__ import annotations

import getpass
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess

from .base import Account, Backend, BackendError, read_json

SERVICE_BASE = "Claude Code"
SERVICE_SUFFIX = "-credentials"
SAFE_ACCOUNT = re.compile(r"^[a-zA-Z0-9._-]+$")


def keychain_service() -> str:
    """Mirror how the CLI derives its Keychain service name."""
    secure_dir = os.environ.get("CLAUDE_SECURESTORAGE_CONFIG_DIR")
    if secure_dir is not None:
        scoped = bool(secure_dir)
        root = secure_dir or str(Path.home() / ".claude")
    else:
        scoped = bool(os.environ.get("CLAUDE_CONFIG_DIR"))
        root = os.environ.get("CLAUDE_CONFIG_DIR") or str(Path.home() / ".claude")
    service = f"{SERVICE_BASE}{SERVICE_SUFFIX}"
    if scoped:
        digest = hashlib.sha256(root.encode()).hexdigest()[:8]
        service = f"{service}-{digest}"
    return service


def keychain_account() -> str:
    try:
        name = os.environ.get("USER") or getpass.getuser()
    except (OSError, KeyError):
        return "claude-code-user"
    return name if SAFE_ACCOUNT.match(name or "") else "claude-code-user"


class DarwinBackend(Backend):
    name = "keychain"
    has_profiles = False
    can_switch = False

    def _from_keychain(self) -> dict | None:
        try:
            result = subprocess.run(
                ["security", "find-generic-password",
                 "-a", keychain_account(), "-w", "-s", keychain_service()],
                capture_output=True, text=True, timeout=20, check=False)
        except (OSError, subprocess.SubprocessError) as exc:
            raise BackendError(f"could not query the Keychain: {exc}") from exc
        if result.returncode != 0:
            return None
        try:
            return json.loads(result.stdout.strip())
        except ValueError as exc:
            raise BackendError("the Keychain entry was not valid JSON") from exc

    def accounts(self) -> list[Account]:
        stored = self._from_keychain()
        if stored is None:
            # Keychain can be unavailable (locked, or a headless session), and the
            # CLI then falls back to a file. Try that before giving up.
            stored = read_json(Path.home() / ".claude" / ".credentials.json")
        oauth = (stored or {}).get("claudeAiOauth")
        if not oauth or not oauth.get("accessToken"):
            raise BackendError(
                "No Claude credentials found in the Keychain or the config directory. "
                "Sign in with `claude auth login` first.")
        identity = (read_json(Path.home() / ".claude.json") or {}).get("oauthAccount") or {}
        return [Account(
            alias="(signed in)",
            email=identity.get("emailAddress"),
            org=identity.get("organizationName"),
            org_uuid=identity.get("organizationUuid"),
            plan=oauth.get("subscriptionType"),
            rate_limit_tier=oauth.get("rateLimitTier"),
            is_active=True,
            access_expires_at=oauth.get("expiresAt"),
            refresh_expires_at=oauth.get("refreshTokenExpiresAt"),
            token=oauth.get("accessToken"),
        )]
