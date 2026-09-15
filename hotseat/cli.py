"""The command line, which is the primary interface.

Every capability is reachable from here, and each command takes `--json` so it can
be scripted. The dashboard is one optional consumer of the same core functions; it
is not privileged, and nothing is reachable only through it.

Exit codes: 0 success, 1 the operation failed, 2 usage error.
"""

from __future__ import annotations

import argparse
import hashlib
import os
import platform
import shutil
import subprocess
import json
import re
import tempfile
import urllib.request
from pathlib import Path
import sys
import time

from . import __version__
from . import bundled, actions, codex, codexsessions, inspect as inspect_mod, modelstats, resume, statusline
from .backends import BackendError, for_platform
from .collect import SIGNIN_WARN_DAYS, Collector
from .sessions import running_sessions

BOLD, DIM, RED, GREEN, YELLOW, RESET = (
    "\033[1m", "\033[2m", "\033[31m", "\033[32m", "\033[33m", "\033[0m")


def _colour(stream=None) -> bool:
    return (stream or sys.stdout).isatty()


def _paint(text: str, code: str) -> str:
    return f"{code}{text}{RESET}" if _colour() else text


def _emit(payload, as_json: bool) -> None:
    if as_json:
        json.dump(payload, sys.stdout, indent=2, default=str)
        sys.stdout.write("\n")


def _strip(text: str) -> str:
    """Text without colour codes, for width calculations."""
    return re.sub(r"\033\[[0-9;]*m", "", text)


def _home(path: str | None) -> str:
    """Shorten a path for display, so the useful end is not pushed off the line."""
    if not path:
        return ""
    home = str(Path.home())
    return "~" + path[len(home):] if path.startswith(home) else path


def _pct(value) -> str:
    return "—" if value is None else f"{round(value * 100)}%"


def _tokens(count: int) -> str:
    """Readable token counts. 225000 is 225K, not a misleading 0M."""
    for limit, suffix in ((1e9, "B"), (1e6, "M"), (1e3, "K")):
        if count >= limit:
            return f"{count / limit:,.1f}{suffix}"
    return str(count)


def _clock(epoch) -> str:
    if not epoch:
        return "—"
    when = time.localtime(epoch)
    same_day = time.strftime("%F", when) == time.strftime("%F", time.localtime())
    return time.strftime("%H:%M" if same_day else "%a %H:%M", when)


def _status_word(account: dict) -> str:
    if account.get("error"):
        return "unavailable"
    usage = account.get("usage")
    if not usage:
        return "unknown"
    return "limited" if usage.get("limited") else "available"


# --- commands -------------------------------------------------------------

def cmd_list(args) -> int:
    snapshot = Collector(interval=0).build()
    if args.json:
        _emit(snapshot, True)
        return 0

    accounts = snapshot.get("accounts") or []
    if not accounts:
        print(snapshot.get("error") or "No accounts found.")
        return 1

    print(f"{'ALIAS':<10} {'PLAN':<6} {'5-HOUR':>7} {'7-DAY':>7}  "
          f"{'STATUS':<12} {'SIGN-IN':>8}  ACCOUNT")
    for account in accounts:
        usage = account.get("usage") or {}
        status = _status_word(account)
        days = account.get("signin_days_left")
        due = "—" if days is None else (f"{days:.0f}d" if days > 0 else "OVERDUE")
        if days is not None and days <= SIGNIN_WARN_DAYS:
            due = _paint(due, RED)
        if status == "available":
            status_text = _paint(status, GREEN)
        elif status == "limited":
            status_text = _paint(status, RED)
        else:
            status_text = _paint(status, YELLOW)

        blocked = usage.get("blocked_models") or []
        if blocked and status == "available":
            # Overall quota is fine but one model family is spent, which is the
            # difference between "usable" and "usable for what you wanted".
            status_text = _paint(f"no {blocked[0]}", YELLOW)
            status = f"no {blocked[0]}"

        marker = "*" if account.get("is_active") else " "
        pad = len(status_text) - len(status)
        print(f"{marker}{account['alias']:<9} {(account.get('plan') or '?'):<6} "
              f"{_pct(usage.get('used_5h')):>7} {_pct(usage.get('used_7d')):>7}  "
              f"{status_text:<{12 + pad}} {due:>8}  {account.get('email') or '?'}")

    sessions = snapshot.get("sessions") or 0
    print()
    print(_paint(f"{sessions} Claude session(s) running.", DIM))
    soon = [a for a in accounts if a.get("signin_due_soon")]
    if soon:
        names = ", ".join(a["alias"] for a in soon)
        print(_paint(f"Browser sign-in needed soon: {names}", RED))
    return 0


