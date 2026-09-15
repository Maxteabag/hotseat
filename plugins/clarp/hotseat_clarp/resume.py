"""Find work that a usage limit stopped, and continue it.

Hitting a limit does not fail loudly in a way you can act on later. A Clarp agent
records the interruption and goes quiet; a terminal session prints one line and
waits. Hours later, when quota is back, there is no list of what stopped.

This builds that list from evidence, and continues each item only when the account
it needs actually has capacity. Resuming into an exhausted account just reproduces
the failure, so quota is checked first rather than after.

Two sources, detected differently:

  * Clarp agents: its state database logs `reason=usage_limit` against the agent.
    An agent counts as still stopped only if it has completed no turn since.
  * Native sessions: the transcript's last assistant entry is an API error whose
    text is the session-limit message.
"""

from __future__ import annotations

import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess

from . import clarp
from .paths import CLARP_STATE
from hotseat.paths import PROJECTS_DIR
from hotseat.textutil import entry_text, trim

#: The text Claude Code writes into a transcript when a session limit is hit.
LIMIT_TEXT = re.compile(r"hit your (session|usage) limit", re.I)
#: How far back a stop is still worth continuing.
DEFAULT_WINDOW_DAYS = 7
RESUME_PROMPT = (
    "Continue the work that was interrupted by the usage limit. The account has "
    "quota again. Re-check the results of the interrupted step before retrying it, "
    "and do not repeat work that already completed."
)


#: The last thing an agent said, for the listing.
_LAST_SAID = """(SELECT m.text FROM messages m
                  WHERE m.agent_id = a.agent_id AND m.role = 'assistant'
                    AND length(trim(coalesce(m.text,''))) > 0
                  ORDER BY m.seq DESC LIMIT 1) AS said"""


class ResumeError(RuntimeError):
    """Stopped work could not be listed, or could not be continued."""


# --- detection ------------------------------------------------------------

def _paused_clarp_agents() -> list[dict]:
    """Clarp agents whose turn queue is paused.

    Stopping a turn leaves `paused=1` behind. While it is set, a prompt does not
    run: the dispatcher queues it and returns success, so the agent looks
    contactable while actually going nowhere. Any work already queued sits behind
    that flag indefinitely.

    This is a different stuck state from a usage limit and is reported separately,
    because prompting is the fix for one and a no-op for the other.
    """
    if not CLARP_STATE.exists():
        return []
    query = """
        SELECT a.session, a.persona, a.backend, a.cwd,
               (SELECT m.text FROM messages m
                 WHERE m.agent_id = a.agent_id AND m.role = 'assistant'
                   AND length(trim(coalesce(m.text,''))) > 0
                 ORDER BY m.seq DESC LIMIT 1) AS said,
               (SELECT COUNT(*) FROM queued_turns q
                 WHERE q.agent_id = a.agent_id AND q.status = 'queued') AS pending,
               (SELECT MIN(q.enqueued_at) FROM queued_turns q
                 WHERE q.agent_id = a.agent_id AND q.status = 'queued') AS oldest
          FROM queue_state_revisions r
          JOIN agents a USING (agent_id)
         WHERE r.paused = 1 AND a.deleted_at IS NULL AND a.archived_at IS NULL
    """
    try:
        connection = sqlite3.connect(f"file:{CLARP_STATE}?mode=ro", uri=True, timeout=10)
        connection.row_factory = sqlite3.Row
        with connection:
            rows = connection.execute(query).fetchall()
    except sqlite3.Error as exc:
        raise ResumeError(f"could not read Clarp queue state: {exc}") from exc
    finally:
        try:
            connection.close()
        except (NameError, sqlite3.Error):
            pass

    return [{
        "kind": "clarp",
        "id": row["session"],
        "name": row["persona"] or row["session"],
        "backend": row["backend"],
        "cwd": row["cwd"],
        "cause": "queue_paused",
        "pending": row["pending"] or 0,
        "last_message": trim(row["said"], 150),
        "stopped_at": (row["oldest"] or 0) / 1000 or None,
    } for row in rows]


def _parked_clarp_agents(window_days: int) -> list[dict]:
    """Clarp agents parked waiting for an account with quota.

    When a Claude turn hits a limit, Clarp parks it and retries an account check
    every minute. The check demands that ONE account serve every parked agent's
    model at once, so a single agent wanting a model nobody has quota for keeps
    every other parked agent waiting, including ones whose model is available.

    Detecting this matters because nothing else names it: the runtime only logs
    "No verified account", and the agent that cannot be served is not necessarily
    the agent you are waiting on.
    """
    if not CLARP_STATE.exists():
        return []
    import time as _time
    cutoff = int((_time.time() - window_days * 86400) * 1000) if window_days else 0
    query = f"""
        WITH parked AS (
            SELECT agent_id, MAX(ts) AS ts FROM state_log
             WHERE json_extract(detail, '$.account_recovery') = 'waiting'
               AND ts >= ?
             GROUP BY agent_id),
        finished AS (
            SELECT agent_id, MAX(ts) AS ok_ts FROM state_log
             WHERE kind = 'done' GROUP BY agent_id)
        SELECT a.session, a.persona, a.backend, a.cwd, a.model, p.ts,
               {_LAST_SAID}
          FROM parked p
          JOIN agents a USING (agent_id)
          LEFT JOIN finished f USING (agent_id)
         WHERE a.deleted_at IS NULL AND a.archived_at IS NULL
           AND (f.ok_ts IS NULL OR f.ok_ts < p.ts)
    """
    try:
        connection = sqlite3.connect(f"file:{CLARP_STATE}?mode=ro", uri=True, timeout=10)
        connection.row_factory = sqlite3.Row
        with connection:
            rows = connection.execute(query, (cutoff,)).fetchall()
    except sqlite3.Error as exc:
        raise ResumeError(f"could not read Clarp state: {exc}") from exc
    finally:
        try:
            connection.close()
        except (NameError, sqlite3.Error):
            pass

    return [{
        "kind": "clarp",
        "id": row["session"],
        "name": row["persona"] or row["session"],
        "backend": row["backend"],
        "cwd": row["cwd"],
        "model": row["model"],
        "cause": "waiting_for_account",
        "pending": 0,
        "last_message": trim(row["said"], 150),
        "stopped_at": (row["ts"] or 0) / 1000,
    } for row in rows]


