#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""Multi-account profile manager for the Codex CLI.

Codex keeps exactly one credential set in ~/.codex/auth.json. This stores a
copy per named profile and swaps the live file, so several ChatGPT accounts
can share one machine without re-running the browser login each time.

Identity and plan are read out of the id_token stored in the profile, so
`list` works offline and never contacts OpenAI.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import re
import tempfile
import shutil
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

CODEX_DIR = Path(os.environ.get("CODEX_HOME") or Path.home() / ".codex")
AUTH_FILE = CODEX_DIR / "auth.json"
PROFILES_DIR = CODEX_DIR / "profiles"
CURRENT_PROFILE_FILE = PROFILES_DIR / ".current_profile"
SWITCHED_AT_FILE = PROFILES_DIR / ".switched_at"

AUTH_CLAIM = "https://api.openai.com/auth"


def fetch_reset_credits(auth_path: Path) -> dict:
    """Fetch banked rate-limit reset credits from OpenAI."""
    if not auth_path.exists():
        return {"available_count": 0, "credits": []}
    try:
        import urllib.request

        data = json.loads(auth_path.read_text())
        tokens = data.get("tokens", {})
        access_token = tokens.get("access_token")
        if not access_token:
            return {"available_count": 0, "credits": []}
        account_id = tokens.get("account_id")

        req = urllib.request.Request(
            "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
        )
        req.add_header("Authorization", f"Bearer {access_token}")
        req.add_header("User-Agent", "Codex Desktop")
        req.add_header("originator", "Codex Desktop")
        req.add_header("OAI-Product-Sku", "CODEX")
        req.add_header("Accept", "application/json")
        if account_id:
            req.add_header("ChatGPT-Account-ID", account_id)

        with urllib.request.urlopen(req, timeout=10) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except Exception:
        return {"available_count": 0, "credits": []}


def _fail(msg: str) -> None:
    print(f"error: {msg}", file=sys.stderr)
    raise SystemExit(1)


def _decode_id_token(token: str) -> dict:
    """Return the JWT payload. Signature is not verified: this is a local
    read of our own stored token, only ever used for display."""
    try:
        part = token.split(".")[1]
        part += "=" * (-len(part) % 4)
        return json.loads(base64.urlsafe_b64decode(part))
    except Exception:
        return {}


def describe(auth_path: Path) -> dict:
    """Identity summary for an auth.json. Never returns token material."""
    out = {
        "email": "?",
        "plan": "?",
        "account_id": "",
        "mode": "?",
        "expires": None,
        "name": "",
        "refreshable": False,
        "last_refresh": "",
    }
    try:
        data = json.loads(auth_path.read_text())
    except Exception:
        return out
    out["mode"] = data.get("auth_mode") or (
        "apikey" if data.get("OPENAI_API_KEY") else "?"
    )
    tokens = data.get("tokens") or {}
    out["account_id"] = tokens.get("account_id", "")
    # The id_token expires hourly by design and Codex refreshes it silently,
    # so its expiry says nothing about whether the profile works. The
    # refresh_token is what makes a stored profile reusable.
    out["refreshable"] = bool(tokens.get("refresh_token"))
    out["last_refresh"] = data.get("last_refresh", "")
    claims = _decode_id_token(tokens.get("id_token", ""))
    out["email"] = claims.get("email", out["email"])
    out["name"] = claims.get("name", "")
    auth = claims.get(AUTH_CLAIM) or {}
    out["plan"] = auth.get("chatgpt_plan_type", out["plan"])
    if claims.get("exp"):
        out["expires"] = datetime.fromtimestamp(claims["exp"], timezone.utc)
    return out


def _current() -> str | None:
    if CURRENT_PROFILE_FILE.exists():
        name = CURRENT_PROFILE_FILE.read_text().strip()
        return name or None
    return None


def _profile_dir(name: str) -> Path:
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]{0,63}", name):
        _fail(
            "profile names must contain only letters, numbers, underscores or hyphens"
        )
    return PROFILES_DIR / name


