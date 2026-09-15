import sys,time
from hotseat.cli import _paint,_emit,_home,BOLD,DIM,GREEN,RED,YELLOW
from hotseat.collect import Collector
from . import clarp,resume,inspect as inspect_mod

def cmd_clarp(args) -> int:
    """Clarp agents, and which account the running ones are spending."""
    try:
        snapshot = Collector(interval=0).build()
        default = next((a["alias"] for a in snapshot.get("accounts") or []
                        if a.get("is_active")), None)
        overview = clarp.overview(default_alias=default)
    except clarp.ClarpError as exc:
        print(f"✗ {exc}", file=sys.stderr)
        return 1

    if args.json:
        _emit(overview, True)
        return 0

    agents = overview["agents"]
    if args.live:
        agents = [a for a in agents if a["live"]]
    if args.backend:
        agents = [a for a in agents if a["backend"] == args.backend]

    if not agents:
        print("No Clarp agents match." if args.live or args.backend
              else "No Clarp agents defined.")
        return 0

    print(f"{'AGENT':<26} {'BACKEND':<8} {'STATE':<8} ACCOUNT")
    for agent in agents:
        state = _paint("live", GREEN) if agent["live"] else _paint("idle", DIM)
        pad = len(state) - (4)
        if agent["live"]:
            account = agent["account"] or "default"
            if agent["pinned"]:
                account += " (pinned)"
        elif agent["backend"] == clarp.CLAUDE_BACKEND:
            account = _paint(f"would use {agent['would_use'] or 'default'}", DIM)
        else:
            account = "—"
        print(f"{(agent['persona'] or agent['session'])[:25]:<26} "
              f"{(agent['backend'] or '?'):<8} {state:<{8 + pad}} {account}")

    print()
    backends = ", ".join(f"{n} {b}" for b, n in sorted(overview["by_backend"].items()))
    print(_paint(f"{overview['total']} agents ({backends}); {overview['live']} live.", DIM))
    if overview["live_by_account"]:
        spend = ", ".join(f"{alias}: {n}" for alias, n in
                          sorted(overview["live_by_account"].items()))
        print(f"Live Claude agents by account — {spend}")
    elif overview["claude_backed"]:
        print(_paint(f"{overview['claude_backed']} agents are Claude-backed; none are "
                     f"running, so none are drawing quota right now.", DIM))
    return 0

