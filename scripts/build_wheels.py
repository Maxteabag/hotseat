"""Build the sdist and one wheel per platform with the Go TUI bundled inside.

Each wheel is a pure-Python package plus a cross-compiled `hotseat-tui` binary
in `hotseat/_bin/`, retagged from `py3-none-any` to the platform it targets.
The sdist carries no binary; `hotseat tui` then falls back to a binary on PATH
or tells you how to build one.

    python3 scripts/build_wheels.py            # dist/: sdist + 4 wheels
    python3 scripts/build_wheels.py --targets linux-amd64

Requires Go, and the `build` and `wheel` Python packages.
"""
from __future__ import annotations

import argparse
import os
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BIN_DIR = ROOT / "hotseat" / "_bin"
DIST = ROOT / "dist"

# name -> (GOOS, GOARCH, wheel platform tag)
TARGETS = {
    "linux-amd64": ("linux", "amd64", "manylinux_2_17_x86_64"),
    "linux-arm64": ("linux", "arm64", "manylinux_2_17_aarch64"),
    "darwin-amd64": ("darwin", "amd64", "macosx_12_0_x86_64"),
    "darwin-arm64": ("darwin", "arm64", "macosx_12_0_arm64"),
}


def run(*cmd: str, env: dict[str, str] | None = None) -> None:
    print("+", " ".join(cmd), flush=True)
    subprocess.run(cmd, cwd=ROOT, env=env, check=True)


def version() -> str:
    ns: dict[str, str] = {}
    exec((ROOT / "hotseat" / "__init__.py").read_text(), ns)
    return ns["__version__"]


def clean_bin() -> None:
    """Drop the staged binary and the egg-info whose SOURCES.txt would remember it."""
    shutil.rmtree(BIN_DIR, ignore_errors=True)
    shutil.rmtree(ROOT / "hotseat.egg-info", ignore_errors=True)


def build_binary(goos: str, goarch: str, out: Path, ver: str) -> None:
    out.parent.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0")
    run("go", "build", "-trimpath", "-ldflags", f"-s -w -X main.version={ver}",
        "-o", str(out), "./cmd/hotseat-tui", env=env)


def build_sdist() -> None:
    clean_bin()
    run(sys.executable, "-m", "build", "--sdist", "--outdir", str(DIST))


def build_wheel(name: str, ver: str) -> Path:
    goos, goarch, plat = TARGETS[name]
    clean_bin()
    build_binary(goos, goarch, BIN_DIR / "hotseat-tui", ver)
    staging = DIST / f"pure-{name}"
    shutil.rmtree(staging, ignore_errors=True)
    run(sys.executable, "-m", "build", "--wheel", "--outdir", str(staging))
    pure = next(staging.glob("*-py3-none-any.whl"))
    run(sys.executable, "-m", "wheel", "tags", "--remove", "--platform-tag", plat, str(pure))
    tagged = next(staging.glob("*.whl"))
    final = DIST / tagged.name
    if final.exists():
        final.unlink()
    shutil.move(str(tagged), final)
    shutil.rmtree(staging)
    clean_bin()
    return final


def build_binaries(names: list[str], ver: str) -> list[Path]:
    """Standalone binaries for the GitHub release, beside the wheels."""
    out = []
    for name in names:
        goos, goarch, _ = TARGETS[name]
        path = DIST / f"hotseat-tui-{ver}-{name}"
        build_binary(goos, goarch, path, ver)
        out.append(path)
    return out


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--targets", default=",".join(TARGETS), help="comma-separated subset of: " + ", ".join(TARGETS))
    parser.add_argument("--no-sdist", action="store_true")
    parser.add_argument("--binaries", action="store_true", help="also emit standalone hotseat-tui binaries")
    args = parser.parse_args()
    names = [n.strip() for n in args.targets.split(",") if n.strip()]
    unknown = [n for n in names if n not in TARGETS]
    if unknown:
        parser.error("unknown targets: " + ", ".join(unknown))
    DIST.mkdir(exist_ok=True)
    ver = version()
    built: list[Path] = []
    if not args.no_sdist:
        build_sdist()
    for name in names:
        built.append(build_wheel(name, ver))
    if args.binaries:
        built += build_binaries(names, ver)
    print("\nBuilt:")
    for p in sorted(DIST.iterdir()):
        if p.is_file():
            print(f"  {p.name}  {p.stat().st_size // 1024} KiB")
    return 0


if __name__ == "__main__":
    sys.exit(main())
