"""Account quota, read from Anthropic's OAuth usage endpoint.

This is the same endpoint the Claude apps use for their own limit displays. It is
strictly better than inferring quota from the rate-limit headers of a real request:

  * it costs no inference and consumes no quota;
  * it still answers when the account is rate limited, where a probe just fails;
  * it reports per-model weekly limits, which the headers never expose.

That last point matters. An account can sit at 53% of its overall weekly allowance
while its weekly limit for one model family is completely spent, and only this
endpoint can tell you that.
"""

from __future__ import annotations

import datetime
from email.utils import parsedate_to_datetime
import json
import urllib.error
import urllib.request

USAGE_ENDPOINT = "https://api.anthropic.com/api/oauth/usage"
OAUTH_BETA = "oauth-2025-04-20"
TIMEOUT_S = 15
#: Severities the endpoint uses, worst first.
SEVERITY_ORDER = ("critical", "serious", "warning", "normal")


class UsageError(RuntimeError):
    """The usage endpoint could not be reached, or refused the token."""
    def __init__(self, message, retry_after=None):
        super().__init__(message)
        self.retry_after=retry_after


def fetch(token: str) -> dict:
    request = urllib.request.Request(USAGE_ENDPOINT, headers={
        "Authorization": f"Bearer {token}",
        "anthropic-beta": OAUTH_BETA,
        "Accept": "application/json",
    })
    try:
        with urllib.request.urlopen(request, timeout=TIMEOUT_S) as response:
            return json.loads(response.read().decode())
    except urllib.error.HTTPError as exc:
        if exc.code in (401, 403):
            raise UsageError("the account rejected this token") from exc
        retry_after=None
        try:
            retry_after=max(0,float(exc.headers.get("Retry-After", "")))
        except (ValueError,TypeError,AttributeError):
            try:
                at=parsedate_to_datetime(exc.headers.get("Retry-After", ""))
                if at.tzinfo is None:at=at.replace(tzinfo=datetime.timezone.utc)
                retry_after=max(0,(at-datetime.datetime.now(datetime.timezone.utc)).total_seconds())
            except (ValueError,TypeError,AttributeError,OverflowError):
                pass
        raise UsageError(f"usage endpoint returned {exc.code}", retry_after=retry_after) from exc
    except (urllib.error.URLError, OSError, TimeoutError, ValueError) as exc:
        raise UsageError(str(exc)) from exc


def _percent(window) -> float | None:
    """Utilisation as a fraction. The endpoint sends percentages, not fractions."""
    if not isinstance(window, dict):
        return None
    value = window.get("utilization")
    if value is None:
        return None
    try:
        return float(value) / 100.0
    except (TypeError, ValueError):
        return None


def _epoch(window) -> int | None:
    if not isinstance(window, dict):
        return None
    stamp = window.get("resets_at")
    if not stamp:
        return None
    try:
        return int(datetime.datetime.fromisoformat(stamp).timestamp())
    except (TypeError, ValueError):
        return None


def _scoped_limits(payload: dict) -> list[dict]:
    """Per-model weekly limits, which are separate from the overall allowance."""
    found = []
    for limit in payload.get("limits") or []:
        if not isinstance(limit, dict) or limit.get("kind") != "weekly_scoped":
            continue
        model = ((limit.get("scope") or {}).get("model") or {})
        name = model.get("display_name") or model.get("id")
        if not name:
            continue
        try:
            used = float(limit.get("percent")) / 100.0
        except (TypeError, ValueError):
            continue
        found.append({
            "model": name,
            "used": used,
            "severity": limit.get("severity") or "normal",
            "reset": _epoch(limit),
            "exhausted": used >= 1.0,
        })
    return sorted(found, key=lambda item: -item["used"])


def summarise(payload: dict) -> dict:
    """Reduce the payload to what the interfaces render."""
    five = payload.get("five_hour")
    seven = payload.get("seven_day")
    extra = payload.get("extra_usage") or {}
    scoped = _scoped_limits(payload)

    used_5h = _percent(five)
    # A locked window is exhausted even when the number has already rolled over.
    limited = any(
        isinstance(window, dict) and (
            bool(window.get("locked_reason"))
            or (used is not None and used >= 1.0))
        for window, used in ((five, used_5h), (seven, _percent(seven))))
    worst = min((limit.get("severity") for limit in payload.get("limits") or []
                 if isinstance(limit, dict)),
                key=lambda s: SEVERITY_ORDER.index(s) if s in SEVERITY_ORDER else 99,
                default="normal")

    return {
        "status": "rejected" if limited else "allowed",
        "available": not limited,
        "limited": limited,
        "severity": worst,
        "used_5h": used_5h,
        "reset_5h": _epoch(five),
        "used_7d": _percent(seven),
        "reset_7d": _epoch(seven),
        # Per-model weekly limits. An account can be fine overall and still have
        # nothing left for one model.
        "scoped": scoped,
        "blocked_models": [s["model"] for s in scoped if s["exhausted"]],
        "extra_usage_enabled": bool(extra.get("is_enabled")),
        "extra_usage_reason": extra.get("disabled_reason"),
        "plan": payload.get("plan_type"),
    }


def for_token(token: str) -> dict:
    return summarise(fetch(token))