def _atomic_copy(source: Path, target: Path) -> None:
    target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temporary = tempfile.mkstemp(prefix=".auth-", dir=target.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(source.read_bytes())
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, target)
    finally:
        Path(temporary).unlink(missing_ok=True)


def _isolated_env(home: Path) -> dict:
    return dict(os.environ, CODEX_HOME=str(home))


def _status(home: Path) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["codex", "-c", 'cli_auth_credentials_store="file"', "login", "status"],
        env=_isolated_env(home),
        capture_output=True,
        text=True,
        timeout=30,
    )


def cmd_verify(args) -> None:
    source = _profile_dir(args.name) / "auth.json"
    info = describe(source)
    if not source.exists() or not info["refreshable"]:
        _fail("saved ChatGPT profile missing or has no refresh token")
    with tempfile.TemporaryDirectory(prefix="codex-verify-") as directory:
        home = Path(directory)
        if _status(home).returncode != 1:
            _fail(
                "empty CODEX_HOME was not reported as logged out; isolation unverified"
            )
        _atomic_copy(source, home / "auth.json")
        if _status(home).returncode != 0:
            _fail("Codex did not accept the saved profile")
    print(
        f"{args.name}: {info['email']} — local login status accepted; "
        "server token validity and quota not tested"
    )


def _mark_switch(previous_account: str, new_account: str) -> None:
    """Stamp only a real change of account, so codex-usage can tell that an
    older reading belongs to someone else. Re-saving or re-activating the
    same account must not raise a warning, or the warning becomes noise."""
    if previous_account and previous_account == new_account:
        return
    PROFILES_DIR.mkdir(parents=True, exist_ok=True)
    SWITCHED_AT_FILE.write_text(
        datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")
    )


def _snapshot(name: str) -> None:
    """Copy the live auth.json into the named profile."""
    if not AUTH_FILE.exists():
        _fail("no ~/.codex/auth.json to save - run `codex login` first")
    target = _profile_dir(name)
    target.mkdir(parents=True, exist_ok=True)
    _atomic_copy(AUTH_FILE, target / "auth.json")


def cmd_save(args) -> None:
    _snapshot(args.name)
    CURRENT_PROFILE_FILE.parent.mkdir(parents=True, exist_ok=True)
    CURRENT_PROFILE_FILE.write_text(args.name)
    info = describe(_profile_dir(args.name) / "auth.json")
    print(f"saved profile '{args.name}'  {info['email']}  plan={info['plan']}")


def cmd_list(args) -> None:
    PROFILES_DIR.mkdir(parents=True, exist_ok=True)
    profiles = sorted(
        p for p in PROFILES_DIR.iterdir() if p.is_dir() and (p / "auth.json").exists()
    )
    if not profiles:
        print("no saved profiles yet - run: hotseat codex-account save <name>")
        return
    active = _current()
    live = describe(AUTH_FILE) if AUTH_FILE.exists() else None
    print(f"{'':2} {'PROFILE':<16} {'EMAIL':<32} {'PLAN':<8} {'MODE':<8} AUTH")
    for p in profiles:
        info = describe(p / "auth.json")
        # A profile is live if its account_id matches the active auth.json;
        # the .current_profile marker alone can go stale if codex re-logs in.
        is_live = bool(
            (live and live["account_id"] and live["account_id"] == info["account_id"]
             and live["email"] != "?" and live["email"] == info["email"])
            or (p.name == active and live and live.get("mode") == "apikey" and info.get("mode") == "apikey")
        )
        mark = "*" if is_live else ("~" if p.name == active else " ")
        if info["mode"] == "apikey":
            auth = "api-key"
        elif info["refreshable"]:
            auth = "ok"
        else:
            auth = "no-refresh"
        print(
            f"{mark:2} {p.name:<16} {info['email']:<32} {info['plan']:<8} "
            f"{info['mode']:<8} {auth}"
        )
    print("\n* = matches the live auth.json   ~ = last activated by this tool")


