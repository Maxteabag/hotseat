"""Refresh a saved Claude profile's expired access token, through the official CLI.

An access token lives about eight hours. A pinned session rotates its own as it
works, but a profile nobody has used since yesterday just sits there expired, and
quota cannot be read with a dead token. This module brings it back without a
browser, for as long as the refresh token is valid (roughly a month).

The refresh is delegated to `claude` itself rather than reimplemented: a
throwaway config directory is seeded with only this profile's credentials, the
copy is backdated so the CLI considers it expired, and one minimal request drives
the CLI's own refresh-and-rotate path. The shared config directory and every
running session are never touched. Rotation issues a new refresh token as well,
so the rotated copy is rescued to disk before anything else can fail.

Before spending that request, a cheaper source is checked: the pinned session
directory for the same alias may already hold a newer token rotated by a session.
"""
from __future__ import annotations

import fcntl
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time

from . import sessiondirs

#: Refresh when less than this remains, so a launched session does not expire
#: moments after it starts.
REFRESH_MARGIN_S = int(os.environ.get("HOTSEAT_REFRESH_MARGIN_S", 30 * 60))
#: After a failed automatic refresh, leave the account alone for this long
#: rather than spending a CLI call on every quota cycle.
RETRY_COOLDOWN_S = 60 * 60
BACKUP_KEEP = 10
CLI_TIMEOUT_S = 120

#: Variables that must not reach the refreshing CLI: an API key outranks account
#: credentials, and the CLAUDE_CODE_* set makes it believe it is a nested child.
BLOCKED_ENV = (
    "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
    "CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
    "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_SESSION_ATTENDED",
    "CLAUDE_CODE_ATTRIBUTION_HEADER", "CLAUDE_CODE_MESSAGING_SOCKET",
    "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_PID", "CLAUDE_EFFORT",
)


class RefreshError(RuntimeError):
    """The profile could not be refreshed. The stored credentials are unchanged."""


# --- storage ---------------------------------------------------------------

def profile_dir(backend, alias: str) -> Path:
    """The saved profile behind an alias, or an explanation of why there is none."""
    profiles = getattr(backend, "profiles_dir", None)
    if profiles is None:
        raise RefreshError("this platform keeps credentials in the Keychain; run `claude` to refresh")
    if alias.startswith("("):
        raise RefreshError("the signed-in account is not a saved profile; it refreshes when a session uses it")
    directory = Path(profiles) / alias
    if not (directory / "credentials.json").is_file():
        raise RefreshError(f"no saved profile named {alias!r}")
    return directory


def _read(path: Path) -> dict:
    try:
        data = json.loads(path.read_text())
    except (OSError, ValueError) as exc:
        raise RefreshError(f"unreadable credentials at {path}: {exc}") from exc
    if not isinstance(data, dict) or not isinstance(data.get("claudeAiOauth"), dict):
        raise RefreshError(f"{path} does not hold Claude OAuth credentials")
    return data


def _expires_at(credentials: dict | None) -> float:
    """Expiry in seconds since the epoch; 0 when unknown."""
    try:
        return int((credentials or {}).get("claudeAiOauth", {}).get("expiresAt") or 0) / 1000
    except (TypeError, ValueError):
        return 0.0


def _atomic_write(path: Path, data: dict) -> None:
    tmp = None
    try:
        with tempfile.NamedTemporaryFile("w", dir=path.parent, delete=False) as handle:
            tmp = Path(handle.name)
            json.dump(data, handle, indent=2)
            handle.flush()
            os.fsync(handle.fileno())
        tmp.chmod(0o600)
        os.replace(tmp, path)
        tmp = None
    finally:
        if tmp is not None:
            tmp.unlink(missing_ok=True)


def _backup(directory: Path, name: str, data: dict | None = None, now: float | None = None) -> Path:
    """Keep a dated copy under backups/ and prune old ones.

    Protects against a bad local write only. Once a newer refresh token exists,
    older copies are dead server-side no matter what is on disk.
    """
    backups = directory / "backups"
    backups.mkdir(exist_ok=True, mode=0o700)
    dest = backups / f"{name}.{int(now or time.time())}.json"
    if data is None:
        shutil.copy2(directory / "credentials.json", dest)
    else:
        dest.write_text(json.dumps(data, indent=2))
    dest.chmod(0o600)
    for prefix in ("credentials", "rotated"):
        for stale in sorted(backups.glob(f"{prefix}.*.json"))[:-BACKUP_KEEP]:
            stale.unlink(missing_ok=True)
    return dest


