"""Recent Codex sessions, and getting a stuck one moving again.

Three mechanics decide what a session needs, and they are easy to confuse:

  * **A writer lock per thread.** Only one process may hold a conversation. A
    second `codex resume` of the same thread fails with "this conversation is
    open in another app" rather than taking over.
  * **Credentials are read once, at startup.** Switching the active Codex account
    never reaches a session that is already running, so a session started before
    the switch keeps failing on the old account no matter what is on disk.
  * **A running session accepts a message.** `codex queue` hands text to a live
    session by thread id, without a terminal and without typing anything.

So a live session is nudged, and a stale one is closed and resumed. Reading is
always safe; both actions are explicit.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
import re
import shutil
import sqlite3
import subprocess
import time

CODEX_HOME = Path(os.environ.get("CODEX_HOME") or Path.home() / ".codex")
HISTORY_DB = CODEX_HOME / "thread_history_1.sqlite"
LOCK_DIR = CODEX_HOME / "thread-writer-locks"
#: Timestamps in thread_turns are epoch seconds, not milliseconds.
TIMEOUT_S = 60
DEFAULT_LIMIT = 12
#: Statuses that mean the turn ended badly rather than finishing.
FAILED = {"failed", "error", "aborted"}
RUNNING = {"inprogress", "in_progress", "running", "started"}


class SessionError(RuntimeError):
    """Codex session history could not be read, or an action could not be run."""


def available() -> bool:
    return HISTORY_DB.exists()


def _walk_text(value) -> str:
    """The first value stored under a "text" key, at any depth.

    Only under that key: returning any string found would pick up the item type
    and other bookkeeping, which reads as a topic but is not one.
    """
    if isinstance(value, dict):
        candidate = value.get("text")
        if isinstance(candidate, str) and candidate.strip():
            return candidate
        for nested in value.values():
            found = _walk_text(nested)
            if found:
                return found
    elif isinstance(value, list):
        for nested in value:
            found = _walk_text(nested)
            if found:
                return found
    return ""


def _first_text(blob: str | None) -> str:
    """Pull readable text out of a stored item.

    Parsed as JSON rather than pattern-matched: the stored text is UTF-8 with JSON
    escapes, and decoding those by hand turns every apostrophe and dash into
    mojibake.
    """
    if not blob:
        return ""
    try:
        decoded = json.loads(blob)
    except (ValueError, TypeError):
        return ""
    return " ".join(_walk_text(decoded).split())


def lock_holders(thread_id: str) -> list[int]:
    """Process ids holding this thread's writer lock, if any.

    The lock files are empty and flock-based, so the holder is only discoverable
    through the kernel rather than by reading the file.
    """
    lock = LOCK_DIR / f"{thread_id}.lock"
    if not lock.exists() or not shutil.which("fuser"):
        return []
    try:
        result = subprocess.run(["fuser", str(lock)], capture_output=True,
                                text=True, timeout=20, check=False)
    except (OSError, subprocess.SubprocessError):
        return []
    return [int(part) for part in result.stdout.split() if part.isdigit()]


def recent(limit: int = DEFAULT_LIMIT) -> list[dict]:
    """The most recently active threads, with what they are and how they ended."""
    if not available():
        raise SessionError(f"No Codex thread history at {HISTORY_DB}.")
    query = """
        SELECT thread_id,
               MAX(started_at) AS last_at,
               COUNT(*) AS turns,
               SUM(CASE WHEN lower(status) IN ('failed','error','aborted') THEN 1 ELSE 0 END)
                   AS failed,
               SUM(CASE WHEN lower(status) IN ('inprogress','in_progress','running','started')
                        THEN 1 ELSE 0 END) AS active
          FROM thread_turns
         GROUP BY thread_id ORDER BY last_at DESC LIMIT ?
    """
    try:
        connection = sqlite3.connect(f"file:{HISTORY_DB}?mode=ro", uri=True, timeout=15)
        connection.row_factory = sqlite3.Row
        with connection:
            rows = connection.execute(query, (limit,)).fetchall()
            found = []
            for row in rows:
                topic = ""
                for item in connection.execute(
                        """SELECT item_json FROM thread_items WHERE thread_id=?
                            ORDER BY rollout_ordinal LIMIT 8""", (row["thread_id"],)):
                    text = _first_text(item["item_json"])
                    if text and not text.startswith(("<", "You are", "#")):
                        topic = text[:100]
                        break
                last_status = connection.execute(
                    """SELECT status FROM thread_turns WHERE thread_id=?
                        ORDER BY started_at DESC LIMIT 1""", (row["thread_id"],)).fetchone()
                found.append({
                    "thread_id": row["thread_id"],
                    "short": row["thread_id"][:8],
                    "last_at": row["last_at"],
                    "turns": row["turns"],
                    "failed": row["failed"] or 0,
                    "active": row["active"] or 0,
                    "last_status": (last_status["status"] if last_status else "") or "",
                    "topic": topic or "(no prompt recorded)",
                })
    except sqlite3.Error as exc:
        raise SessionError(f"could not read Codex history: {exc}") from exc
    finally:
        try:
            connection.close()
        except (NameError, sqlite3.Error):
            pass

    for entry in found:
        entry["holders"] = lock_holders(entry["thread_id"])
        entry["live"] = bool(entry["holders"])
        entry["state"] = _state_of(entry)
    return found


def _state_of(entry: dict) -> str:
    """What this session needs, in one word."""
    status = (entry.get("last_status") or "").lower()
    if status in RUNNING and entry["live"]:
        return "working"
    if status in FAILED:
        # A failed last turn with the lock still held is the stuck case: the
        # holder keeps failing and nothing else can take the conversation.
        return "stuck" if entry["live"] else "failed"
    return "idle" if entry["live"] else "closed"


def resolve(prefix: str) -> dict:
    """Find one session by id prefix, refusing an ambiguous one."""
    matches = [entry for entry in recent(limit=200)
               if entry["thread_id"].startswith(prefix)]
    if not matches:
        raise SessionError(f"No recent Codex session starting {prefix!r}")
    if len(matches) > 1:
        names = ", ".join(m["short"] for m in matches[:5])
        raise SessionError(f"{prefix!r} matches several sessions: {names}")
    return matches[0]


# --- actions --------------------------------------------------------------

def nudge(thread_id: str, message: str, run=None) -> dict:
    """Hand a message to a session that is already running.

    Uses Codex's own queue, so nothing is typed into a terminal and no window has
    to be opened. It only reaches a live session.
    """
    run = run or subprocess.run
    try:
        result = run(["codex", "queue", "--thread", thread_id, "--message", message],
                     capture_output=True, text=True, timeout=TIMEOUT_S, check=False)
    except (OSError, subprocess.SubprocessError) as exc:
        raise SessionError(f"could not queue a message: {exc}") from exc
    if result.returncode != 0:
        raise SessionError((result.stderr or result.stdout or "codex queue failed").strip()[:200])
    return {"thread_id": thread_id, "queued": True, "message": message}


def release(thread_id: str, signal_process=None) -> list[int]:
    """Close whatever holds this thread's lock, so it can be resumed.

    Terminating rather than killing: Codex releases the lock and flushes its
    history on the way out. The holder is a session that can no longer make
    progress, which is why this is only reached for a stuck one.
    """
    signal_process = signal_process or os.kill
    closed = []
    for pid in lock_holders(thread_id):
        parent = _parent_of(pid)
        # The lock is held by the inner binary; terminating its launcher takes
        # the whole session down cleanly rather than orphaning a child.
        target = parent if parent > 1 and _looks_like_codex(parent) else pid
        try:
            signal_process(target, 15)
            closed.append(target)
        except (OSError, ProcessLookupError, PermissionError) as exc:
            raise SessionError(f"could not close process {target}: {exc}") from exc
    return closed


def _parent_of(pid: int) -> int:
    try:
        for line in Path(f"/proc/{pid}/status").read_text().splitlines():
            if line.startswith("PPid:"):
                return int(line.split()[1])
    except (OSError, ValueError, IndexError):
        pass
    return 0


def _looks_like_codex(pid: int) -> bool:
    try:
        return "codex" in Path(f"/proc/{pid}/cmdline").read_bytes().decode(
            errors="replace").lower()
    except OSError:
        return False
