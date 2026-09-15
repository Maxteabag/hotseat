"""Read an account's quota straight from the API's rate-limit headers.

A minimal request is the only way to learn an account's standing: the numbers
arrive as response headers rather than from any status endpoint. The request is
deliberately as small as possible and its answer is discarded.
"""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request

API_URL = "https://api.anthropic.com/v1/messages"
ANTHROPIC_VERSION = "2023-06-01"
OAUTH_BETA = "oauth-2025-04-20"
USER_AGENT = "hotseat/0.1"
# Tried in order; a retired model answers 404 and the next one is used.
PROBE_MODELS = ("claude-haiku-4-5-20251001", "claude-3-5-haiku-20241022")
TIMEOUT_S = 15


class ProbeError(RuntimeError):
    """The API could not be reached, or refused the token outright."""


def probe(token: str) -> dict[str, str]:
    """Return the rate-limit headers for an access token.

    A 429 is a perfectly good answer: it still carries the headers that say when
    the limit resets, which is exactly what the dashboard needs to show.
    """
    last_error = None
    for model in PROBE_MODELS:
        body = json.dumps({
            "model": model,
            "max_tokens": 1,
            "messages": [{"role": "user", "content": "hi"}],
        }).encode()
        request = urllib.request.Request(API_URL, data=body, headers={
            "Authorization": f"Bearer {token}",
            "anthropic-version": ANTHROPIC_VERSION,
            "anthropic-beta": OAUTH_BETA,
            "Content-Type": "application/json",
            "User-Agent": USER_AGENT,
        })
        try:
            with urllib.request.urlopen(request, timeout=TIMEOUT_S) as response:
                return {k.lower(): v for k, v in response.headers.items()}
        except urllib.error.HTTPError as exc:
            headers = {k.lower(): v for k, v in exc.headers.items()}
            if exc.code == 404:
                last_error = f"model {model} unavailable"
                continue
            if exc.code in (401, 403):
                raise ProbeError("the account rejected this token") from exc
            return headers
        except (urllib.error.URLError, OSError, TimeoutError) as exc:
            last_error = str(exc)
    raise ProbeError(last_error or "no response from the API")


def _fraction(headers: dict[str, str], name: str) -> float | None:
    """Utilisation arrives as a ready-made fraction, for example 0.36 for 36%."""
    raw = headers.get(name)
    if raw is None:
        return None
    try:
        return max(0.0, float(raw))
    except (TypeError, ValueError):
        return None


def _epoch(headers: dict[str, str], *names: str) -> int | None:
    for name in names:
        raw = headers.get(name)
        if raw and raw.isdigit():
            return int(raw)
    return None


def summarise(headers: dict[str, str]) -> dict:
    """Reduce the raw headers to the few numbers the dashboard renders.

    An account can be rejected on its 5-hour window yet still serve requests from
    purchased extra usage, so availability follows the status fields rather than
    being inferred from utilisation.
    """
    unified = headers.get("anthropic-ratelimit-unified-status", "unknown")
    status_5h = headers.get("anthropic-ratelimit-unified-5h-status", "unknown")
    status_7d = headers.get("anthropic-ratelimit-unified-7d-status", "unknown")
    available = unified == "allowed" or status_5h == "allowed"
    return {
        "status": unified,
        "status_5h": status_5h,
        "status_7d": status_7d,
        "available": available,
        "limited": not available,
        "used_5h": _fraction(headers, "anthropic-ratelimit-unified-5h-utilization"),
        "reset_5h": _epoch(headers, "anthropic-ratelimit-unified-5h-reset",
                           "anthropic-ratelimit-unified-reset"),
        "used_7d": _fraction(headers, "anthropic-ratelimit-unified-7d-utilization"),
        "reset_7d": _epoch(headers, "anthropic-ratelimit-unified-7d-reset"),
        "overage_status": headers.get("anthropic-ratelimit-unified-overage-status"),
        "overage_reason": headers.get("anthropic-ratelimit-unified-overage-disabled-reason"),
        # Which window is currently the binding constraint, straight from the API.
        "binding": headers.get("anthropic-ratelimit-unified-representative-claim"),
        "fallback": headers.get("anthropic-ratelimit-unified-fallback"),
        "fallback_at": _fraction(headers, "anthropic-ratelimit-unified-fallback-percentage"),
    }