def _store(directory: Path, credentials: dict, now: float) -> None:
    _backup(directory, "credentials", now=now)
    _atomic_write(directory / "credentials.json", credentials)


class _lock:
    """Serialise refreshes so only one process ever rotates a profile's token."""

    def __init__(self, directory: Path) -> None:
        self.path = directory / ".lock"

    def __enter__(self):
        self.handle = self.path.open("a")
        fcntl.flock(self.handle, fcntl.LOCK_EX)
        return self

    def __exit__(self, *exc_info) -> None:
        try:
            fcntl.flock(self.handle, fcntl.LOCK_UN)
        finally:
            self.handle.close()


# --- the refresh -----------------------------------------------------------

def _pinned_credentials(alias: str, session_root: Path | None) -> tuple[Path | None, dict | None]:
    """Credentials rotated by a pinned session for this alias, if that directory exists."""
    path = (session_root or sessiondirs.accounts_root()) / alias / ".credentials.json"
    if not path.is_file():
        return None, None
    try:
        data = json.loads(path.read_text())
    except (OSError, ValueError):
        return path, None
    if not isinstance(data, dict) or not isinstance(data.get("claudeAiOauth"), dict):
        return path, None
    return path, data


def _refresh_via_cli(directory: Path, credentials: dict, *, run, claude: str, now: float) -> dict:
    identity = directory / "oauthAccount.json"
    with tempfile.TemporaryDirectory(prefix="hotseat-refresh-") as work:
        work_dir = Path(work)
        seeded = json.loads(json.dumps(credentials))
        # The CLI refreshes only when it considers the token expired, so backdate
        # this disposable copy. The stored profile is untouched.
        seeded["claudeAiOauth"]["expiresAt"] = int((now - 60) * 1000)
        cred_path = work_dir / ".credentials.json"
        _atomic_write(cred_path, seeded)
        config = {}
        if identity.is_file():
            try:
                config["oauthAccount"] = json.loads(identity.read_text())
            except (OSError, ValueError):
                pass
        _atomic_write(work_dir / ".claude.json", config)

        env = {k: v for k, v in os.environ.items() if k not in BLOCKED_ENV}
        env["CLAUDE_CONFIG_DIR"] = str(work_dir)
        # `auth status` only reports; a minimal request is what drives the CLI's
        # own refresh-and-rotate path.
        command = [claude, "--safe-mode", "--print", "--model", "haiku", "--tools", "",
                   "--no-session-persistence", "--max-budget-usd", "0.02",
                   "--output-format", "json", "--system-prompt", "Reply only OK.", "Reply OK."]
        try:
            run(command, cwd=str(work_dir), env=env, check=False,
                capture_output=True, text=True, timeout=CLI_TIMEOUT_S)
        except (OSError, subprocess.SubprocessError) as exc:
            raise RefreshError(f"could not run {claude}: {exc}") from exc

        try:
            rotated = json.loads(cred_path.read_text())
        except (OSError, ValueError) as exc:
            raise RefreshError(f"the CLI left unreadable credentials behind: {exc}") from exc
        if rotated != seeded:
            # A rotated refresh token exists only here until it is stored. Persist a
            # copy before anything else can fail; a crash now would lock the account out.
            _backup(directory, "rotated", rotated, now)
        return rotated