def cmd_switch(args) -> None:
    target = _profile_dir(args.name)
    if not (target / "auth.json").exists():
        _fail(f"no saved profile '{args.name}' - try: hotseat codex-account list")
    before = describe(AUTH_FILE)["account_id"] if AUTH_FILE.exists() else ""
    # Snapshot whatever is live first, so an unsaved refresh is never lost.
    active = _current()
    if AUTH_FILE.exists() and not active:
        _fail("save the current login with hotseat codex-account save <name> before switching")
    if AUTH_FILE.exists() and active:
        saved = describe(_profile_dir(active) / "auth.json")
        live = describe(AUTH_FILE)
        if (saved["account_id"] != before
                or (live["mode"] != "apikey" and (
                    live["email"] == "?" or saved["email"] != live["email"]))):
            _fail(
                "live account differs from the profile marker; save it under the correct name first"
            )
        _snapshot(active)
    PROFILES_DIR.mkdir(parents=True, exist_ok=True)
    _atomic_copy(target / "auth.json", AUTH_FILE)
    CURRENT_PROFILE_FILE.write_text(args.name)
    info = describe(AUTH_FILE)
    _mark_switch(before, info["account_id"])
    print(f"switched to '{args.name}'  {info['email']}  plan={info['plan']}")
    if not info["refreshable"] and info["mode"] != "apikey":
        print(
            f"warning: this profile has no refresh token; if Codex rejects "
            f"it run: hotseat codex-account login {args.name}"
        )


def cmd_current(args) -> None:
    if not AUTH_FILE.exists():
        print("not logged in")
        return
    info = describe(AUTH_FILE)
    active = _current()
    print(f"email:   {info['email']}")
    print(f"name:    {info['name']}")
    print(f"plan:    {info['plan']}")
    print(f"mode:    {info['mode']}")
    print(f"profile: {active or '(unsaved)'}")


def cmd_login(args) -> None:
    """Approve a login in the user's browser; save without activation."""
    target = _profile_dir(args.name) / "auth.json"
    force = getattr(args, "force", False)
    browser = getattr(args, "browser", False)
    if target.exists() and not force:
        _fail("profile already exists; use --force to overwrite, or choose a new name")
    with tempfile.TemporaryDirectory(prefix="codex-login-") as directory:
        home = Path(directory)
        if _status(home).returncode != 1:
            _fail("empty CODEX_HOME was not reported as logged out; refusing login")
        if browser:
            print(
                f"Sign in via the browser as {args.email}. "
                "The active profile will not be switched.",
                flush=True,
            )
            cmd = ["codex", "-c", 'cli_auth_credentials_store="file"', "login"]
        else:
            print(
                f"Approve the device code in your own browser as {args.email}. "
                "The active profile will not be switched.",
                flush=True,
            )
            cmd = [
                "codex",
                "-c",
                'cli_auth_credentials_store="file"',
                "login",
                "--device-auth",
            ]
        try:
            rc = subprocess.call(cmd, env=_isolated_env(home))
        except KeyboardInterrupt:
            _fail("login cancelled; no profile saved")
        if rc != 0:
            _fail(f"codex login exited {rc}; no profile saved")
        source = home / "auth.json"
        info = describe(source)
        if info["email"].lower() != args.email.lower() or not info["refreshable"]:
            _fail("login identity mismatch or missing refresh token; no profile saved")
        if _status(home).returncode != 0:
            _fail("Codex did not accept the new credentials; no profile saved")
        if target.exists() and not force:
            _fail("profile was created during login; refusing to overwrite it")
        _atomic_copy(source, target)
    print(f"saved profile '{args.name}'  {info['email']}  plan={info['plan']}")
    print(f"activate with: hotseat codex-account switch {args.name}")


