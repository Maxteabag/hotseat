"""934 fixed-nine synthetic emitter controller; no login/CLI/install actions."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import time
import traceback

SOURCE = "48425e53b37a1df0749e16124cd26672f7d15c76"
BRANCH = "refs/heads/ci/auth934-go-48425e5-934e0001"
ROSTER = {
    "collect": [
        "TestStructuredAuthMetadataUsesNativeCauseAndPreservesFallbackPolicy",
        "TestStructuredAuthMetadataIsOnlyTwoAdditionsToFrozenLegacyAccountBytes",
        "TestStructuredAuthMetadataKeepsSameNameAndEmailOrganizationsDistinct",
        "TestStructuredAuthMetadataDoesNotPromoteGenericRefreshFailureOrProbeOldToken",
        "TestStructuredAuthMetadataPreservesTypedRefreshCauseWithoutProbingOldToken",
        "TestStructuredAuthMetadataDoesNotAuthorizeAReboundRefreshIdentity",
    ],
    "quota": [
        "TestUsageErrorKeepsNativeHTTPStatusWithoutChangingLegacyMessage",
        "TestTransportTimeoutAndCancellationRemainUnwrappableWithoutAuthGuessing",
        "TestHeaderProbeRetainsItsOwnCredentialRejectionStatus",
    ],
}

class ControllerFailure(Exception):
    pass


def main():
    start = time.monotonic()
    whole_deadline = start + 510
    cleanup_reserve_s = 10
    result_code = 1
    workspace = Path(os.environ["GITHUB_WORKSPACE"])
    out = workspace / "auth934-go-evidence"
    out.mkdir(mode=0o700)
    phases = []
    summary = {"source": SOURCE, "controller": os.environ["GITHUB_SHA"],
               "branch": os.environ["GITHUB_REF"], "roster": ROSTER,
               "executed_tests_claim": False, "phases": phases, "result": "failed"}
    active = [None]

    def interruption(signum, frame):
        raise ControllerFailure("controller_interrupted_" + str(signum))
    signal.signal(signal.SIGTERM, interruption)
    signal.signal(signal.SIGINT, interruption)

    def run(name, argv, env, cwd, seconds):
        # One clock from controller entry. File opening and spawn consume this
        # phase's time; ten seconds remain for owned cleanup and final receipts.
        phase_start = time.monotonic()
        phase_deadline = min(phase_start + seconds, whole_deadline - cleanup_reserve_s)
        row = {"name": name, "argv": argv, "deadline_monotonic": phase_deadline,
               "started_monotonic": phase_start, "actual_exit": None,
               "ownership": "not_spawned", "closure": "no_actor_created"}
        phases.append(row)
        proc = None
        try:
            with (out / (name + ".stdout")).open("wb") as stdout, (out / (name + ".stderr")).open("wb") as stderr:
                remaining = phase_deadline - time.monotonic()
                row["remaining_before_spawn_s"] = remaining
                if remaining <= 0:
                    raise ControllerFailure(name + "_no_budget_before_spawn")
                proc = subprocess.Popen(argv, cwd=cwd, env=env, stdout=stdout,
                                        stderr=stderr, start_new_session=True)
                active[0] = proc
                row.update(pid=proc.pid, ownership="private_unreaped_leader", closure="pending")
                # Do NOT restart a deadline after Popen; setup/spawn may be slow.
                while True:
                    if time.monotonic() >= phase_deadline:
                        row["timed_out"] = True
                        raise ControllerFailure(name + "_timeout")
                    event = os.waitid(os.P_PID, proc.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
                    if event is not None:
                        break
                    time.sleep(0.02)
        except BaseException:
            # A cleanup exception may replace the propagating exception, so
            # retain the original full diagnostic as well as Python's chain.
            row["primary_exception_traceback"] = traceback.format_exc()
            raise
        finally:
            try:
                if proc is not None:
                    # WNOWAIT reserves this private group number through the
                    # LAST signal. Unknown identity grants no signal or wait.
                    try:
                        os.waitid(os.P_PID, proc.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
                    except (ChildProcessError, OSError):
                        proc.returncode = sys.maxsize
                        row.update(ownership="unknown_no_numeric_signal_or_wait", closure="UNKNOWN")
                        raise ControllerFailure(name + "_ownership_unknown")
                    row["closure"] = "UNKNOWN"  # stays unknown unless wait succeeds
                    cleanup_deadline = min(whole_deadline - 3, time.monotonic() + 6)
                    for sig in (signal.SIGTERM, signal.SIGKILL):
                        try:
                            os.killpg(proc.pid, sig)
                        except ProcessLookupError:
                            row.setdefault("signals", []).append([int(sig), "group_not_found"])
                        else:
                            row.setdefault("signals", []).append([int(sig), "sent"])
                        if sig == signal.SIGTERM:
                            grace = min(0.2, max(0, cleanup_deadline - time.monotonic()))
                            if grace:
                                time.sleep(grace)
                    remaining = min(5, cleanup_deadline - time.monotonic())
                    if remaining <= 0:
                        proc.returncode = sys.maxsize
                        row["ownership"] = "cleanup_budget_exhausted_unreaped_no_late_retry"
                        raise ControllerFailure(name + "_cleanup_budget_exhausted")
                    try:
                        row["actual_exit"] = proc.wait(timeout=remaining)
                    except subprocess.TimeoutExpired:
                        proc.returncode = sys.maxsize
                        row["ownership"] = "drain_unknown_no_late_numeric_retry"
                        raise ControllerFailure(name + "_drain_unknown")
                    else:
                        row.update(ownership="leader_reaped_after_last_signal",
                                   closure="owned_leader_reaped_not_arbitrary_descendant_proof")
                        active[0] = None  # ONLY positive owned-leader reap clears it
            except BaseException:
                row["cleanup_exception_traceback"] = traceback.format_exc()
                if proc is not None:
                    proc.returncode = sys.maxsize
                    row["closure"] = "UNKNOWN"
                raise
            finally:
                row["elapsed_s"] = time.monotonic() - phase_start
                (out / (name + ".receipt.json")).write_text(json.dumps(row, indent=2) + "\n")
        if time.monotonic() >= phase_deadline:
            raise ControllerFailure(name + "_phase_budget_including_cleanup_exceeded")
        if time.monotonic() >= whole_deadline - 3:
            raise ControllerFailure("whole_deadline_cleanup_or_receipt_reserve")
        if row["actual_exit"] != 0:
            raise ControllerFailure(name + "_nonzero")

    try:
        if os.environ.get("GITHUB_REPOSITORY") != "Maxteabag/hotseat" or os.environ.get("GITHUB_EVENT_NAME") != "push" or summary["branch"] != BRANCH:
            raise ControllerFailure("unexpected_context")
        if not hasattr(os, "WNOWAIT") or signal.getsignal(signal.SIGCHLD) != signal.SIG_DFL:
            raise ControllerFailure("no_private_wait_ownership")
        source = workspace / "source"
        controller = workspace / "controller"
        def git(path, *args):
            return subprocess.check_output(["/usr/bin/git", "-C", str(path), *args], timeout=10).decode().strip()
        if git(source, "rev-parse", "HEAD") != SOURCE or git(controller, "rev-parse", "HEAD") != summary["controller"]:
            raise ControllerFailure("checkout_pin_mismatch")
        if git(source, "status", "--porcelain", "--untracked-files=no"):
            raise ControllerFailure("modified_source")
        (out / "source-tree.txt").write_text(git(source, "ls-tree", "-r", "HEAD") + "\n")
        for relative in (".github/workflows/auth934-emitter-nine.yml", "tools/auth934/emitter_nine.py"):
            summary.setdefault("controller_file_sha256", {})[relative] = hashlib.sha256((controller / relative).read_bytes()).hexdigest()
        go = shutil.which("go")
        unshare = shutil.which("unshare")
        if not go or not unshare:
            raise ControllerFailure("missing_toolchain_or_namespace")
        home = workspace / "auth934-private"
        home.mkdir(mode=0o700)
        for part in ("tmp", "cache", "config", "data", "modcache", "gocache"):
            (home / part).mkdir(mode=0o700)
        # No global umask changes. This allowlist never copies auth/cloud/GitHub
        # tokens, live HOME, profile paths, proxy credentials or Go user settings.
        env = {"PATH": str(Path(go).parent) + ":/usr/bin:/bin", "HOME": str(home),
               "TMPDIR": str(home / "tmp"), "XDG_CACHE_HOME": str(home / "cache"),
               "XDG_CONFIG_HOME": str(home / "config"), "XDG_DATA_HOME": str(home / "data"),
               "GOMODCACHE": str(home / "modcache"), "GOCACHE": str(home / "gocache"),
               "GOTOOLCHAIN": "local", "GOENV": "off", "GOFLAGS": "-mod=readonly",
               "CGO_ENABLED": "0", "LANG": "C.UTF-8", "GOPROXY": "https://proxy.golang.org",
               "GOSUMDB": "sum.golang.org"}
        run("toolchain", [go, "version"], env, source, 10)
        if not re.search(r"\bgo1\.26\.0\b", (out / "toolchain.stdout").read_text()):
            raise ControllerFailure("toolchain_version_mismatch")
        # Network is used only for locked public Go modules. No provider probe.
        run("dependencies", [go, "mod", "download"], env, source, 120)
        env.update(GOPROXY="off", GOSUMDB="off")
        for package, names in ROSTER.items():
            expr = "^(" + "|".join(names) + ")$"
            # Fresh network namespace: no external provider route. The reviewed
            # clientFor fixture uses httptest.NewRecorder, NOT a listener.
            argv = [unshare, "--user", "--map-root-user", "--net", "--", go,
                    "test", "-json", "./internal/" + package, "-count=1", "-timeout=120s", "-run", expr]
            run(package, argv, env, source, 180)
            events = [json.loads(line) for line in (out / (package + ".stdout")).read_text().splitlines() if line]
            top = [event for event in events if event.get("Test") and "/" not in event["Test"] and event.get("Action") in {"pass", "fail", "skip"}]
            if sorted(event["Test"] for event in top) != sorted(names) or any(event["Action"] != "pass" for event in top):
                raise ControllerFailure(package + "_missing_duplicate_skipped_or_failed_terminal")
            if any(event.get("Action") in {"fail", "skip"} for event in events):
                raise ControllerFailure(package + "_subtest_or_package_failure")
            summary.setdefault("terminal_outcomes", {})[package] = top
        if active[0] is not None or time.monotonic() >= whole_deadline - 3:
            raise ControllerFailure("whole_deadline_or_unresolved_actor_before_terminal")
        summary["executed_tests_claim"] = True
        summary["result"] = "passed_nine_synthetic_only"
        result_code = 0
    except BaseException as exc:
        traceback.print_exc(file=sys.stderr, chain=True)
        summary["error_type"] = type(exc).__name__
        summary["error_message"] = str(exc)
        summary["exception_traceback"] = traceback.format_exc()
        summary["error_code"] = str(exc) if isinstance(exc, ControllerFailure) else "controller_setup_or_cleanup_error"
        result_code = 1
    finally:
        summary["elapsed_s"] = time.monotonic() - start
        summary["active_owned_handle"] = active[0] is not None
        summary["unresolved_actor"] = ({"pid": active[0].pid, "closure": "UNKNOWN",
            "action": "retain_scratch_and_fail_no_numeric_retry"} if active[0] is not None else None)
        summary["whole_deadline_monotonic"] = whole_deadline
        if time.monotonic() >= whole_deadline:
            summary.update(result="failed", error_code="whole_deadline_before_final_receipts")
            result_code = 1
        # Keep scratch until Actions disposes the isolated runner; never delete
        # resources after uncertain ownership. Artifacts include ONLY out.
        (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        index = {str(path.relative_to(out)): hashlib.sha256(path.read_bytes()).hexdigest()
                 for path in sorted(out.iterdir()) if path.is_file()}
        (out / "SHA256-INDEX.json").write_text(json.dumps(index, indent=2) + "\n")
        # Include teardown, parsing, summary and index writes in the SAME bound.
        # Kernel/filesystem calls cannot be forcibly bounded here: late return
        # fails rather than claiming a passing terminal inside 510 seconds.
        if time.monotonic() >= whole_deadline:
            print("controller whole deadline exceeded during final receipts", file=sys.stderr)
            summary.update(result="failed", error_code="whole_deadline_after_final_receipts",
                           elapsed_s=time.monotonic() - start)
            (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
            index["summary.json"] = hashlib.sha256((out / "summary.json").read_bytes()).hexdigest()
            (out / "SHA256-INDEX.json").write_text(json.dumps(index, indent=2) + "\n")
            result_code = 1
    return result_code

if __name__ == "__main__":
    sys.exit(main())