def cmd_show(args) -> int:
    snapshot = Collector(interval=0).build()
    match = next((a for a in snapshot.get("accounts") or []
                  if a["alias"] == args.alias), None)
    if match is None:
        print(f"No account named {args.alias!r}", file=sys.stderr)
        return 1
    if args.json:
        _emit(match, True)
        return 0

    usage = match.get("usage") or {}
    print(f"{_paint(match['alias'], BOLD)}  {match.get('email') or '?'}")
    print(f"  organisation   {match.get('org') or '?'}")
    print(f"  plan           {match.get('plan') or '?'}")
    print(f"  status         {_status_word(match)}")
    print(f"  5-hour         {_pct(usage.get('used_5h'))} · resets {_clock(usage.get('reset_5h'))}")
    print(f"  7-day          {_pct(usage.get('used_7d'))} · resets {_clock(usage.get('reset_7d'))}")
    for limit in usage.get("scoped") or []:
        flag = _paint("  exhausted", RED) if limit["exhausted"] else ""
        print(f"  {limit['model'] + ' (weekly)':<14} {_pct(limit['used'])}{flag}")
    if usage.get("binding"):
        print(f"  binding limit  {usage['binding'].replace('_', ' ')}")
    if match.get("access_hours_left") is not None:
        print(f"  access token   {match['access_hours_left']:.1f}h left")
    if match.get("signin_days_left") is not None:
        print(f"  sign-in due    in {match['signin_days_left']:.0f} days")
    if usage.get("overage_status"):
        state = usage["overage_status"]
        reason = (usage.get("overage_reason") or "").replace("_", " ")
        print(f"  extra usage    {state}{f' ({reason})' if reason else ''}")
    if match.get("error"):
        print(f"  error          {match['error']}")
    return 0


def cmd_verify(args) -> int:
    backend = for_platform()
    try:
        result = actions.verify(backend, args.alias)
    except actions.ActionError as exc:
        if args.json:
            _emit({"alias": args.alias, "ok": False, "error": str(exc)}, True)
        else:
            print(_paint(f"✗ {exc}", RED), file=sys.stderr)
        return 1
    if args.json:
        _emit(result, True)
    else:
        print(_paint(f"✓ {args.alias} is live (checked against the API just now)", GREEN))
    return 0


def cmd_use(args) -> int:
    """Replace this shell with a session pinned to the account."""
    backend = for_platform()
    try:
        actions.exec_session(backend, args.alias, args.args)
    except actions.ActionError as exc:
        print(_paint(f"✗ {exc}", RED), file=sys.stderr)
        return 1
    except OSError as exc:
        print(_paint(f"✗ could not start claude: {exc}", RED), file=sys.stderr)
        return 1
    return 0  # not reached: the process has been replaced


def cmd_window(args) -> int:
    backend = for_platform()
    try:
        result = actions.launch(backend, args.alias)
    except actions.ActionError as exc:
        print(_paint(f"✗ {exc}", RED), file=sys.stderr)
        return 1
    if args.json:
        _emit(result, True)
    else:
        print(f"Opened a {result['terminal']} window pinned to {args.alias}.")
    return 0


def cmd_switch(args) -> int:
    """Change the machine-wide default. The dangerous one."""
    backend = for_platform()
    sessions = running_sessions()
    if not args.yes:
        print(f"Switching the default to {args.alias} retargets {sessions} running "
              f"session(s) mid-conversation.")
        print(f"To pin one session instead, leaving the others alone: "
              f"hotseat use {args.alias}")
        if not sys.stdin.isatty():
            print("Refusing without --yes.", file=sys.stderr)
            return 1
        if input("Switch anyway? [y/N] ").strip().lower() not in ("y", "yes"):
            print("Cancelled.")
            return 1
    try:
        result = actions.switch(backend, args.alias, sessions, acknowledged=True)
    except actions.ActionError as exc:
        print(_paint(f"✗ {exc}", RED), file=sys.stderr)
        return 1
    if args.json:
        _emit(result, True)
    else:
        print(f"Default account is now {args.alias}.")
    return 0


