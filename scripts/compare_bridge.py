#!/usr/bin/env python3
"""Diff the Go port's bridge output against the Python bridge on this machine.

Both implementations read the same credential files and call the same endpoints,
so after normalising the fields that legitimately differ between two runs
(timestamps, live quota fractions, cache flags) the JSON must be identical.

    scripts/compare_bridge.py snapshot            # cached quota only
    scripts/compare_bridge.py snapshot --refresh  # live quota reads
    scripts/compare_bridge.py work
    scripts/compare_bridge.py cli list codex models resets sessions resume

The Go binary defaults to ./bin/hotseat; override with HOTSEAT_GO.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
GO_BIN = os.environ.get("HOTSEAT_GO", str(ROOT / "bin" / "hotseat"))
VOLATILE = {"generated_at", "checked_at", "reset", "used", "cached", "warning", "revision",
            "updated", "last_at", "stopped_at", "last_activity", "quota_age_s", "token_hours_left",
            "access_hours_left", "hours_left", "holders", "live", "pid"}


def run(argv: list[str], timeout: int = 600) -> dict | list:
    out = subprocess.run(argv, capture_output=True, text=True, timeout=timeout, cwd=ROOT)
    if out.returncode != 0 and not out.stdout.strip():
        raise SystemExit(f"{' '.join(argv)} failed:\n{out.stderr}")
    return json.loads(out.stdout)


def normalise(value):
    if isinstance(value, dict):
        return {k: normalise(v) for k, v in sorted(value.items()) if k not in VOLATILE}
    if isinstance(value, list):
        return [normalise(v) for v in value]
    if isinstance(value, float):
        return round(value, 3)
    return value


def diff(a, b, path="") -> list[str]:
    if type(a) is not type(b):
        return [f"{path}: python={a!r} go={b!r}"]
    if isinstance(a, dict):
        out = []
        for key in sorted(set(a) | set(b)):
            if key not in a:
                out.append(f"{path}.{key}: only in go = {b[key]!r}")
            elif key not in b:
                out.append(f"{path}.{key}: only in python = {a[key]!r}")
            else:
                out += diff(a[key], b[key], f"{path}.{key}")
        return out
    if isinstance(a, list):
        if len(a) != len(b):
            return [f"{path}: length python={len(a)} go={len(b)}"]
        return [d for i, (x, y) in enumerate(zip(a, b)) for d in diff(x, y, f"{path}[{i}]")]
    return [] if a == b else [f"{path}: python={a!r} go={b!r}"]


def compare(label: str, py_argv: list[str], go_argv: list[str]) -> bool:
    py = normalise(run(py_argv))
    go = normalise(run(go_argv))
    problems = diff(py, go)
    print(f"== {label}: {'OK' if not problems else f'{len(problems)} difference(s)'}")
    for line in problems[:60]:
        print("   " + line)
    return not problems


def main(argv: list[str]) -> int:
    if not argv:
        print(__doc__)
        return 2
    ok = True
    op, rest = argv[0], argv[1:]
    if op in ("snapshot", "work"):
        py = [sys.executable, "-m", "hotseat.tui_bridge", op, *rest]
        go = [GO_BIN, "bridge", op, *rest]
        ok &= compare(f"bridge {op} {' '.join(rest)}", py, go)
    elif op == "cli":
        for sub in rest or ["list", "codex", "models", "resets", "sessions", "resume"]:
            py = [sys.executable, "-m", "hotseat", sub, "--json"]
            go = [GO_BIN, sub, "--json"]
            ok &= compare(f"cli {sub} --json", py, go)
    else:
        print(f"unknown operation {op}")
        return 2
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
