"""Codex accounts, alongside the Claude ones.

Codex stores one live credential at ~/.codex/auth.json and keeps saved accounts
under ~/.codex/profiles/<name>/. Identity is not recorded separately: it lives in
the claims of the stored id_token, which is read locally and never sent anywhere.

Quota is a different shape from Claude's. An account can carry several named limit
windows at once, primary and secondary, and being blocked on one does not mean the
account is unusable. Rather than reimplement that, this delegates to the maintained
`compare_limits` helper, which reads the server's own numbers through an isolated
app-server per profile and never switches the live account.
"""

from __future__ import annotations

import base64
import json
import os
from pathlib import Path
import shutil
import subprocess
from . import bundled
import time

CODEX_HOME = Path(os.environ.get("CODEX_HOME") or Path.home() / ".codex")
LIVE_AUTH = CODEX_HOME / "auth.json"
PROFILES_DIR = CODEX_HOME / "profiles"
COMPARE_HELPER = Path(__file__).with_name("compare_limits.py")
#: Each profile is probed through its own app-server, one at a time, so a probe
#: spawns one node process per saved account. That is far too heavy to run on a
#: refresh timer, hence the cache below.
PROBE_TIMEOUT_S = 300
#: Codex quota windows are hourly and weekly, so a stale reading costs nothing and
#: re-probing every few minutes costs a process per account.
LIMITS_TTL_S = 1800
LIVE_NAME = "(live)"
#: Shown beside an account that exists only in the live credential file.
UNSAVED_TAG = "unsaved"


class CodexError(RuntimeError):
    """Codex is not installed here, or its accounts could not be read."""


def available() -> bool:
    return LIVE_AUTH.exists() or PROFILES_DIR.is_dir()


def _claims(id_token: str | None) -> dict:
    """Decode the identity claims of a JWT without verifying it.

    Verification would need OpenAI's signing keys and a network call. This is only
    used to label an account the user is already signed into, so the claims are
    read as-is and never trusted for authorisation.
    """
    if not id_token or id_token.count(".") != 2:
        return {}
    payload = id_token.split(".")[1]
    payload += "=" * (-len(payload) % 4)
    try:
        return json.loads(base64.urlsafe_b64decode(payload))
    except (ValueError, TypeError):
        return {}


def _identity(auth_file: Path) -> dict | None:
    try:
        stored = json.loads(auth_file.read_text())
    except (OSError, ValueError):
        return None
    tokens = stored.get("tokens") or {}
    claims = _claims(tokens.get("id_token"))
    auth = claims.get("https://api.openai.com/auth") or {}
    expires = claims.get("exp")
    return {
        "email": claims.get("email"),
        "plan": auth.get("chatgpt_plan_type"),
        "account_id": auth.get("chatgpt_account_id") or tokens.get("account_id"),
        "token_expires_at": expires,
        "token_hours_left": ((expires - time.time()) / 3600) if expires else None,
        "api_key_present": bool(stored.get("OPENAI_API_KEY")),
    }


def saved_profiles() -> list[str]:
    if not PROFILES_DIR.is_dir():
        return []
    return sorted(entry.name for entry in PROFILES_DIR.iterdir()
                  if (entry / "auth.json").exists())


def accounts() -> list[dict]:
    """Every Codex account on this machine, live one included.

    The live credential is reported as its own entry when no saved profile holds
    the same account. An unsaved live account is easy to lose, so it is named
    rather than quietly folded into whichever profile happens to be listed first.
    """
    live = _identity(LIVE_AUTH) if LIVE_AUTH.exists() else None
    found = []
    for name in saved_profiles():
        identity = _identity(PROFILES_DIR / name / "auth.json")
        if identity is None:
            continue
        is_live = bool(live and identity.get("account_id")
                       and identity["account_id"] == live.get("account_id")
                       and identity.get("email") == live.get("email"))
        found.append({"provider": "codex", "alias": name, "is_active": is_live,
                      "saved": True, **identity})

    if live and not any(entry["is_active"] for entry in found):
        found.insert(0, {"provider": "codex", "alias": LIVE_NAME, "is_active": True,
                         "saved": False, **live})
    return found


