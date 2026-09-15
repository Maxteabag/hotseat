"""Linux and other file-based installs: credentials live in the config directory.

Two sources are merged. The signed-in account always appears. Saved profiles, if
the machine has any, appear alongside it. An account is matched to a profile by
email *and* organisation: the same address can belong to several organisations,
and treating those as one account is how credentials get overwritten.
"""

from __future__ import annotations

import os
from pathlib import Path

from .base import Account, Backend, read_json


def config_dir() -> Path:
    override = os.environ.get("CLAUDE_CONFIG_DIR")
    return Path(override) if override else Path.home() / ".claude"


class LinuxBackend(Backend):
    name = "file"
    has_profiles = True
    can_switch = True

    def __init__(self, root: Path | None = None) -> None:
        self.root = root or config_dir()
        self.profiles_dir = self.root / "profiles"

    # --- pieces -----------------------------------------------------------
    def _identity(self) -> dict:
        config = (Path.home() / ".claude.json" if self.root == Path.home() / ".claude"
                  else self.root / ".claude.json")
        data = read_json(config) or {}
        return data.get("oauthAccount") or {}

    def _active(self) -> Account | None:
        creds = read_json(self.root / ".credentials.json")
        oauth = (creds or {}).get("claudeAiOauth")
        if not oauth or not oauth.get("accessToken"):
            return None
        identity = self._identity()
        return Account(
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
        )

    def _tracked_alias(self) -> str | None:
        marker = self.profiles_dir / ".current_profile"
        try:
            return marker.read_text().strip() or None
        except (OSError, ValueError):
            return None

    def _profiles(self) -> list[Account]:
        if not self.profiles_dir.is_dir():
            return []
        found = []
        for entry in sorted(self.profiles_dir.iterdir()):
            creds = read_json(entry / "credentials.json") if entry.is_dir() else None
            oauth = (creds or {}).get("claudeAiOauth")
            if not oauth:
                continue
            meta = read_json(entry / "meta.json") or {}
            found.append(Account(
                alias=entry.name,
                email=meta.get("email"),
                org=meta.get("org"),
                org_uuid=meta.get("orgUuid"),
                plan=oauth.get("subscriptionType"),
            rate_limit_tier=oauth.get("rateLimitTier"),
                access_expires_at=oauth.get("expiresAt"),
                refresh_expires_at=oauth.get("refreshTokenExpiresAt"),
                token=oauth.get("accessToken"),
            ))
        return found

    def raw_credentials(self, alias: str) -> tuple[dict, dict | None] | None:
        """Full credentials from the same source selected by account discovery."""
        account = next((a for a in self.accounts() if a.alias == alias), None)
        if account is None:
            return None
        if account.is_active:
            credentials = read_json(self.root / ".credentials.json")
            if credentials:
                return credentials, self._identity()
        entry = self.profiles_dir / alias
        credentials = read_json(entry / "credentials.json")
        if not credentials:
            return None
        return credentials, read_json(entry / "oauthAccount.json")

    # --- api --------------------------------------------------------------
    def accounts(self) -> list[Account]:
        active = self._active()
        profiles = self._profiles()

        if active is not None and profiles:
            same = [p for p in profiles
                    if p.email and p.email == active.email and p.org_uuid == active.org_uuid]
            tracked = self._tracked_alias()
            chosen = next((p for p in same if p.alias == tracked), same[0] if same else None)
            if chosen is not None:
                # The profile and the live file are the same account, so show one row,
                # preferring the live token which is the one actually in use.
                chosen.is_active = True
                chosen.plan = active.plan or chosen.plan
                chosen.rate_limit_tier = active.rate_limit_tier or chosen.rate_limit_tier
                chosen.token = active.token or chosen.token
                chosen.access_expires_at = active.access_expires_at
                chosen.refresh_expires_at = active.refresh_expires_at
                return profiles
            return [active, *profiles]

        if active is not None:
            return [active]
        return profiles
