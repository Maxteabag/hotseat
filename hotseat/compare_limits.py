#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""Compare live Codex rate limits across saved accounts, isolated per profile.

Answers "which saved account has usable quota, and which resets soonest?" without
switching the live account. For each profile it copies auth.json into a private
mode-700 temporary CODEX_HOME and reads `account/rateLimits/read` through an
ephemeral `codex app-server --stdio`. Refreshed tokens are written back atomically.

This reports the server's own numbers, unlike `codex-usage`, whose cached
rollouts do not record account attribution. It does not switch accounts, redeem
reset credits, or wake agents.

Usage:
    python -m hotseat.compare_limits                 # all saved profiles
    python -m hotseat.compare_limits --json
    python -m hotseat.compare_limits personal work   # named profiles only
    python -m hotseat.compare_limits --available     # only accounts with usable quota
"""

from __future__ import annotations

import argparse
import asyncio
import datetime as dt
import json
import os
import shutil
import sys
import tempfile
from pathlib import Path

from .codex_accounts import AUTH_FILE, PROFILES_DIR, describe, _atomic_copy  # noqa: E402


class Rpc:
    async def __aenter__(self):
        self.p = await asyncio.create_subprocess_exec(
            "codex",
            "-c",
            'cli_auth_credentials_store="file"',
            "app-server",
            "--stdio",
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.DEVNULL,
        )
        self.seq = 0
        await self.call(
            "initialize",
            {
                "clientInfo": {"name": "codex_compare_limits", "version": "1"},
                "capabilities": {"experimentalApi": True},
            },
        )
        self.p.stdin.write(b'{"method":"initialized"}\n')
        await self.p.stdin.drain()
        return self

    async def call(self, method, params=None):
        self.seq += 1
        self.p.stdin.write(
            (
                json.dumps({"id": self.seq, "method": method, "params": params}) + "\n"
            ).encode()
        )
        await self.p.stdin.drain()

        async def read():
            while True:
                line = await self.p.stdout.readline()
                if not line:
                    raise RuntimeError("codex app-server exited")
                msg = json.loads(line)
                if msg.get("id") == self.seq:
                    if "error" in msg:
                        raise RuntimeError(str(msg["error"]))
                    return msg["result"]

        return await asyncio.wait_for(read(), 60)

    async def __aexit__(self, *args):
        if self.p.returncode is None:
            self.p.terminate()
            try:
                await asyncio.wait_for(self.p.wait(), 5)
            except asyncio.TimeoutError:
                self.p.kill()
                await self.p.wait()


def _read_profile(auth: Path) -> dict:
    """Return the rate-limit payload for one profile in an isolated home."""
    probe_dir = Path(tempfile.mkdtemp(prefix="codex-limits-"))
    os.chmod(probe_dir, 0o700)
    temp_auth = probe_dir / "auth.json"
    shutil.copy(auth, temp_auth)
    old = os.environ.get("CODEX_HOME")
    os.environ["CODEX_HOME"] = str(probe_dir)

    async def go():
        async with Rpc() as rpc:
            return await rpc.call("account/rateLimits/read")

    try:
        result = asyncio.run(go())
        try:
            if temp_auth.read_bytes() != auth.read_bytes():
                _atomic_copy(temp_auth, auth)
        except OSError:
            pass
        return result
    finally:
        if old is None:
            os.environ.pop("CODEX_HOME", None)
        else:
            os.environ["CODEX_HOME"] = old
        shutil.rmtree(probe_dir, ignore_errors=True)


def _window(win) -> dict | None:
    if not isinstance(win, dict):
        return None
    return {
        "used_percent": win.get("usedPercent"),
        "window_minutes": win.get("windowDurationMins"),
        "resets_at": win.get("resetsAt"),
    }


def _label(minutes) -> str:
    if not minutes:
        return "window"
    minutes = int(minutes)
    if minutes % 10080 == 0:
        return "weekly"
    if minutes % 1440 == 0:
        return f"{minutes // 1440}-day"
    if minutes % 60 == 0:
        return f"{minutes // 60}-hour"
    return f"{minutes}-min"


def _fmt(epoch) -> str:
    if not epoch:
        return "-"
    return (
        dt.datetime.fromtimestamp(int(epoch), dt.timezone.utc)
        .astimezone()
        .strftime("%Y-%m-%d %H:%M %Z")
    )


def _summarize(name: str, email: str, payload: dict) -> dict:
    bucket = payload.get("rateLimits") or {}
    by_id = payload.get("rateLimitsByLimitId") or {}
    windows = []
    for limit_id, lim in by_id.items():
        for key in ("primary", "secondary"):
            w = _window(lim.get(key))
            if w and w.get("used_percent") is not None:
                windows.append(
                    {
                        "limit": limit_id,
                        "tier": key,
                        "label": _label(w["window_minutes"]),
                        **w,
                    }
                )
    blocked = bool(
        bucket.get("rateLimitReachedType") or bucket.get("spendControlReached")
    )
    usable = any((w["used_percent"] or 0) < 100 for w in windows) and not blocked
    # Server object key order is not stable, so pick the minimum explicitly.
    soonest = min(
        (w for w in windows if w.get("resets_at")),
        key=lambda w: w["resets_at"],
        default=None,
    )
    return {
        "name": name,
        "email": email,
        "plan": bucket.get("planType"),
        "blocked": blocked,
        "usable": usable,
        "account_id": payload.get("accountId"),
        "resets_at": soonest["resets_at"] if soonest else None,
        "reset_display": _fmt(soonest["resets_at"]) if soonest else "-",
        "soonest_window": (
            f"{soonest['limit']}/{soonest['tier']} {soonest['label']}"
            if soonest
            else "-"
        ),
        "windows": windows,
        "reset_credits": (payload.get("rateLimitResetCredits") or {}).get(
            "availableCount", 0
        ),
    }


def main() -> None:
    ap = argparse.ArgumentParser(
        prog="compare-limits",
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    ap.add_argument("profiles", nargs="*", help="profile names (default: all saved)")
    ap.add_argument("--json", action="store_true", help="machine-readable output")
    ap.add_argument(
        "--available", action="store_true", help="only accounts with usable quota"
    )
    args = ap.parse_args()

    if args.profiles:
        names = args.profiles
    else:
        names = sorted(
            p.name
            for p in PROFILES_DIR.iterdir()
            if p.is_dir() and (p / "auth.json").exists()
        )
    if AUTH_FILE.exists():
        names = ["(live)", *names]

    rows = []
    for name in names:
        auth = AUTH_FILE if name == "(live)" else PROFILES_DIR / name / "auth.json"
        if not auth.exists():
            print(f"skip {name}: no credentials", file=sys.stderr)
            continue
        email = describe(auth).get("email", "?")
        try:
            payload = _read_profile(auth)
        except Exception as exc:  # noqa: BLE001
            rows.append({"name": name, "email": email, "error": str(exc)})
            continue
        rows.append(_summarize(name, email, payload))

    if args.available:
        rows = [r for r in rows if r.get("usable")]

    rows.sort(key=lambda r: (r.get("error") is not None, r.get("resets_at") or 9**18))

    if args.json:
        print(json.dumps(rows, indent=2))
        return

    for r in rows:
        if r.get("error"):
            print(f"{r['name']:12} ERROR  {r['error'][:60]}")
            continue
        mark = "usable" if r["usable"] else "BLOCKED"
        print(f"{r['name']:12} {mark:8} {str(r['plan'] or '?'):28} {r['email']}")
        if r["reset_credits"]:
            print(f"             banked resets: {r['reset_credits']}")
        for w in sorted(r["windows"], key=lambda w: w.get("resets_at") or 0):
            pct = w.get("used_percent")
            print(
                f"             {w['limit']:22} {w['tier']:9} {w['label']:6} "
                f"{pct:>5}% used  resets {_fmt(w.get('resets_at'))}"
            )


if __name__ == "__main__":
    main()
