"""Write docs/demo-fixture.json: synthetic accounts and sessions for screenshots.

Reset times are generated relative to now so countdowns look natural. Nothing
here is read from a real account.
"""
import json, time, pathlib

now = time.time()
H, D = 3600, 86400

def codex(alias, email, plan, used, reset, credits, workspace="", active=False, error=""):
    a = {"provider": "codex", "alias": alias, "email": email, "plan": plan, "workspace": workspace,
         "active": active, "saved": True, "can_switch": True, "can_launch": True,
         "checked_at": now - 40, "reset_credits": credits, "windows": []}
    if error:
        a.update(error=error, checked_at=0, reset_credits=None, reset_credits_error=error)
    else:
        a["windows"] = [{"label": "Codex · weekly", "used": used, "reset": now + reset}]
    return a

def claude(alias, email, plan, five, weekly, fable, active=False, cached=False, error="", workspace=""):
    a = {"provider": "claude", "alias": alias, "email": email, "plan": plan, "workspace": workspace,
         "active": active, "saved": True, "can_switch": True, "can_launch": True,
         "checked_at": now - (11 * 60 if cached else 25), "cached": cached, "windows": []}
    if error:
        a.update(error=error, checked_at=0)
        return a
    a["windows"] = [
        {"label": "All models · 5-hour", "used": five, "reset": now + 2 * H + 13 * 60},
        {"label": "All models · weekly", "used": weekly, "reset": now + 2 * D + 5 * H},
        {"label": "Fable · weekly", "used": fable, "reset": now + 2 * D + 5 * H},
    ]
    a["limited"] = five >= 1
    return a

accounts = [
    codex("work", "dev@example.com", "Business", 0.27, 3 * D + 4 * H, 2, workspace="Engineering", active=True),
    codex("research", "dev@example.com", "Business", 0.61, 3 * D + 4 * H, 1, workspace="Data Platform"),
    codex("personal", "me@example.com", "Pro", 1.0, 1 * D + 2 * H, 0),
    codex("side-project", "side@example.com", "Plus", 0.08, 5 * D + 19 * H, 0),
    codex("legacy", "old@example.com", "Pro", 0, 0, 0, error="401 token_revoked: The refresh token was revoked. Sign in again."),
    claude("studio", "studio@example.com", "Max 20×", 0.18, 0.62, 0.84, active=True),
    claude("team", "dev@example.com", "Team 5×", 0.41, 0.23, 0.55, workspace="Example Org"),
    claude("personal", "me@example.com", "Max 5×", 1.0, 0.47, 1.0),
    claude("lab", "lab@example.com", "Team 5×", 0.05, 0.71, 0.12, cached=True, workspace="Example Org"),
    claude("archive", "archive@example.com", "Max 5×", 0, 0, 0, error="HTTP 429: usage lookup throttled"),
]

work = {"items": [
    {"id": "019a4c1e-7d3b-4f2a-9c1d-3e8f6a2b5c70", "provider": "codex", "title": "Finish the Postgres migration and rerun the integration suite",
     "state": "failed", "cwd": "/home/dev/src/shop-api", "updated": now - 35 * 60, "can_resume": True, "model": "gpt-5-codex",
     "last_user": "Finish the migration and run the tests.", "last_assistant": "Stopped after reaching the weekly usage limit.",
     "reason": "usage_limit"},
    {"id": "019a4b90-11aa-4e6e-8c02-6f1d2a9b3c44", "provider": "codex", "title": "Add retry with backoff to the webhook dispatcher",
     "state": "stuck", "cwd": "/home/dev/src/notify", "updated": now - 2 * H, "can_reboot": True, "live": True, "model": "gpt-5-codex",
     "last_user": "Add exponential backoff and a dead-letter queue.", "last_assistant": "Waiting for quota."},
    {"id": "019a4a02-9e51-4b8d-b7f3-0c2d4e6f8a10", "provider": "codex", "title": "Write release notes for v2.4",
     "state": "failed", "cwd": "/home/dev/src/docs", "updated": now - 6 * H, "can_resume": True, "model": "gpt-5",
     "last_user": "Draft the release notes from the changelog.", "last_assistant": "Stopped after reaching the usage limit."},
    {"id": "bd98a474-3c1e-4b6a-9f20-7e5d1c8a2b93", "provider": "claude", "title": "Review the release changes",
     "state": "stuck", "cwd": "/home/dev/src/shop-api", "updated": now - 50 * 60, "can_reboot": True, "live": True, "model": "claude-opus-5",
     "last_user": "Review changes for the release.", "last_assistant": "Waiting for quota."},
    {"id": "7f3a91c2-0d4e-4a8b-b6c1-2e9f5d7a8b41", "provider": "claude", "title": "Refactor the auth middleware into a package",
     "state": "failed", "cwd": "/home/dev/src/gateway", "updated": now - 4 * H, "can_resume": True, "model": "claude-sonnet-5",
     "last_user": "Split the middleware into its own package.", "last_assistant": "Stopped after reaching the 5-hour limit."},
]}

out = pathlib.Path(__file__).with_name("demo-fixture.json")
out.write_text(json.dumps({"generated_at": now, "sessions": 3, "accounts": accounts, "work": work}, indent=1, ensure_ascii=False))
print(out)