def _stopped_clarp_agents(window_days: int) -> list[dict]:
    """Clarp agents whose last usage-limit stop has not been followed by a turn."""
    if not CLARP_STATE.exists():
        return []
    cutoff_ms = None
    if window_days:
        import time
        cutoff_ms = int((time.time() - window_days * 86400) * 1000)

    query = """
        WITH stopped AS (
            SELECT agent_id, MAX(ts) AS fail_ts
              FROM state_log
             WHERE kind IN ('interrupted', 'error')
               AND json_extract(detail, '$.reason') = 'usage_limit'
             GROUP BY agent_id),
        finished AS (
            SELECT agent_id, MAX(ts) AS ok_ts FROM state_log
             WHERE kind = 'done' GROUP BY agent_id)
        SELECT a.session, a.persona, a.backend, a.cwd, s.fail_ts,
               (SELECT m.text FROM messages m
                 WHERE m.agent_id = a.agent_id AND m.role = 'assistant'
                   AND length(trim(coalesce(m.text,''))) > 0
                 ORDER BY m.seq DESC LIMIT 1) AS said
          FROM stopped s
          JOIN agents a USING (agent_id)
          LEFT JOIN finished f USING (agent_id)
         WHERE a.deleted_at IS NULL AND a.archived_at IS NULL
           AND (f.ok_ts IS NULL OR f.ok_ts < s.fail_ts)
           AND (? IS NULL OR s.fail_ts >= ?)
         ORDER BY s.fail_ts DESC
    """
    try:
        # Read-only: this database belongs to a running service.
        connection = sqlite3.connect(f"file:{CLARP_STATE}?mode=ro", uri=True, timeout=10)
        connection.row_factory = sqlite3.Row
        with connection:
            rows = connection.execute(query, (cutoff_ms, cutoff_ms)).fetchall()
    except sqlite3.Error as exc:
        raise ResumeError(f"could not read Clarp state: {exc}") from exc
    finally:
        try:
            connection.close()
        except (NameError, sqlite3.Error):
            pass

    return [{
        "kind": "clarp",
        "id": row["session"],
        "name": row["persona"] or row["session"],
        "backend": row["backend"],
        "cwd": row["cwd"],
        "cause": "usage_limit",
        "pending": 0,
        "last_message": trim(row["said"], 150),
        "stopped_at": (row["fail_ts"] or 0) / 1000,
    } for row in rows]


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


def stopped(window_days: int = DEFAULT_WINDOW_DAYS,
            include_paused: bool = True) -> list[dict]:
    """Everything that stopped and has not run since, by either cause.

    Two causes, kept apart because their fixes differ: a usage limit, which a
    prompt fixes once quota returns, and a paused queue, which a prompt does not
    fix at all.
    """
    items = _stopped_native_sessions(window_days)
    items += _stopped_clarp_agents(window_days)
    seen_ids = {item["id"] for item in items}
    items += [row for row in _parked_clarp_agents(window_days)
              if row["id"] not in seen_ids]
    if include_paused:
        paused = _paused_clarp_agents()
        paused_ids = {row["id"] for row in paused}
        # A pause blocks dispatch regardless of the earlier interruption reason.
        items = [item for item in items
                 if item["kind"] != "clarp" or item["id"] not in paused_ids]
        items += paused
    return sorted(items, key=_priority)


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

    Clarp's account check requires one account to serve every parked agent's model
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

    if item["backend"] != clarp.CLAUDE_BACKEND:
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


def continue_item(item: dict, prompt: str = RESUME_PROMPT, run=None) -> dict:
    """Continue one stopped item. Returns what was done, or raises."""
    run = run or subprocess.run
    if item.get("cause") == "queue_paused":
        raise ResumeError(
            f"{item['name']}'s queue is paused. A prompt would be queued behind the "
            f"pause rather than run. Clear it from the Clarp app: start or remove "
            f"the waiting turns for that agent.")
    if item["kind"] == "clarp":
        try:
            # origin=automation, because this is not the user typing. Clarp
            # records it as such rather than as a message they sent.
            result = run([clarp.CLARP_ADMIN, "prompt", "--to", item["id"],
                          "--text", prompt, "--origin", "automation"],
                         capture_output=True, text=True, timeout=60, check=False)
        except (OSError, subprocess.SubprocessError) as exc:
            raise ResumeError(f"could not prompt {item['name']}: {exc}") from exc
        if result.returncode != 0:
            raise ResumeError((result.stderr or "clarp-admin prompt failed").strip()[:200])
        return {"id": item["id"], "kind": "clarp", "continued": True}

    # A native session has no supervisor to accept a prompt, so the resume command
    # is handed back rather than run behind the user's back.
    return {
        "id": item["id"],
        "kind": "native",
        "continued": False,
        "command": f"claude --resume {item['id']}",
        "cwd": item.get("cwd"),
    }
