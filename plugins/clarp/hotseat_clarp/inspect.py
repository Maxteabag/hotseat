"""What a stopped piece of work was actually doing.

A list of stopped agents is only half useful: deciding whether to continue one
means knowing what it was in the middle of. This recovers that from whatever the
two sources kept.

Everything here is read-only and local. Content is truncated on the way out rather
than in the caller, so a long transcript cannot flood a terminal, and speech markup
is stripped because it is delivery scaffolding, not what the agent said.
"""

from __future__ import annotations

import json
from pathlib import Path
import re
import sqlite3

from .paths import CLARP_STATE
from hotseat.paths import PROJECTS_DIR
from hotseat.textutil import clean as _clean, entry_text, trim

#: A continuation prompt. It is not something the user typed, but its summary is
#: often the best description of what the work was, so it is mined rather than
#: discarded.
CONTEXT_PREAMBLE = "This session is being continued from a previous conversation"
SUMMARY_MARKER = re.compile(r"(?:Summary:|Primary Request and Intent:?)\s*(.+)", re.S)


class InspectError(RuntimeError):
    """The stopped work could not be inspected."""


def _summary_of(text: str | None) -> str:
    """Pull the work description out of a context-continuation preamble."""
    if not text or not text.startswith(CONTEXT_PREAMBLE):
        return ""
    match = SUMMARY_MARKER.search(text)
    return trim(match.group(1) if match else text[len(CONTEXT_PREAMBLE):], 500)


# --- clarp ----------------------------------------------------------------

def _clarp_detail(session: str) -> dict | None:
    if not CLARP_STATE.exists():
        return None
    try:
        connection = sqlite3.connect(f"file:{CLARP_STATE}?mode=ro", uri=True, timeout=10)
        connection.row_factory = sqlite3.Row
        with connection:
            agent = connection.execute(
                """SELECT agent_id, persona, backend, cwd, model FROM agents
                    WHERE session = ? AND deleted_at IS NULL""", (session,)).fetchone()
            if agent is None:
                return None
            agent_id = agent["agent_id"]
            messages = connection.execute(
                """SELECT role, text, timestamp FROM messages
                    WHERE agent_id = ? AND role IN ('user','assistant')
                      AND length(trim(coalesce(text,''))) > 0
                    ORDER BY seq DESC LIMIT 200""", (agent_id,)).fetchall()
            queued = connection.execute(
                """SELECT text, origin, enqueued_at FROM queued_turns
                    WHERE agent_id = ? AND status = 'queued'
                    ORDER BY queue_seq""", (agent_id,)).fetchall()
    except sqlite3.Error as exc:
        raise InspectError(f"could not read Clarp state: {exc}") from exc
    finally:
        try:
            connection.close()
        except (NameError, sqlite3.Error):
            pass

    ordered = list(reversed(messages))
    user_texts = [m["text"] for m in reversed(ordered) if m["role"] == "user"]
    last_user = next((t for t in user_texts if not (t or "").startswith(CONTEXT_PREAMBLE)), None)
    summary = next((_summary_of(t) for t in user_texts if _summary_of(t)), "")
    last_assistant = next((m["text"] for m in reversed(ordered)
                           if m["role"] == "assistant"), None)

    return {
        "kind": "clarp",
        "id": session,
        "name": agent["persona"] or session,
        "backend": agent["backend"],
        "model": agent["model"],
        "cwd": agent["cwd"],
        "last_user": trim(last_user),
        "summary": summary,
        "last_assistant": trim(last_assistant),
        "last_activity": (ordered[-1]["timestamp"] if ordered else None),
        "queued": [{"origin": row["origin"], "text": trim(row["text"], 300)}
                   for row in queued],
        "recent": [{"role": m["role"], "text": trim(m["text"], 160)}
                   for m in ordered[-6:]],
    }


# --- native ---------------------------------------------------------------

def _native_detail(session_id: str) -> dict | None:
    matches = list(PROJECTS_DIR.glob(f"*/{session_id}.jsonl"))
    if not matches:
        # Accept the short form shown in listings.
        matches = list(PROJECTS_DIR.glob(f"*/{session_id}*.jsonl"))
    if not matches:
        return None
    transcript = matches[0]

    entries = []
    try:
        with transcript.open(errors="replace") as handle:
            for line in handle:
                try:
                    entries.append(json.loads(line))
                except ValueError:
                    continue
    except OSError as exc:
        raise InspectError(f"could not read the transcript: {exc}") from exc
    if not entries:
        return None

    def usable(entry, role):
        if entry.get("type") != role or entry.get("isApiErrorMessage"):
            return False
        text = entry_text(entry).strip()
        # Tool results and system scaffolding arrive as user entries too.
        return bool(text) and not text.startswith(("<", CONTEXT_PREAMBLE))

    users = [e for e in entries if usable(e, "user")]
    summary = next((_summary_of(entry_text(e)) for e in reversed(entries)
                    if _summary_of(entry_text(e))), "")
    assistants = [e for e in entries if usable(e, "assistant")]
    last = entries[-1]

    return {
        "kind": "native",
        "id": transcript.stem,
        "name": transcript.stem[:8],
        "backend": "claude",
        "model": next((e.get("message", {}).get("model") for e in reversed(entries)
                       if (e.get("message") or {}).get("model")), None),
        "cwd": last.get("cwd") or next((e.get("cwd") for e in reversed(entries)
                                        if e.get("cwd")), None),
        "branch": next((e.get("gitBranch") for e in reversed(entries)
                        if e.get("gitBranch")), None),
        "last_user": trim(entry_text(users[-1])) if users else "",
        "summary": summary,
        "last_assistant": trim(entry_text(assistants[-1])) if assistants else "",
        "last_activity": last.get("timestamp"),
        "queued": [],
        "recent": [{"role": e.get("type"), "text": trim(entry_text(e), 160)}
                   for e in entries if usable(e, "user") or usable(e, "assistant")][-6:],
        "turns": len(users),
    }


def detail(identifier: str) -> dict:
    """Everything known about one stopped item, by session slug or transcript id."""
    found = _clarp_detail(identifier)
    if found is None:
        found = _native_detail(identifier)
    if found is None:
        raise InspectError(f"nothing found for {identifier!r}")
    return found
