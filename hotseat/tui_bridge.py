"""JSON boundary for the Go TUI. Only public account fields leave this module."""
from __future__ import annotations
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import subprocess
from . import bundled
import time
from . import actions, codex, usage, resetcredits, quota_cache, codexlaunch
from . import refresh as token_refresh
from .backends import for_platform
from .sessions import running_sessions


def group_accounts(rows):
    """Display each provider/email/workspace once without deleting any profiles.

    Keep a successful profile for actions, but show a short stable display alias.
    Never group accounts across workspaces or merge unknown identities.
    """
    groups = {}
    for row in rows:
        email, workspace = row.get("email"), row.get("workspace")
        key = (row["provider"], email, workspace) if email and email != "Unknown email" and workspace else (row["provider"], row["alias"])
        groups.setdefault(key, []).append(row)
    result = []
    for members in groups.values():
        best = max(members, key=lambda r: (bool(r.get("checked_at")) and not r.get("error"), not bool(r.get("error")), bool(r.get("active"))))
        item = dict(best)
        item["active"] = any(r.get("active") for r in members)
        item["display_alias"] = min((r["alias"] for r in members), key=lambda s: (len(s), s))
        item["aliases"] = [r["alias"] for r in members]
        item["profile_notes"] = [r["alias"] + ": " + ("Sign-in expired" if "token_revoked" in r["error"] else r["error"]) for r in members if r.get("error")]
        if item["provider"] == "codex" and item.get("checked_at") and not any("bengalfox" in w["label"].lower() or "spark" in w["label"].lower() for w in item.get("windows", [])):
            item["profile_notes"].append("OpenAI reports no separate Spark allowance for this account.")
        result.append(item)
    return sorted(result, key=lambda r: (r["provider"], not r.get("active"), bool(r.get("error")), r["display_alias"]))


def snapshot(refresh=False):
    backend = for_platform()
    result = {"accounts": [], "generated_at": time.time(), "errors": [], "sessions": running_sessions()}
    try:
        claude_accounts = backend.accounts()
    except Exception as exc:
        claude_accounts = []
        result["errors"].append(f"Claude discovery: {exc}")

    def claude_row(account):
        row = {"provider": "claude", "alias": account.alias, "email": account.email or "Unknown email",
               "plan": getattr(account,"plan_label",account.plan or "Unknown plan"),
               "rate_limit_tier": getattr(account,"rate_limit_tier",None), "workspace": account.org or "",
               "active": account.is_active, "saved": not account.alias.startswith("("), "windows": [], "error": "",
               "can_switch": backend.can_switch and not account.alias.startswith("("), "can_launch": True, "checked_at": 0}
        if refresh:
            try:
                if not account.token:
                    raise ValueError("No stored token")
                if token_refresh.expired(account):
                    try:
                        token_refresh.auto(backend, account)
                    except Exception as exc:  # RefreshError, or anything unexpected underneath it
                        raise ValueError(f"Access token expired; refresh failed: {exc}") from exc
                limits = quota_cache.read(account.token)
                row["checked_at"] = limits.get("_checked_at",time.time())
                row["cached"] = limits.get("_cached",False)
                row["warning"] = limits.get("_warning","")
                row["limited"] = bool(limits.get("limited"))
                for label, value, reset in (("All models · 5-hour", limits.get("used_5h"), limits.get("reset_5h")),
                                            ("All models · weekly", limits.get("used_7d"), limits.get("reset_7d"))):
                    if value is not None:
                        row["windows"].append({"label": label, "used": value, "reset": reset or 0})
                row["windows"] += [{"label": f"{item['model']} · weekly", "used": item["used"], "reset": item.get("reset") or 0}
                                    for item in limits.get("scoped", [])]
            except Exception as exc:
                row["error"] = str(exc)
        return row

    def codex_rows():
        try:
            overview = codex.overview(with_limits=refresh)
            credits = {r["alias"]:r for r in resetcredits.balances()} if refresh else {}
            rows = []
            for item in overview["accounts"]:
                limits = item.get("usage") or {}
                rows.append({"provider": "codex", "alias": item["alias"], "email": item.get("email") or "Unknown email",
                             "plan": item.get("plan") or "Unknown plan", "workspace": item.get("account_id") or "",
                             "active": bool(item.get("is_active")), "saved": bool(item.get("saved")),
                             "can_switch": bool(item.get("saved")), "can_launch": bool(item.get("saved")),
                             "reset_credits": credits.get(item["alias"], {}).get("available_count"),
                             "reset_credits_error": credits.get(item["alias"], {}).get("error") or "",
                             "windows": [{"label": w["label"], "used": w["used"], "reset": w.get("reset") or 0}
                                         for w in limits.get("windows", [])],
                             "checked_at": time.time() if limits and not limits.get("error") else 0,
                             "error": limits.get("error") or ("No quota reading returned" if refresh and not limits else "")})
            return rows, None
        except Exception as exc:
            return [], f"Codex discovery: {exc}"

    with ThreadPoolExecutor(max_workers=5) as pool:
        codex_job = pool.submit(codex_rows)
        result["accounts"] = list(pool.map(claude_row, claude_accounts))
        rows, error = codex_job.result()
        result["accounts"] += rows
        if error:
            result["errors"].append(error)
    result["accounts"] = group_accounts(result["accounts"])
    return result


def perform(provider, alias, operation, acknowledged=False):
    """Resolve account again at action time; never accept arbitrary commands."""
    if operation not in ("switch", "launch"):
        raise ValueError("Unsupported action")
    if provider == "claude":
        backend = for_platform()
        actions._find_account(backend, alias)
        if operation == "switch":
            return actions.switch(backend, alias, running_sessions(), acknowledged)
        return actions.launch(backend, alias)
    if provider=="codex" and operation=="launch":
        return codexlaunch.launch(alias)
    if provider != "codex" or operation != "switch":
        raise ValueError("This action is not available for that provider")
    if not acknowledged:
        raise ValueError("Changing the default requires confirmation")
    if not any(a["alias"] == alias and a["saved"] for a in codex.accounts()):
        raise ValueError("Saved Codex profile not found")
    completed = subprocess.run(bundled.command("hotseat.codex_accounts", "switch", alias), capture_output=True, text=True, timeout=30)
    if completed.returncode:
        raise ValueError(completed.stderr.strip() or "Account switch failed")
    return {"ok": True, "alias": alias}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("operation", choices=("snapshot", "switch", "launch", "work", "inspect-work", "resume-work", "reboot-work"))
    parser.add_argument("--refresh", action="store_true")
    parser.add_argument("--provider", choices=("codex", "claude"))
    parser.add_argument("--alias")
    parser.add_argument("--id")
    parser.add_argument("--revision")
    parser.add_argument("--acknowledged", action="store_true")
    args = parser.parse_args()
    try:
        if args.operation in ("work","inspect-work","resume-work","reboot-work"):
            from . import work
            if args.operation=="work": output=work.listing()
            elif args.operation=="inspect-work":output=work.detail(args.provider,args.id)
            else:output=work.act(args.provider,args.id,args.operation.split("-")[0],args.revision,args.acknowledged)
        elif args.operation == "snapshot":
            output = snapshot(args.refresh)
        else:
            if not args.alias or not args.provider:
                parser.error("actions require --alias and --provider")
            output = perform(args.provider, args.alias, args.operation, args.acknowledged)
        print(json.dumps(output))
    except Exception as exc:
        print(json.dumps({"error": str(exc)}))
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