def cmd_models(args) -> int:
    usage = modelstats.recent_by_model(days=args.days)
    if usage is None:
        print("No local usage statistics found.", file=sys.stderr)
        return 1
    if args.json:
        _emit(usage, True)
        return 0

    total = usage["total_tokens"]
    print(f"Last {usage['days']} days, every account on this machine: "
          f"{_tokens(total)} tokens")
    if usage.get("stale"):
        print(_paint(f"Counts run to {usage['as_of']}; today is not included yet.", DIM))
    print()
    for model in usage["models"]:
        # A share too small to fill a block still gets a tick, so it is visibly
        # present rather than appearing to be zero.
        bar = "█" * round(model["share"] * 28) or "▏"
        share = model["share"] * 100
        print(f"  {model['label']:<14} {_tokens(model['tokens']):>8} "
              f"{share:5.1f}%  {bar}" if share >= 0.1 else
              f"  {model['label']:<14} {_tokens(model['tokens']):>8} "
              f"{'<0.1':>5}%  {bar}")
    return 0


def cmd_codex_account(args) -> int:
    """Run the packaged Codex profile helper with this Python interpreter."""
    return subprocess.call(bundled.command("hotseat.codex_accounts", *args.args))


def cmd_codex(args) -> int:
    """Codex accounts and their quota, alongside the Claude ones."""
    try:
        overview = codex.overview(with_limits=not args.no_quota)
    except codex.CodexError as exc:
        print(f"✗ {exc}", file=sys.stderr)
        return 1
    if args.json:
        _emit(overview, True)
        return 0

    entries = overview["accounts"]
    if not entries:
        print("No Codex accounts found.")
        return 1

    print(f"{'PROFILE':<14} {'WORST':>6}  {'STATUS':<9} {'TOKEN':>9}  ACCOUNT")
    for entry in entries:
        usage_row = entry.get("usage") or {}
        worst = usage_row.get("worst_used")
        used = f"{round(worst * 100)}%" if worst is not None else "—"
        if usage_row.get("error"):
            word, colour = "error", YELLOW
        elif not usage_row:
            word, colour = "unknown", DIM
        elif usage_row.get("usable"):
            word, colour = "usable", GREEN
        else:
            word, colour = "blocked", RED

        hours = entry.get("token_hours_left")
        if hours is None:
            token = "—"
        elif hours <= 0:
            # A saved profile refreshes on activation, so an expired stored token
            # is normal rather than a problem to flag.
            token = "expired"
        else:
            token = f"{hours:.1f}h"

        marker = "*" if entry.get("is_active") else " "
        name = "live (unsaved)" if not entry["saved"] else entry["alias"]
        print(f"{marker}{name:<13} {used:>6}  {_paint(word, colour)}{' ' * (9 - len(word))} "
              f"{token:>9}  {entry.get('email') or '?'}")

    print()
    if overview.get("unsaved_live"):
        print(_paint(f"! The live account ({overview['unsaved_live']}) is not saved as a "
                     f"profile. Switching profiles would lose it.", YELLOW))
        print(_paint("  Save it first: hotseat codex-account save <name>", DIM))
    if not overview.get("quota_read"):
        print(_paint("Quota could not be read; showing accounts only.", DIM))
    age = overview.get("quota_age_s")
    if age and age > 60:
        print(_paint(f"Quota figures are {age / 60:.0f} minutes old. Each refresh "
                     f"starts one process per account, so they are not re-read on "
                     f"a timer.", DIM))
    return 0


def cmd_sessions(args) -> int:
    """Recent Codex sessions, and what each one needs."""
    try:
        found = codexsessions.recent(limit=args.limit)
    except codexsessions.SessionError as exc:
        print(f"✗ {exc}", file=sys.stderr)
        return 1
    if args.json:
        _emit(found, True)
        return 0
    if not found:
        print("No recent Codex sessions.")
        return 0

    tone = {"working": GREEN, "stuck": RED, "failed": YELLOW, "idle": DIM, "closed": DIM}
    print(f"{'ID':<10}{'STATE':<9}{'LAST':<14}{'TURNS':>6}{'FAILED':>7}  TOPIC")
    for entry in found:
        when = time.strftime("%d %b %H:%M", time.localtime(entry["last_at"]))
        state = _paint(entry["state"], tone.get(entry["state"], DIM))
        pad = len(state) - len(entry["state"])
        print(f"{entry['short']:<10}{state:<{9 + pad}}{when:<14}"
              f"{entry['turns']:>6}{entry['failed']:>7}  {entry['topic'][:52]}")

    stuck = [e for e in found if e["state"] == "stuck"]
    print()
    if stuck:
        print(_paint(f"{len(stuck)} stuck: the holder keeps failing and nothing else can "
                     f"take the conversation.", YELLOW))
        print(f"  Reboot one: {_paint('hotseat reboot ' + stuck[0]['short'], BOLD)}")
    print(_paint("Nudge a live one: hotseat nudge <id> \"continue\"", DIM))
    return 0


