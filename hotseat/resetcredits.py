"""Read Codex usage-reset balances. This module never redeems or buys resets."""
from concurrent.futures import ThreadPoolExecutor
import json
from pathlib import Path
import time
import urllib.error
import urllib.request
from . import codex

ENDPOINT = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"


def read_balance(path: Path) -> dict:
    result = {"available_count": None, "checked_at": time.time(), "error": None}
    try:
        tokens = json.loads(path.read_text()).get("tokens") or {}
        if not tokens.get("access_token") or not tokens.get("account_id"):
            raise ValueError("No complete OAuth credentials")
        request = urllib.request.Request(ENDPOINT, headers={
            "Authorization": "Bearer " + tokens["access_token"],
            "ChatGPT-Account-ID": tokens["account_id"], "Accept": "application/json",
            "User-Agent": "Codex Desktop", "originator": "Codex Desktop", "OAI-Product-Sku": "CODEX",
        }, method="GET")
        with urllib.request.urlopen(request, timeout=12) as response:
            payload = json.load(response)
        if not isinstance(payload, dict):
            raise ValueError("Invalid reset balance response")
        count = payload.get("available_count")
        if type(count) is not int or count < 0:
            raise ValueError("Reset balance response has no valid available_count")
        result["available_count"] = count
    except urllib.error.HTTPError as exc:
        result["error"] = f"HTTP {exc.code}"
    except (OSError, ValueError, TypeError, AttributeError, urllib.error.URLError) as exc:
        result["error"] = str(exc)
    return result


def balances(aliases=None) -> list[dict]:
    accounts = codex.accounts()
    if aliases:
        missing = set(aliases) - {a["alias"] for a in accounts}
        if missing:
            raise ValueError("Unknown Codex profiles: " + ", ".join(sorted(missing)))
        accounts = [a for a in accounts if a["alias"] in aliases]

    def one(account):
        path = codex.PROFILES_DIR / account["alias"] / "auth.json" if account["saved"] else codex.LIVE_AUTH
        return {"alias": account["alias"], "email": account.get("email"), **read_balance(path)}
    with ThreadPoolExecutor(max_workers=3) as pool:
        return list(pool.map(one, accounts))