def probe_credentials(auth_path: Path, profile_name: str = "") -> dict:
    """Run an isolated test request against OpenAI to check token validity and live quota."""
    if not auth_path.exists():
        return {"status": "error", "error": f"credentials not found: {auth_path}"}
    info = describe(auth_path)
    probe_dir = CODEX_DIR / f".probe_{profile_name or 'live'}_{os.getpid()}"
    if probe_dir.exists():
        shutil.rmtree(probe_dir, ignore_errors=True)
    probe_dir.mkdir(parents=True, mode=0o700)
    try:
        temp_auth = probe_dir / "auth.json"
        shutil.copy(auth_path, temp_auth)
        env = dict(os.environ, CODEX_HOME=str(probe_dir))
        cmd = [
            "codex",
            "-c",
            'cli_auth_credentials_store="file"',
            "exec",
            "--json",
            "--skip-git-repo-check",
            "respond with 1",
        ]
        proc = subprocess.run(cmd, env=env, capture_output=True, text=True, timeout=45)

        # Preserve refreshed tokens if any
        try:
            new_bytes = temp_auth.read_bytes()
            if new_bytes != auth_path.read_bytes():
                _atomic_copy(temp_auth, auth_path)
        except Exception:
            pass

        error_msg = None
        rate_limits = None
        for line in proc.stdout.splitlines():
            try:
                rec = json.loads(line)
            except Exception:
                continue
            if rec.get("type") == "error":
                error_msg = rec.get("message")
            pl = rec.get("payload") or {}
            rl = pl.get("rate_limits")
            if rl:
                rate_limits = rl

        if error_msg:
            if "revoked" in error_msg.lower() or "refresh token" in error_msg.lower():
                return {"status": "revoked", "email": info["email"], "error": error_msg}
            if (
                "usage limit" in error_msg.lower()
                or "out of credits" in error_msg.lower()
            ):
                return {
                    "status": "limit_reached",
                    "email": info["email"],
                    "error": error_msg,
                    "reset_credits": fetch_reset_credits(auth_path),
                }
            return {
                "status": "error",
                "email": info["email"],
                "error": error_msg,
                "reset_credits": fetch_reset_credits(auth_path),
            }

        if rate_limits:
            return {
                "status": "ok",
                "email": info["email"],
                "rate_limits": rate_limits,
                "reset_credits": fetch_reset_credits(auth_path),
            }
        # Some Codex versions complete a turn without emitting a rate_limits
        # window. A successful turn with no error still proves the account is
        # accepted and has quota; do not mislabel that as UNKNOWN.
        if proc.returncode == 0 and '"turn.completed"' in proc.stdout:
            return {
                "status": "ok",
                "email": info["email"],
                "rate_limits": None,
                "note": "turn completed; no rate-limit window reported by this Codex version",
                "reset_credits": fetch_reset_credits(auth_path),
            }
        return {
            "status": "unknown",
            "email": info["email"],
            "stdout": proc.stdout,
            "stderr": proc.stderr,
            "reset_credits": fetch_reset_credits(auth_path),
        }
    except Exception as e:
        return {"status": "error", "email": info.get("email"), "error": str(e)}
    finally:
        shutil.rmtree(probe_dir, ignore_errors=True)