def cmd_nudge(args) -> int:
    """Hand a message to a running session without opening a terminal."""
    try:
        entry = codexsessions.resolve(args.id)
        if not entry["live"]:
            print(f"✗ {entry['short']} is not running, so it cannot accept a message. "
                  f"Reboot it instead: hotseat reboot {entry['short']}", file=sys.stderr)
            return 1
        result = codexsessions.nudge(entry["thread_id"], args.message)
    except codexsessions.SessionError as exc:
        print(f"✗ {exc}", file=sys.stderr)
        return 1
    if args.json:
        _emit(result, True)
    else:
        print(_paint(f"✓ queued for {entry['short']}: {args.message}", GREEN))
    return 0


def cmd_reboot(args) -> int:
    """Close a stuck session and resume the same conversation."""
    try:
        entry = codexsessions.resolve(args.id)
    except codexsessions.SessionError as exc:
        print(f"✗ {exc}", file=sys.stderr)
        return 1

    if entry["live"] and not args.yes:
        print(f"{entry['short']} is held by process(es) "
              f"{', '.join(str(p) for p in entry['holders'])}.")
        print("Rebooting closes that session. Its history is kept; its scrollback is not.")
        if not sys.stdin.isatty():
            print("Refusing without --yes.", file=sys.stderr)
            return 1
        if input("Close and resume? [y/N] ").strip().lower() not in ("y", "yes"):
            print("Cancelled.")
            return 1

    closed = []
    try:
        if entry["live"]:
            closed = codexsessions.release(entry["thread_id"])
            _wait_for_lock(entry["thread_id"])
        result = actions.launch_command(
            ["codex", "resume", entry["thread_id"]], cwd=args.cwd)
    except (codexsessions.SessionError, actions.ActionError) as exc:
        print(f"✗ {exc}", file=sys.stderr)
        return 1

    if args.json:
        _emit({"thread_id": entry["thread_id"], "closed": closed, **result}, True)
    else:
        if closed:
            print(f"Closed {', '.join(str(p) for p in closed)}.")
        print(_paint(f"✓ resumed {entry['short']} in a new {result['terminal']} window",
                     GREEN))
        if args.message:
            print(_paint("  waiting for it to attach before sending the message…", DIM))
    if args.message:
        return _send_after_attach(entry, args.message)
    return 0


def _wait_for_lock(thread_id: str, seconds: float = 10.0) -> bool:
    """Wait for the writer lock to clear, so the resume is not refused."""
    deadline = time.time() + seconds
    while time.time() < deadline:
        if not codexsessions.lock_holders(thread_id):
            return True
        time.sleep(0.5)
    return False


def _send_after_attach(entry: dict, message: str, seconds: float = 45.0) -> int:
    """Queue a message once the resumed session has taken the lock."""
    deadline = time.time() + seconds
    while time.time() < deadline:
        if codexsessions.lock_holders(entry["thread_id"]):
            try:
                codexsessions.nudge(entry["thread_id"], message)
            except codexsessions.SessionError as exc:
                print(f"✗ resumed, but the message did not send: {exc}", file=sys.stderr)
                return 1
            print(_paint(f"✓ sent: {message}", GREEN))
            return 0
        time.sleep(1.0)
    print("✗ resumed, but it did not attach in time to accept the message.",
          file=sys.stderr)
    return 1






def cmd_resume(args) -> int:
    """List stopped native sessions; plugins may extend this command."""
    items = resume.stopped(window_days=args.days)
    if args.kind:
        items=[i for i in items if i["kind"]==args.kind]
    if args.cause:
        items=[i for i in items if i.get("cause")==args.cause]
    snapshot=Collector(interval=0).build()
    accounts=snapshot.get("accounts") or []
    default=next((a["alias"] for a in accounts if a.get("is_active")),None)
    for item in items:item["readiness"]=resume.readiness(item,accounts,default)
    if args.json:
        result={"items":items,"default_account":default}
        if args.go:result["results"]=[resume.continue_item(i) for i in items if i["readiness"]["ready"]]
        _emit(result,True)
    else:
        for item in items:
            print(f"{item['name']} · {item.get('cwd') or ''}")
            print(f"  claude --resume {item['id']}")
        if not items:print("No stopped native sessions found.")
        if args.go:print("Native sessions require a terminal; use a resume command above.")
    return 0


