"""Find native sessions stopped by usage limits and describe how to resume them."""

from __future__ import annotations

import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess

from .paths import PROJECTS_DIR
from .textutil import entry_text, trim

#: The text Claude Code writes into a transcript when a session limit is hit.
LIMIT_TEXT = re.compile(r"hit your (session|usage) limit", re.I)
#: How far back a stop is still worth continuing.
DEFAULT_WINDOW_DAYS = 7
RESUME_PROMPT = (
    "Continue the work that was interrupted by the usage limit. The account has "
    "quota again. Re-check the results of the interrupted step before retrying it, "
    "and do not repeat work that already completed."
)


class ResumeError(RuntimeError):
    """Stopped work could not be listed, or could not be continued."""


# --- detection ------------------------------------------------------------







def _last_entries(path: Path, keep: int = 40) -> list[dict]:
    """The tail of a transcript, parsed. Transcripts are large, so read once."""
    entries: list[dict] = []
    try:
        with path.open(errors="replace") as handle:
            for line in handle:
                try:
                    entries.append(json.loads(line))
                except ValueError:
                    continue
                if len(entries) > keep * 8:
                    del entries[:keep * 4]
    except OSError:
        return []
    return entries[-keep:]


def _stopped_native_sessions(window_days: int) -> list[dict]:
    """Native sessions whose transcript ends on a usage-limit error."""
    if not PROJECTS_DIR.is_dir():
        return []
    import time
    cutoff = time.time() - window_days * 86400 if window_days else 0

    found = []
    for transcript in PROJECTS_DIR.glob("*/*.jsonl"):
        try:
            modified = transcript.stat().st_mtime
        except OSError:
            continue
        if modified < cutoff:
            continue

        entries = _last_entries(transcript)
        if not entries:
            continue
        # Taken from the tail already in hand: re-reading a transcript that can run
        # to tens of megabytes just for one line would not be worth it.
        said = next((entry_text(e) for e in reversed(entries)
                     if e.get("type") == "assistant" and not e.get("isApiErrorMessage")
                     and entry_text(e).strip()), "")
        # Only the final assistant turn counts. An earlier limit that was worked
        # through is history, not something waiting to be continued.
        last = next((e for e in reversed(entries)
                     if e.get("type") in ("assistant", "user")), None)
        if not last or not last.get("isApiErrorMessage"):
            continue
        content = last.get("message", {}).get("content")
        text = content if isinstance(content, str) else json.dumps(content)
        if not LIMIT_TEXT.search(text or ""):
            continue

        found.append({
            "kind": "native",
            "cause": "usage_limit",
            "pending": 0,
            "id": transcript.stem,
            "name": transcript.stem[:8],
            "backend": "claude",
            "cwd": (last.get("cwd") or _decode_project_dir(transcript.parent.name)),
            "last_message": trim(said, 150),
            "stopped_at": modified,
        })
    return sorted(found, key=lambda item: -item["stopped_at"])


def _decode_project_dir(name: str) -> str:
    """Project directories encode a path with dashes; recover a usable guess."""
    return "/" + name.lstrip("-").replace("-", "/")




def _priority(item: dict) -> tuple:
    """Order by how much attention an item deserves, not merely by recency.

    Work already queued and unable to run outranks everything: it is blocked and
    someone is waiting on it. A paused queue holding nothing is last, because it is
    leftover state rather than stalled work. Recency only breaks ties.
    """
    has_pending = bool(item.get("pending"))
    idle_pause = item.get("cause") == "queue_paused" and not has_pending
    return (not has_pending, idle_pause, -(item.get("stopped_at") or 0))


# --- continuing -----------------------------------------------------------

def _family(model: str | None) -> str:
    """The model family an id belongs to: claude-fable-5-1 -> fable."""
    lowered = (model or "").lower()
    for known in ("fable", "opus", "sonnet", "haiku"):
        if known in lowered:
            return known
    return ""


def servable_by(model: str | None, accounts: list[dict]) -> list[str]:
    """Which accounts could serve this model right now.

    An account qualifies when it is not rate limited and has not spent the weekly
    limit for that model's family. Per-model limits are the point: an account can
    be healthy overall and still unable to serve one family.
    """
    family = _family(model)
    able = []
    for account in accounts:
        usage = account.get("usage")
        if not usage or usage.get("limited"):
            continue
        blocked = {(_family(m) or m.lower()) for m in usage.get("blocked_models") or []}
        if family and family in blocked:
            continue
        if account.get("alias"):
            able.append(account["alias"])
    return able


def starving_models(items: list[dict], accounts: list[dict]) -> dict[str, list[str]]:
    """Models no account can serve, mapped to the agents waiting on them.

    Supervisor's account check requires one account to serve every parked agent's model
    at once, so a single unservable model keeps the whole parked set waiting,
    including agents whose own model is available. Naming it is the difference
    between "nothing runs" and "this is why".
    """
    starving: dict[str, list[str]] = {}
    for item in items:
        if item.get("cause") != "waiting_for_account":
            continue
        if servable_by(item.get("model"), accounts):
            continue
        starving.setdefault(item.get("model") or "unknown", []).append(item["name"])
    return starving


def readiness(item: dict, accounts: list[dict], default_alias: str | None) -> dict:
    """Whether this item can be continued, and what stands in the way.

    Two different things get confused here, so they are kept apart:

      * a **hard** block means continuing now would reproduce the original
        failure, so it is refused;
      * a **warning** means it may fail for a reason that depends on which model
        the work uses, which cannot be known in advance.

    An exhausted account is hard. A spent limit for one model family is a warning,
    because the resumed work may not use that family at all.
    """
    if item.get("cause") == "waiting_for_account":
        able = servable_by(item.get("model"), accounts)
        if able:
            return {"ready": True, "hard": None,
                    "warning": f"parked; {', '.join(able[:2])} could serve it"}
        return {"ready": False,
                "hard": f"no account can serve {item.get('model') or 'its model'}",
                "warning": None}

    if item.get("cause") == "queue_paused":
        # A prompt to a paused queue is accepted and then queued, so it reports
        # success while changing nothing. Refusing is the honest answer.
        pending = item.get("pending") or 0
        detail = f", {pending} turn(s) already waiting" if pending else ""
        return {"ready": False,
                "hard": f"queue is paused{detail}; a prompt would only queue",
                "warning": None}

    if item["backend"] != "claude":
        # Codex limits are OpenAI's. Nothing here can see that quota, and
        # pretending otherwise would be worse than saying so.
        return {"ready": True, "hard": None,
                "warning": "OpenAI quota is not visible from here"}

    alias = item.get("account") or default_alias
    account = next((a for a in accounts if a.get("alias") == alias), None)
    if account is None:
        return {"ready": False, "hard": f"account {alias or 'unknown'} is not saved here",
                "warning": None}
    usage = account.get("usage")
    if not usage:
        return {"ready": False, "hard": f"{alias} quota is unknown", "warning": None}
    if usage.get("limited"):
        return {"ready": False, "hard": f"{alias} is still rate limited", "warning": None}

    blocked = usage.get("blocked_models") or []
    return {"ready": True, "hard": None,
            "warning": f"{alias} has no {blocked[0]} quota left" if blocked else None}




def stopped(window_days=DEFAULT_WINDOW_DAYS, include_paused=True):
    return sorted(_stopped_native_sessions(window_days),key=_priority)


def continue_item(item,prompt=RESUME_PROMPT,run=None):
    if item.get("kind")!="native":raise ResumeError("This source requires its plugin")
    return {"id":item["id"],"kind":"native","continued":False,"command":f"claude --resume {item['id']}","cwd":item.get("cwd")}