#: (fetched_at, result). Process-local, so a CLI run never reuses a server's copy.
_LIMITS_CACHE: tuple[float, dict[str, dict]] | None = None


def cached_limits(max_age: float = LIMITS_TTL_S) -> tuple[dict[str, dict], float | None]:
    """Quota from cache when it is fresh enough, otherwise a new probe.

    Returns the readings and the age of what was returned, so a caller can say
    how old the numbers are rather than implying they are current.
    """
    global _LIMITS_CACHE
    now = time.time()
    if _LIMITS_CACHE is not None:
        fetched_at, cached = _LIMITS_CACHE
        if now - fetched_at <= max_age:
            return cached, now - fetched_at
    fresh = limits()
    # Only cache a real reading. Caching a failure would hide a recovery for the
    # rest of the window.
    if fresh:
        _LIMITS_CACHE = (now, fresh)
        return fresh, 0.0
    if _LIMITS_CACHE is not None:
        fetched_at, cached = _LIMITS_CACHE
        return cached, now - fetched_at
    return {}, None


def limits(timeout: int = PROBE_TIMEOUT_S) -> dict[str, dict]:
    """Live quota per account, keyed by profile name.

    Delegates to the maintained comparison helper rather than reimplementing the
    app-server protocol. Returns an empty mapping when it is unavailable: quota is
    an enrichment here, and missing it must not hide the accounts themselves.
    """
    if not COMPARE_HELPER.exists() or not shutil.which("codex"):
        return {}
    try:
        result = subprocess.run(bundled.command("hotseat.compare_limits", "--json"),
                                capture_output=True, text=True, timeout=timeout,
                                check=False)
    except (OSError, subprocess.SubprocessError):
        return {}
    if result.returncode != 0 or not result.stdout.strip():
        return {}
    try:
        rows = json.loads(result.stdout)
    except ValueError:
        return {}

    keyed = {}
    for row in rows if isinstance(rows, list) else []:
        if not isinstance(row, dict) or not row.get("name"):
            continue
        windows = [w for w in (row.get("windows") or [])
                   if isinstance(w, dict) and isinstance(w.get("used_percent"), (int, float))
                   and not isinstance(w.get("used_percent"), bool)]
        worst = max((w.get("used_percent") or 0 for w in windows), default=None)
        keyed[row["name"]] = {
            "usable": bool(row.get("usable")),
            "blocked": bool(row.get("blocked")),
            "error": row.get("error"),
            # One account can hold several named windows at once; the highest is
            # what actually constrains it.
            "worst_used": (worst / 100.0) if worst is not None else None,
            "reset": row.get("resets_at"),
            "reset_window": row.get("soonest_window"),
            "windows": [{
                "label": f"{w.get('limit','?')} {w.get('tier','')} {w.get('label','')}".strip(),
                "used": (w.get("used_percent") or 0) / 100.0,
                "reset": w.get("resets_at"),
            } for w in windows],
        }
    return keyed


def overview(with_limits: bool = True, max_age: float = 0.0) -> dict:
    """Codex accounts, with quota when it can be read.

    `max_age` accepts a cached reading up to that many seconds old. Background
    callers should pass a generous value: probing spawns a process per account.
    """
    if not available():
        raise CodexError("No Codex installation found at ~/.codex.")
    entries = accounts()
    quota, age = ({}, None)
    if with_limits:
        quota, age = cached_limits(max_age) if max_age else (limits(), 0.0)
    for entry in entries:
        # The helper reports the live credential under its own name too.
        key = entry["alias"] if entry["saved"] else LIVE_NAME
        entry["usage"] = quota.get(key)
    unsaved = [e for e in entries if not e["saved"]]
    return {
        "accounts": entries,
        "unsaved_live": unsaved[0]["email"] if unsaved else None,
        "quota_read": bool(quota),
        "quota_age_s": age,
    }