def cmd_inspect(args) -> int:
    """What one stopped piece of work was doing, so you can decide about it."""
    try:
        detail = inspect_mod.detail(args.id)
    except inspect_mod.InspectError as exc:
        print(f"✗ {exc}", file=sys.stderr)
        return 1
    if args.json:
        _emit(detail, True)
        return 0

    print(f"{_paint(detail['name'], BOLD)}  {detail['kind']} · {detail.get('backend') or '?'}"
          + (f" · {detail['model']}" if detail.get("model") else ""))
    if detail.get("cwd"):
        branch = f" ({detail['branch']})" if detail.get("branch") else ""
        print(f"  {_paint('working in', DIM)} {detail['cwd']}{branch}")
    if detail.get("last_activity"):
        print(f"  {_paint('last activity', DIM)} {detail['last_activity']}")
    print()

    if detail.get("summary"):
        print(_paint("What it was working on", BOLD))
        print(f"  {detail['summary']}")
        print()
    if detail.get("last_user"):
        print(_paint("Last asked", BOLD))
        print(f"  {detail['last_user']}")
        print()
    if detail.get("last_assistant"):
        print(_paint("Last said", BOLD))
        print(f"  {detail['last_assistant']}")
        print()
    for item in detail.get("queued") or []:
        print(_paint(f"Queued and waiting ({item['origin'] or 'unknown'})", YELLOW))
        print(f"  {item['text']}")
        print()
    if args.full:
        print(_paint("Recent exchange", BOLD))
        for turn in detail.get("recent") or []:
            who = "you" if turn["role"] == "user" else detail["name"]
            print(f"  {_paint(who + ':', DIM)} {turn['text']}")
    return 0


def cmd_statusline(args) -> int:
    line = statusline.render()
    if line:
        print(line)
    return 0


def cmd_serve(args) -> int:
    from .server import build_server
    import webbrowser

    collector = Collector(interval=args.interval)
    try:
        server = build_server(args.host, args.port, collector,
                              idle_exit_s=args.idle_exit)
    except OSError as exc:
        print(f"cannot listen on {args.host}:{args.port}: {exc}", file=sys.stderr)
        return 1

    url = f"http://{args.host}:{args.port}/"
    print(f"dashboard at {url}")
    if args.idle_exit:
        print(f"exits after {args.idle_exit // 60} minutes with no requests")
    if args.host != "127.0.0.1":
        print(_paint("warning: not bound to loopback; anyone who can reach this port "
                     "can act on your accounts", YELLOW))
    if not args.no_browser:
        webbrowser.open(url)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print()
    finally:
        collector.stop()
        server.server_close()
    return 0


# --- wiring ---------------------------------------------------------------

def cmd_resets(args) -> int:
    """Read banked Codex usage resets; never redeem them."""
    from .resetcredits import balances
    try:
        rows = balances(args.aliases)
    except ValueError as exc:
        print(str(exc), file=sys.stderr)
        return 1
    if args.json:
        _emit({"accounts": rows}, True)
    else:
        print(f"{'PROFILE':<26} {'RESETS':>6}  STATUS")
        for row in rows:
            count = str(row["available_count"]) if row["available_count"] is not None else "—"
            print(f"{row['alias']:<26} {count:>6}  {row['error'] or 'ok'}")
    return 1 if any(row["error"] for row in rows) else 0


RELEASES = "https://github.com/Maxteabag/hotseat/releases/download"


def _tui_platform() -> tuple[str, str] | None:
    """(GOOS, GOARCH) for a release binary that runs here, or None."""
    system = {"linux": "linux", "darwin": "darwin"}.get(sys.platform)
    machine = platform.machine().lower()
    arch = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(machine)
    return (system, arch) if system and arch else None


def _tui_cache_dir(env: dict) -> Path:
    base = env.get("XDG_DATA_HOME") or str(Path(env.get("HOME", str(Path.home()))) / ".local" / "share")
    return Path(base) / "hotseat" / "bin"