def cmd_resume(args) -> int:
    """List, and optionally continue, work that a usage limit stopped."""
    try:
        items = resume.stopped(window_days=args.days)
    except resume.ResumeError as exc:
        print(f"✗ {exc}", file=sys.stderr)
        return 1

    if args.kind:
        items = [i for i in items if i["kind"] == args.kind]
    if args.cause:
        items = [i for i in items if i.get("cause") == args.cause]

    snapshot = Collector(interval=0).build()
    accounts = snapshot.get("accounts") or []
    default = next((a["alias"] for a in accounts if a.get("is_active")), None)
    for item in items:
        item["readiness"] = resume.readiness(item, accounts, default)

    if args.json:
        payload = {"items": items, "default_account": default}
        results = []
        if args.go:
            for item in items:
                if item["kind"] != "clarp" or not item["readiness"]["ready"]:
                    continue
                try:
                    results.append(resume.continue_item(item))
                except resume.ResumeError as exc:
                    results.append({"id": item["id"], "continued": False,
                                    "error": str(exc)})
            payload["results"] = results
        _emit(payload, True)
        return 1 if any("error" in result for result in results) else 0

    if not items:
        print(f"Nothing was stopped by a usage limit in the last {args.days} days.")
        return 0

    ready = [i for i in items if i["readiness"]["ready"]]
    blocked = [i for i in items if not i["readiness"]["ready"]]

    for item in items:
        when = (time.strftime("%d %b %H:%M", time.localtime(item["stopped_at"]))
                if item.get("stopped_at") else "—")
        state = item["readiness"]
        if state["hard"]:
            shown = _paint(state["hard"], YELLOW)
        elif item["kind"] == "native":
            shown = _paint("needs a terminal", DIM)
        else:
            shown = _paint("ready", GREEN)
        if state["warning"] and not state["hard"]:
            shown += _paint(f"  ({state['warning']})", DIM)
        cause = {"usage_limit": "usage limit",
                 "queue_paused": "queue paused",
                 "waiting_for_account": "waiting for quota"}.get(item.get("cause"), "stopped")
        waiting = _paint(f" · {item['pending']} waiting", RED) if item.get("pending") else ""

        print(f"{_paint(item['name'][:28], BOLD)}{waiting}")
        print(f"  {_paint(item['kind'] + ' · ' + cause + ' · ' + when, DIM)}")
        if item.get("cwd"):
            print(f"  {_paint(_home(item['cwd']), DIM)}")
        if item.get("last_message"):
            print(f"  {_paint('said:', DIM)} {item['last_message']}")
        print(f"  {shown}")
        print()

    print()
    starving = resume.starving_models(items, accounts)
    if starving:
        print(_paint("--- one model is holding up the rest ---", YELLOW))
        for model, names in starving.items():
            who = ", ".join(names[:4]) + ("…" if len(names) > 4 else "")
            print(f"  No account can serve {_paint(model, BOLD)}, wanted by {who}.")
        print("  Clarp requires one account to serve every parked agent at once, so"
              "\n  these keep the other parked agents waiting too. Move them to a model"
              "\n  that has quota, or release them, and the rest recover.")
        print()

    print(_paint("Inspect any of these: hotseat inspect <name>", DIM))
    paused = [i for i in items if i.get("cause") == "queue_paused"]
    if paused:
        waiting = sum(i.get("pending") or 0 for i in paused)
        print(_paint(f"{len(paused)} agent(s) have a paused queue"
                     + (f", holding {waiting} turn(s) that will not run" if waiting else "")
                     + ". A prompt would queue behind the pause, not run, so these are "
                       "not continued here. Clear the pause in the Clarp app.", YELLOW))
    exhausted = [i for i in blocked if i.get("cause") != "queue_paused"]
    if exhausted:
        print(_paint(f"{len(exhausted)} cannot continue yet; continuing into an "
                     f"exhausted account just fails again.", DIM))

    clarp_ready = [i for i in ready if i["kind"] == "clarp"]
    native = [i for i in ready if i["kind"] == "native"]

    if not args.go:
        if clarp_ready:
            print(f"{len(clarp_ready)} Clarp agent(s) can be continued now: "
                  f"{_paint('hotseat resume --go', BOLD)}")
        if native:
            print(f"{len(native)} native session(s) have no supervisor to prompt. "
                  f"Resume one yourself:")
            for item in native[:5]:
                print(f"  cd {item['cwd']} && claude --resume {item['id']}")
            if len(native) > 5:
                print(f"  ... and {len(native) - 5} more (--json for all)")
        return 0

    if not clarp_ready:
        print("Nothing to continue.")
        return 0

    failures = 0
    for item in clarp_ready:
        try:
            resume.continue_item(item)
            print(_paint(f"✓ continued {item['name']}", GREEN))
        except resume.ResumeError as exc:
            failures += 1
            print(_paint(f"✗ {item['name']}: {exc}", RED), file=sys.stderr)
    return 1 if failures else 0

def cmd_inspect(args) -> int:
    """What one stopped piece of work was doing, so you can decide about it."""
    try:
        detail = inspect_mod.detail(args.id)
    except inspect_mod.InspectError as exc:
        print(f"✗ {exc}", file=sys.stderr)
        return 1
    if args.json:
        _emit(detail, True)
        return 0

    print(f"{_paint(detail['name'], BOLD)}  {detail['kind']} · {detail.get('backend') or '?'}"
          + (f" · {detail['model']}" if detail.get("model") else ""))
    if detail.get("cwd"):
        branch = f" ({detail['branch']})" if detail.get("branch") else ""
        print(f"  {_paint('working in', DIM)} {detail['cwd']}{branch}")
    if detail.get("last_activity"):
        print(f"  {_paint('last activity', DIM)} {detail['last_activity']}")
    print()

    if detail.get("summary"):
        print(_paint("What it was working on", BOLD))
        print(f"  {detail['summary']}")
        print()
    if detail.get("last_user"):
        print(_paint("Last asked", BOLD))
        print(f"  {detail['last_user']}")
        print()
    if detail.get("last_assistant"):
        print(_paint("Last said", BOLD))
        print(f"  {detail['last_assistant']}")
        print()
    for item in detail.get("queued") or []:
        print(_paint(f"Queued and waiting ({item['origin'] or 'unknown'})", YELLOW))
        print(f"  {item['text']}")
        print()
    if args.full:
        print(_paint("Recent exchange", BOLD))
        for turn in detail.get("recent") or []:
            who = "you" if turn["role"] == "user" else detail["name"]
            print(f"  {_paint(who + ':', DIM)} {turn['text']}")
    return 0
