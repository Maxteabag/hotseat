"""Per-model token usage, read from Claude Code's own local statistics.

This answers a different question from the quota bars and must not be confused
with them:

  * The quota bars come from the API and describe one account's remaining budget.
  * These numbers come from `stats-cache.json`, count tokens rather than quota,
    and cover every account used on this machine.

They are reported side by side because "how much of the week is gone" and "what
was it spent on" are both worth knowing, but they are never added together.
"""

from __future__ import annotations

import datetime
import json
import re
from collections import Counter
from pathlib import Path

from .backends.linux import config_dir

DEFAULT_DAYS = 7
#: Trailing release dates are noise in a label: claude-haiku-4-5-20251001.
DATE_SUFFIX = re.compile(r"-\d{8}$")
FAMILY_ORDER = ("fable", "opus", "sonnet", "haiku")


def stats_file(root: Path | None = None) -> Path:
    return (root or config_dir()) / "stats-cache.json"


def label(model: str) -> str:
    """Turn a model id into something readable: claude-fable-5-1 -> Fable 5.1."""
    name = DATE_SUFFIX.sub("", model)
    name = name.removeprefix("claude-")
    parts = name.split("-")
    if not parts:
        return model
    family = parts[0].capitalize()
    version = ".".join(parts[1:])
    return f"{family} {version}".strip()


def family(model: str) -> str:
    lowered = model.lower()
    for known in FAMILY_ORDER:
        if known in lowered:
            return known
    return "other"


def recent_by_model(days: int = DEFAULT_DAYS, root: Path | None = None,
                    today: datetime.date | None = None) -> dict | None:
    """Aggregate the last `days` of per-model token counts.

    Returns None when the statistics file is absent or unreadable, which is a
    normal state on a fresh install rather than an error.
    """
    path = stats_file(root)
    try:
        data = json.loads(path.read_text())
    except (OSError, ValueError):
        return None

    daily = data.get("dailyModelTokens")
    if not isinstance(daily, list):
        return None

    today = today or datetime.date.today()
    cutoff = (today - datetime.timedelta(days=days)).isoformat()

    totals: Counter[str] = Counter()
    for row in daily:
        if not isinstance(row, dict) or str(row.get("date", "")) < cutoff:
            continue
        by_model = row.get("tokensByModel")
        if not isinstance(by_model, dict):
            continue
        for model, tokens in by_model.items():
            if isinstance(tokens, (int, float)) and tokens > 0:
                totals[model] += int(tokens)

    total = sum(totals.values())
    models = [{
        "model": model,
        "label": label(model),
        "family": family(model),
        "tokens": count,
        "share": (count / total) if total else 0.0,
    } for model, count in totals.most_common()]

    as_of = data.get("lastComputedDate")
    stale = bool(as_of and as_of < (today - datetime.timedelta(days=1)).isoformat())
    return {
        "days": days,
        "as_of": as_of,
        # The cache is recomputed periodically, so today's activity may be missing.
        "stale": stale,
        "total_tokens": total,
        "models": models,
    }