def _fetch(url: str) -> bytes:
    with urllib.request.urlopen(url, timeout=60) as response:  # noqa: S310 - fixed https host
        return response.read()


def find_tui_binary(package_dir: Path | None = None, env: dict | None = None) -> str | None:
    """Locate the Go interface, preferring the copy shipped inside the wheel.

    Order: HOTSEAT_TUI_BIN, the binary bundled in hotseat/_bin (platform wheels),
    bin/hotseat-tui in a source checkout, a release binary downloaded earlier for
    this exact version, then anything on PATH.
    """
    env = os.environ if env is None else env
    package_dir = Path(__file__).resolve().parent if package_dir is None else package_dir
    suffix = ".exe" if sys.platform == "win32" else ""
    candidates = [env.get("HOTSEAT_TUI_BIN"),
                  package_dir / "_bin" / f"hotseat-tui{suffix}",
                  package_dir.parent / "bin" / f"hotseat-tui{suffix}"]
    target = _tui_platform()
    if target:
        candidates.append(_tui_cache_dir(env) / f"hotseat-tui-{__version__}-{target[0]}-{target[1]}")
    for candidate in candidates:
        if candidate and Path(candidate).is_file():
            return str(candidate)
    return shutil.which("hotseat-tui", path=env.get("PATH"))


def download_tui_binary(env: dict | None = None, fetch=None) -> str:
    """Fetch this version's release binary, verify its SHA-256, and cache it.

    Used when an install carries no binary (sdist, git install). The checksum
    comes from the release's SHA256SUMS; a mismatch leaves nothing behind.
    """
    env = os.environ if env is None else env
    fetch = _fetch if fetch is None else fetch
    target = _tui_platform()
    if not target:
        raise RuntimeError(f"no prebuilt hotseat-tui for {sys.platform}/{platform.machine()}")
    name = f"hotseat-tui-{__version__}-{target[0]}-{target[1]}"
    base = f"{RELEASES}/v{__version__}"
    expected = None
    for line in fetch(f"{base}/SHA256SUMS").decode().splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[1].lstrip("*") == name:
            expected = parts[0].lower()
    if not expected:
        raise RuntimeError(f"{name} is not listed in the v{__version__} release checksums")
    data = fetch(f"{base}/{name}")
    actual = hashlib.sha256(data).hexdigest()
    if actual != expected:
        raise RuntimeError(f"checksum mismatch for {name}: expected {expected}, got {actual}")
    cache = _tui_cache_dir(env)
    cache.mkdir(parents=True, exist_ok=True)
    final = cache / name
    with tempfile.NamedTemporaryFile(dir=cache, prefix=name + ".", delete=False) as handle:
        handle.write(data)
        temp = Path(handle.name)
    temp.chmod(0o755)
    temp.replace(final)
    return str(final)