def refresh(backend, alias: str, *, force: bool = False, margin: float = REFRESH_MARGIN_S,
            run=subprocess.run, claude: str = "claude", now=None, session_root: Path | None = None) -> dict:
    """Bring a saved profile's access token back to full length.

    Returns a summary dict. Raises RefreshError when nothing could be done; the
    stored credentials are then exactly as they were.
    """
    now = time.time() if now is None else now
    directory = profile_dir(backend, alias)
    with _lock(directory):
        credentials = _read(directory / "credentials.json")
        expires = _expires_at(credentials)
        remaining = expires - now
        result = {"alias": alias, "refreshed": False, "source": None, "expires_at": expires,
                  "hours_left": round(remaining / 3600, 2)}
        if not force and remaining > margin:
            result["source"] = "fresh"
            return result

        pinned_path, pinned = _pinned_credentials(alias, session_root)
        if pinned and _expires_at(pinned) > expires and _expires_at(pinned) - now > margin:
            _store(directory, pinned, now)
            result.update(refreshed=True, source="session", expires_at=_expires_at(pinned),
                          hours_left=round((_expires_at(pinned) - now) / 3600, 2))
            return result

        rotated = _refresh_via_cli(directory, credentials, run=run, claude=claude, now=now)
        oauth = rotated.get("claudeAiOauth") or {}
        if not oauth.get("accessToken"):
            raise RefreshError("the CLI could not refresh this profile; its refresh token is probably "
                               "superseded or revoked, so a browser sign-in is needed")
        if oauth.get("accessToken") == credentials["claudeAiOauth"].get("accessToken"):
            # The CLI declined to rotate. Report honestly rather than pretend; the
            # copy it saw was backdated on purpose, so its expiry says nothing.
            result["source"] = "unchanged"
            return result
        new_expires = _expires_at(rotated)
        if new_expires <= now:
            raise RefreshError("the CLI returned an already-expired token")
        _store(directory, rotated, now)
        if pinned_path is not None and _expires_at(pinned) < new_expires:
            # Keep the pinned session directory from lagging behind the profile.
            _atomic_write(pinned_path, rotated)
        result.update(refreshed=True, source="cli", expires_at=new_expires,
                      hours_left=round((new_expires - now) / 3600, 2))
        return result


# --- automatic use during quota reads ------------------------------------------

def _memo_path() -> Path:
    return Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache")) / "hotseat" / "refresh-failures.json"


def _memo_read() -> dict:
    try:
        data = json.loads(_memo_path().read_text())
        return data if isinstance(data, dict) else {}
    except (OSError, ValueError):
        return {}


def _memo_write(data: dict) -> None:
    path = _memo_path()
    try:
        path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        _atomic_write(path, data)
    except OSError:
        pass


def auto(backend, account, *, now=None, **kwargs) -> str:
    """Refresh an expired account during a quota read and return the new token.

    Disabled with HOTSEAT_NO_REFRESH=1. A failure is remembered for
    RETRY_COOLDOWN_S so the TUI's refresh cycle does not spend a CLI call on the
    same dead profile every fifteen seconds.
    """
    now = time.time() if now is None else now
    if os.environ.get("HOTSEAT_NO_REFRESH"):
        raise RefreshError("automatic refresh disabled (HOTSEAT_NO_REFRESH)")
    memo = _memo_read()
    last = memo.get(account.alias) or {}
    if isinstance(last, dict) and now - float(last.get("at", 0)) < RETRY_COOLDOWN_S:
        raise RefreshError(f"{last.get('error', 'refresh failed')} (not retried for "
                           f"{int((RETRY_COOLDOWN_S - (now - float(last['at']))) / 60)} min)")
    try:
        outcome = refresh(backend, account.alias, now=now, **kwargs)
    except RefreshError as exc:
        memo[account.alias] = {"at": now, "error": str(exc)}
        _memo_write(memo)
        raise
    if account.alias in memo:
        memo.pop(account.alias)
        _memo_write(memo)
    if not outcome["refreshed"] and outcome["source"] != "fresh":
        raise RefreshError("the CLI did not rotate the token")
    directory = profile_dir(backend, account.alias)
    oauth = _read(directory / "credentials.json")["claudeAiOauth"]
    account.token = oauth.get("accessToken")
    account.access_expires_at = oauth.get("expiresAt")
    account.refresh_expires_at = oauth.get("refreshTokenExpiresAt", account.refresh_expires_at)
    return account.token


def expired(account, now=None, margin: float = 0) -> bool:
    """Whether the stored access token can no longer be used."""
    now = time.time() if now is None else now
    expires = getattr(account, "access_expires_at", None)
    return bool(expires) and expires / 1000 - now <= margin
