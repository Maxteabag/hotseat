"""The things the dashboard is allowed to do, and the guards around them.

Safety model:
  verify  - one API request. Touches nothing.
  launch  - starts a session pinned to one account by environment variable. It
            writes no credential file, so other sessions are unaffected.
  switch  - changes the machine-wide default. This is the dangerous one: every
            running session reads the same credential file, so switching
            retargets all of them. Guarded, and never silent.
  login   - hands off to the official interactive sign-in.

Tokens never appear in a response body, a log line, or a command line. `launch`
passes the token through the child process environment only.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import tempfile
import time

from . import ratelimits
from . import sessiondirs
from .backends import BackendError

#: Terminals this knows how to launch a command in. Order is only the last-resort
#: tiebreak: picking whatever is first on PATH opens a terminal the user may not
#: even use, which is worse than useless when they are watching for a window.
TERMINALS = ("ghostty", "kitty", "alacritty", "wezterm", "foot",
             "gnome-terminal", "konsole", "xfce4-terminal", "xterm")
#: The XDG default-terminal launcher. Honouring it is better than guessing: it
#: opens whichever terminal the user actually set as their default, and it takes
#: the command directly rather than behind -e.
XDG_LAUNCHER = "xdg-terminal-exec"
#: How each one takes "run this command". Most use -e.
TERMINAL_EXEC_FLAG = {"gnome-terminal": "--", "xfce4-terminal": "-x",
                      XDG_LAUNCHER: "--"}


def _terminal_flag(terminal: str) -> str:
    return TERMINAL_EXEC_FLAG.get(os.path.basename(terminal), "-e")


def _process_name(pid: int) -> str:
    try:
        return (Path(f"/proc/{pid}/comm").read_text().strip())
    except OSError:
        return ""


def _parent_of(pid: int) -> int:
    try:
        for line in Path(f"/proc/{pid}/status").read_text().splitlines():
            if line.startswith("PPid:"):
                return int(line.split()[1])
    except (OSError, ValueError, IndexError):
        pass
    return 0


def _terminal_from_ancestors() -> str | None:
    """The terminal this process is actually running inside, if any."""
    pid = os.getpid()
    for _ in range(12):  # shells nest; a terminal is never far up
        pid = _parent_of(pid)
        if pid <= 1:
            return None
        name = _process_name(pid)
        if name in TERMINALS and shutil.which(name):
            return name
    return None


def _running_terminal() -> str | None:
    """A terminal emulator already running for this user.

    This is the one that catches the case that matters: a detached server has no
    terminal ancestor, and the first entry on PATH may be an emulator that is
    installed but never used. What is already running is what the user is looking at.
    """
    try:
        listing = subprocess.run(["ps", "-u", str(os.getuid()), "-o", "comm="],
                                 capture_output=True, text=True, timeout=15,
                                 check=False).stdout
    except (OSError, subprocess.SubprocessError):
        return None
    running = {line.strip() for line in listing.splitlines()}
    for candidate in TERMINALS:
        if candidate in running and shutil.which(candidate):
            return candidate
    return None


def detect_terminal() -> str | None:
    """Pick the terminal to open a window in, most specific evidence first.

    Order matters. A terminal that is merely installed is the worst guess: it can
    open a window in an emulator the user never uses, which looks like nothing
    happened at all.
    """
    preferred = os.environ.get("HOTSEAT_TERMINAL") or os.environ.get("TERMINAL")
    if preferred and shutil.which(preferred):
        return preferred
    if shutil.which(XDG_LAUNCHER):
        return XDG_LAUNCHER
    return (_terminal_from_ancestors()
            or _running_terminal()
            or next((t for t in TERMINALS if shutil.which(t)), None))


class ActionError(RuntimeError):
    """An action was refused or could not be completed."""


def _find_account(backend, alias: str):
    for account in backend.accounts():
        if account.alias == alias:
            return account
    raise ActionError(f"No account named {alias!r}")


def verify(backend, alias: str) -> dict:
    """Prove an account works. No local check can establish this."""
    account = _find_account(backend, alias)
    if not account.token:
        raise ActionError(f"{alias} has no stored token")
    try:
        summary = ratelimits.summarise(ratelimits.probe(account.token))
    except ratelimits.ProbeError as exc:
        raise ActionError(f"{alias} did not answer: {exc}") from exc
    return {"alias": alias, "ok": True, "usage": summary}


#: Variables that must not reach a launched session. An API key outranks account
#: credentials and would bill the wrong one; the CLAUDE_CODE_* set makes a fresh
#: session believe it is a nested child of whatever started this dashboard.
BLOCKED_ENV = (
    "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
    "CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_CHILD_SESSION",
    "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_EXECPATH", "CLAUDE_CODE_SESSION_ATTENDED",
    "CLAUDE_CODE_ATTRIBUTION_HEADER", "CLAUDE_CODE_MESSAGING_SOCKET",
    "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_PID", "CLAUDE_EFFORT",
)


def _clean_env(config_dir: Path) -> dict:
    env = {k: v for k, v in os.environ.items() if k not in BLOCKED_ENV}
    env["CLAUDE_CONFIG_DIR"] = str(config_dir)
    return env


def _credentials_for(backend, account) -> tuple[dict, dict | None]:
    """Fetch the full stored credentials and identity for an account.

    Falls back to what the account object carries, so a backend that exposes only
    a token still produces a usable session.
    """
    fetch = getattr(backend, "raw_credentials", None)
    if callable(fetch):
        found = fetch(account.alias)
        if found:
            return found
    credentials = {"claudeAiOauth": {
        "accessToken": account.token,
        "expiresAt": account.access_expires_at,
        "refreshTokenExpiresAt": account.refresh_expires_at,
        "subscriptionType": account.plan,
    }}
    identity = {"emailAddress": account.email, "organizationName": account.org,
                "organizationUuid": account.org_uuid}
    return credentials, identity


def _write_launcher(config_dir: Path) -> Path:
    """Write a script that points one session at one config directory.

    A terminal in single-instance mode hands the command to an existing daemon, and
    what that daemon does with a caller's environment varies. Setting the variable
    inside the script removes the question. It holds no secret: only a path.
    """
    work = Path(tempfile.mkdtemp(prefix="hotseat-launch-"))
    work.chmod(0o700)
    script = work / "run.sh"
    script.write_text(
        "#!/bin/sh\n"
        "# Starts a Claude session pinned to one account. Generated by hotseat.\n"
        f"unset {' '.join(BLOCKED_ENV)}\n"
        'PATH="${PATH:-/usr/bin:/bin}:/usr/bin:/bin"\n'
        f'CLAUDE_CONFIG_DIR="{config_dir}"\n'
        "export CLAUDE_CONFIG_DIR\n"
        "exec claude \"$@\"\n")
    script.chmod(0o700)
    return script


def _sweep_old_launchers(older_than_s: int = 3600) -> None:
    """Remove leftovers from launches that never ran. Never fails the launch."""
    root = Path(tempfile.gettempdir())
    cutoff = time.time() - older_than_s
    for stale in root.glob("hotseat-launch-*"):
        try:
            if stale.is_dir() and stale.stat().st_mtime < cutoff:
                shutil.rmtree(stale, ignore_errors=True)
        except OSError as exc:
            print(f"hotseat: could not clear {stale.name}: {exc}")


def prepare(backend, alias: str) -> tuple[Path, dict]:
    """Set up an account's session directory and return it with a clean environment.

    This is the primitive both front ends share. The command line execs a session
    in place with it; the dashboard hands it to a new terminal window.

    The session gets its own config directory holding that account's credentials.
    That is what makes the account visible from inside the session, and it lets the
    session refresh its own token. Other running sessions read the shared credential
    file and are unaffected.
    """
    account = _find_account(backend, alias)
    if not account.token:
        raise ActionError(f"{alias} has no stored token")
    credentials, identity = _credentials_for(backend, account)
    config_dir = sessiondirs.ensure(account, credentials, identity)
    return config_dir, _clean_env(config_dir)


def exec_session(backend, alias: str, argv: list[str] | None = None,
                 execute=None) -> None:
    """Replace this process with a session pinned to the account.

    The command line's launch path. Nothing is spawned and no terminal is opened:
    the shell that ran hotseat becomes the pinned session.
    """
    config_dir, env = prepare(backend, alias)
    execute = execute or os.execvpe
    _ = config_dir
    execute("claude", ["claude", *(argv or [])], env)


def launch(backend, alias: str, spawn=None, terminal: str | None = None) -> dict:
    """Open a NEW terminal window running a session pinned to this account.

    Used by the dashboard, which has no terminal of its own to hand over.
    """
    account = _find_account(backend, alias)
    if not account.token:
        raise ActionError(f"{alias} has no stored token")

    spawn = spawn or subprocess.Popen
    terminal = terminal or detect_terminal()
    if terminal is None:
        raise ActionError("No supported terminal found. Set HOTSEAT_TERMINAL to the "
                          "one you use, or start a pinned session from the command "
                          "line with `hotseat use <alias>`.")

    config_dir, env = prepare(backend, alias)

    _sweep_old_launchers()
    script = _write_launcher(config_dir)
    argv = [terminal, _terminal_flag(terminal), str(script)]

    try:
        spawn(argv, env=env, start_new_session=True,
              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    except (OSError, subprocess.SubprocessError) as exc:
        shutil.rmtree(script.parent, ignore_errors=True)
        raise ActionError(f"Could not start {terminal}: {exc}") from exc
    return {"alias": alias, "terminal": terminal, "started": True,
            "config_dir": str(config_dir)}


def _atomic_write(path: Path, payload: bytes) -> None:
    tmp = None
    try:
        with tempfile.NamedTemporaryFile(dir=path.parent, delete=False) as handle:
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


def switch(backend, alias: str, sessions: int, acknowledged: bool) -> dict:
    """Change the machine-wide default account.

    Refuses unless the caller has acknowledged how many sessions this disturbs,
    so the dangerous path can never be taken by a stray click.
    """
    # The safety gate comes first, so an unacknowledged switch is refused the same
    # way everywhere rather than falling through to a platform message.
    if not acknowledged:
        raise ActionError(
            f"Switching retargets {sessions} running session(s). Confirm before proceeding.")
    if not getattr(backend, "can_switch", False) or not hasattr(backend, "profiles_dir"):
        raise ActionError("Switching the default account is not supported on this platform")

    target = backend.profiles_dir / alias
    source = target / "credentials.json"
    if not source.exists():
        raise ActionError(f"No saved profile named {alias!r}")

    live = backend.root / ".credentials.json"
    identity_file = target / "oauthAccount.json"

    # Snapshot the outgoing account first, but only into a profile that already
    # holds that same account. Writing it anywhere else destroys a live token.
    try:
        current = backend._active()
    except BackendError:
        current = None
    if current is not None and live.exists():
        for candidate in backend._profiles():
            same_account = (candidate.email and candidate.email == current.email
                            and candidate.org_uuid == current.org_uuid)
            if same_account:
                destination = backend.profiles_dir / candidate.alias / "credentials.json"
                _atomic_write(destination, live.read_bytes())
                break

    _atomic_write(live, source.read_bytes())

    if identity_file.exists():
        config = Path.home() / ".claude.json"
        try:
            data = json.loads(config.read_text())
            data["oauthAccount"] = json.loads(identity_file.read_text())
            _atomic_write(config, json.dumps(data, indent=2).encode())
        except (OSError, ValueError) as exc:
            raise ActionError(f"Switched credentials but could not update identity: {exc}") from exc

    marker = backend.profiles_dir / ".current_profile"
    try:
        marker.write_text(alias)
    except OSError as exc:
        raise ActionError(f"Switched, but the active-profile marker is now wrong: {exc}") from exc

    return {"alias": alias, "switched": True, "sessions_affected": sessions}


def launch_command(command: list[str], cwd: str | None = None,
                   spawn=None, terminal: str | None = None) -> dict:
    """Run a command in a new terminal window, in the terminal the user uses."""
    spawn = spawn or subprocess.Popen
    terminal = terminal or detect_terminal()
    if terminal is None:
        raise ActionError("No supported terminal found. Set HOTSEAT_TERMINAL to the "
                          "one you use.")
    inner = " ".join(shlex.quote(part) for part in command)
    where = f"cd {shlex.quote(cwd)} && " if cwd else ""
    argv = [terminal, _terminal_flag(terminal), "bash", "-lc", f"{where}exec {inner}"]
    try:
        spawn(argv, env=_clean_env(Path.home() / ".claude"), start_new_session=True,
              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    except (OSError, subprocess.SubprocessError) as exc:
        raise ActionError(f"Could not start {terminal}: {exc}") from exc
    return {"terminal": terminal, "started": True}


def login(alias: str | None = None) -> dict:
    """Hand off to the official interactive sign-in, in a terminal."""
    terminal = detect_terminal()
    if terminal is None:
        raise ActionError("No supported terminal found. Run `claude auth login` yourself.")
    argv = [terminal, _terminal_flag(terminal), "claude", "auth", "login"]
    try:
        subprocess.Popen(argv, start_new_session=True,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    except (OSError, subprocess.SubprocessError) as exc:
        raise ActionError(f"Could not start {terminal}: {exc}") from exc
    return {"started": True, "terminal": terminal}