def cmd_tui(args) -> int:
    """Run the Go interface while preserving this Python environment."""
    root = Path(__file__).resolve().parent.parent
    binary = find_tui_binary()
    if not binary and not os.environ.get("HOTSEAT_NO_DOWNLOAD"):
        target = _tui_platform()
        if target:
            print(_paint(f"Fetching hotseat-tui {__version__} for {target[0]}/{target[1]} from GitHub releases…", DIM),
                  file=sys.stderr)
            try:
                binary = download_tui_binary()
            except (OSError, RuntimeError) as exc:
                print(_paint(f"✗ download failed: {exc}", RED), file=sys.stderr)
    if not binary:
        print("No hotseat-tui binary found. Platform wheels bundle it; from a checkout run: "
              "go build -o bin/hotseat-tui ./cmd/hotseat-tui, or set HOTSEAT_TUI_BIN.", file=sys.stderr)
        return 1
    command = [binary, "--python", sys.executable, "--backend-root", str(root)]
    if args.demo:
        command.append("--demo")
    return subprocess.call(command)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="hotseat",
        description="Inspect and use the Claude Code and Codex accounts on this machine.")
    parser.add_argument("--version", action="version", version=f"hotseat {__version__}")
    sub = parser.add_subparsers(dest="command")

    nodes = {}
    def add(name, handler, help_text, json_flag=True):
        node = sub.add_parser(name, help=help_text)
        node.set_defaults(handler=handler)
        nodes[name]=node
        if json_flag:
            node.add_argument("--json", action="store_true",
                              help="Machine-readable output")
        return node

    tui_node = add("tui", cmd_tui, "Open the Bubble Tea account desk", json_flag=False)
    tui_node.add_argument("--demo", action="store_true", help="Synthetic data; no account actions")

    add("list", cmd_list, "Show every account, its quota and its sign-in deadline")
    add("show", cmd_show, "Everything known about one account").add_argument("alias")
    add("verify", cmd_verify, "Prove an account works by calling the API").add_argument("alias")

    use = add("use", cmd_use, "Run a session pinned to an account, in this terminal",
              json_flag=False)
    use.add_argument("alias")
    use.add_argument("args", nargs=argparse.REMAINDER,
                     help="Arguments passed through to claude")

    add("window", cmd_window, "Open a new terminal window pinned to an account").add_argument("alias")

    switch = add("switch", cmd_switch,
                 "Change the machine-wide default account (affects running sessions)")
    switch.add_argument("alias")
    switch.add_argument("--yes", action="store_true", help="Skip the confirmation")

    models = add("models", cmd_models, "Token usage by model, across all accounts")
    models.add_argument("--days", type=int, default=modelstats.DEFAULT_DAYS)

    inspect_node = add("inspect", cmd_inspect,
                       "What a stopped agent or session was working on")
    inspect_node.add_argument("id", help="Session slug or transcript id, from `resume`")
    inspect_node.add_argument("--full", action="store_true",
                              help="Also show the last few exchanges")

    resume_node = add("resume", cmd_resume,
                      "List work stopped by a usage limit, and continue it")
    resume_node.add_argument("--go", action="store_true",
                             help="Continue ready work through installed plugins")
    resume_node.add_argument("--kind", help="native, or a source provided by an installed plugin")
    resume_node.add_argument("--cause", choices=("usage_limit", "queue_paused"),
                             help="Only this stop cause")
    resume_node.add_argument("--days", type=int, default=resume.DEFAULT_WINDOW_DAYS,
                             help="How far back to look")

    sessions_node = add("sessions", cmd_sessions, "Recent Codex sessions and their state")
    sessions_node.add_argument("--limit", type=int, default=codexsessions.DEFAULT_LIMIT)

    nudge_node = add("nudge", cmd_nudge, "Send a message to a running Codex session")
    nudge_node.add_argument("id", help="Session id or prefix, from `sessions`")
    nudge_node.add_argument("message")

    reboot_node = add("reboot", cmd_reboot,
                      "Close a stuck Codex session and resume the same conversation")
    reboot_node.add_argument("id", help="Session id or prefix, from `sessions`")
    reboot_node.add_argument("--message", help="Send this once it has attached")
    reboot_node.add_argument("--cwd", help="Directory to resume in")
    reboot_node.add_argument("--yes", action="store_true", help="Skip the confirmation")

    resets_node = add("resets", cmd_resets, "Read available Codex usage-reset credits (no redemption)")
    resets_node.add_argument("aliases", nargs="*", help="Codex profiles; defaults to all")

    account_node = add("codex-account", cmd_codex_account,
                       "Codex profiles: list, current, save, switch, login, verify, probe, remove", json_flag=False)
    account_node.add_argument("args", nargs=argparse.REMAINDER, help="Arguments for the bundled profile helper")

    codex_node = add("codex", cmd_codex, "Codex accounts and their quota")
    codex_node.add_argument("--no-quota", action="store_true",
                            help="Skip the live quota probe, which is the slow part")


    add("statusline", cmd_statusline, "Print the current account, for a status line",
        json_flag=False)

    serve = add("serve", cmd_serve, "Start the web dashboard", json_flag=False)
    serve.add_argument("--host", default="127.0.0.1",
                       help="Interface to bind. Loopback by default; anything else "
                            "exposes account controls to your network.")
    serve.add_argument("--port", type=int, default=8787)
    serve.add_argument("--interval", type=int, default=300,
                       help="How stale a snapshot may be before a request rebuilds it")
    serve.add_argument("--idle-exit", type=int, default=1800,
                       help="Exit after this many seconds with no requests (0 to stay up)")
    serve.add_argument("--no-browser", action="store_true")
    from . import plugins
    plugins.configure_cli(add,nodes)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if not getattr(args, "command", None):
        parser.print_help()
        return 2
    try:
        return args.handler(args)
    except BackendError as exc:
        print(_paint(f"✗ {exc}", RED), file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        return 130