def cmd_probe(args) -> None:
    if args.name:
        target = _profile_dir(args.name) / "auth.json"
        name = args.name
    else:
        target = AUTH_FILE
        name = _current() or "live"
    if not target.exists():
        _fail(f"no credentials found for '{name}'")

    res = probe_credentials(target, name)
    rc = res.get("reset_credits") or fetch_reset_credits(target)
    if getattr(args, "json", False):
        res["reset_credits"] = rc
        print(json.dumps(res, indent=2))
        return

    email = res.get("email", "?")
    status = res.get("status")
    print(f"Codex live probe - {name} ({email})")
    if status == "revoked":
        print(f"  STATUS: ❌ REVOKED / EXPIRED")
        print(f"  {res.get('error')}")
        print(f"  Fix: codex-login {name} --email {email} --force")
    elif status == "limit_reached":
        print(f"  STATUS: ⛔ USAGE LIMIT REACHED")
        print(f"  {res.get('error')}")
    elif status == "ok":
        print(f"  STATUS: ✅ ACTIVE")
        rl = res.get("rate_limits") or {}
        pri = rl.get("primary")
        sec = rl.get("secondary")
        if pri:
            dt = datetime.fromtimestamp(
                pri.get("resets_at", 0), timezone.utc
            ).astimezone()
            print(
                f"  Primary (5-hour):    {pri.get('used_percent', 0.0):5.1f}% used  (resets: {dt:%Y-%m-%d %H:%M})"
            )
        if sec:
            dt = datetime.fromtimestamp(
                sec.get("resets_at", 0), timezone.utc
            ).astimezone()
            print(
                f"  Secondary (weekly):  {sec.get('used_percent', 0.0):5.1f}% used  (resets: {dt:%Y-%m-%d %H:%M})"
            )
        if not pri and not sec:
            print("  (no window data reported; account accepted a live test turn)")
    else:
        print(f"  STATUS: ⚠️ {(status or 'unknown').upper()}")
        print(f"  {res.get('error') or res.get('stderr') or res.get('stdout')}")

    avail = rc.get("available_count", 0)
    credits = rc.get("credits", [])
    if avail > 0:
        print(f"\n  Banked resets:       {avail} available")
        now = datetime.now(timezone.utc)
        for c in credits:
            if c.get("status") != "available":
                continue
            title = c.get("title") or "Full reset"
            exp_iso = c.get("expires_at")
            if exp_iso:
                try:
                    exp_dt = datetime.fromisoformat(exp_iso.replace("Z", "+00:00"))
                    delta = exp_dt - now
                    secs = int(delta.total_seconds())
                    if secs <= 0:
                        exp_str = f"{exp_dt.astimezone():%Y-%m-%d %H:%M} (expired)"
                    else:
                        d, r = divmod(secs, 86400)
                        h = r // 3600
                        exp_str = f"{exp_dt.astimezone():%Y-%m-%d %H:%M} (in {d}d {h}h)"
                except Exception:
                    exp_str = exp_iso
            else:
                exp_str = "unknown"
            print(f"    - {title}: expires {exp_str}")


def cmd_remove(args) -> None:
    target = _profile_dir(args.name)
    if not target.exists():
        _fail(f"no saved profile '{args.name}'")
    shutil.rmtree(target)
    if _current() == args.name:
        CURRENT_PROFILE_FILE.write_text("")
    print(f"removed profile '{args.name}' (the live session is untouched)")


def main() -> None:
    ap = argparse.ArgumentParser(
        prog="hotseat codex-account",
        description="Save, list, and switch Codex CLI accounts.",
    )
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("save", help="snapshot the live login under a name")
    p.add_argument("name")
    p.set_defaults(func=cmd_save)

    p = sub.add_parser("list", help="list saved profiles")
    p.set_defaults(func=cmd_list)

    p = sub.add_parser("switch", help="activate a saved profile")
    p.add_argument("name")
    p.set_defaults(func=cmd_switch)

    p = sub.add_parser("current", help="show the live account")
    p.set_defaults(func=cmd_current)

    p = sub.add_parser(
        "login",
        help="add a new account via browser OAuth or device code without switching",
    )
    p.add_argument("name")
    p.add_argument(
        "--email", required=True, help="refuse to save if a different account signs in"
    )
    p.add_argument(
        "--browser",
        action="store_true",
        help="use standard browser OAuth flow instead of device code",
    )
    p.add_argument(
        "--force", "-f", action="store_true", help="overwrite an existing saved profile"
    )
    p.set_defaults(func=cmd_login)

    p = sub.add_parser("verify", help="check a saved profile locally without switching")
    p.add_argument("name")
    p.set_defaults(func=cmd_verify)

    p = sub.add_parser(
        "probe",
        help="test server token validity and live quota in an isolated environment",
    )
    p.add_argument(
        "name", nargs="?", help="profile name to probe (defaults to live profile)"
    )
    p.add_argument("--json", action="store_true", help="machine-readable output")
    p.set_defaults(func=cmd_probe)

    p = sub.add_parser("remove", help="delete a saved profile")
    p.add_argument("name")
    p.set_defaults(func=cmd_remove)

    args = ap.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
