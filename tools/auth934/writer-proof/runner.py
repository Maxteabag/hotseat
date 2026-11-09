"""934 fixed-fourteen public writer controller; no login/CLI/install actions."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import subprocess
import sys
import time
import traceback

SOURCE = "48425e53b37a1df0749e16124cd26672f7d15c76"
BRANCH = "refs/heads/ci/auth934-go-48425e5-writer001"
from observer import ROSTER, terminal_outcomes

class ControllerFailure(Exception):
    pass


def dropped_exec():
    """Executed only AFTER setpriv. Refuse before Go if any drop premise fails."""
    uid, gid = int(sys.argv[2]), int(sys.argv[3])
    proof = Path(sys.argv[4])
    command = sys.argv[5:]
    if uid <= 0 or gid <= 0 or os.getuid() != uid or os.geteuid() != uid or os.getgid() != gid or os.getegid() != gid:
        raise ControllerFailure("runner_identity_not_dropped")
    if os.getgroups() not in ([], [gid]):
        raise ControllerFailure("supplementary_groups_not_dropped")
    status = dict(line.split(":", 1) for line in Path("/proc/self/status").read_text().splitlines() if ":" in line)
    if status.get("NoNewPrivs", "").strip() != "1":
        raise ControllerFailure("no_new_privs_not_set")
    for name in ("CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"):
        if int(status[name].strip(), 16) != 0:
            raise ControllerFailure("capabilities_not_empty_" + name)
    allowed = {"PATH", "HOME", "TMPDIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
               "GOMODCACHE", "GOCACHE", "GOTOOLCHAIN", "GOENV", "GOFLAGS", "CGO_ENABLED", "LANG", "GOPROXY", "GOSUMDB"}
    if set(os.environ) != allowed or os.environ["GOPROXY"] != "off" or os.environ["GOSUMDB"] != "off":
        raise ControllerFailure("dropped_environment_not_exact_offline_allowlist")
    if {name for _, name in socket.if_nameindex()} != {"lo"} or Path("/proc/net/route").read_text().splitlines()[1:]:
        raise ControllerFailure("namespace_has_non_loopback_interface_or_route")
    proof.write_text(json.dumps({"uid": uid, "gid": gid, "groups": os.getgroups(),
        "NoNewPrivs": 1, "capabilities": "all_zero", "network": "loopback_interface_only_no_ipv4_route",
        "environment_keys": sorted(os.environ), "scope": "network_only_not_mount_or_pid_sandbox"}, indent=2) + "\n")
    # No privileged source/test/Go command precedes this owned drop proof.
    os.execve(command[0], command, dict(os.environ))


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

    def run(name, argv, env, cwd, seconds, *, privileged_namespace_wrapper=False):
        # One clock from controller entry. File opening and spawn consume this
        # phase's time; ten seconds remain for owned cleanup and final receipts.
        phase_start = time.monotonic()
        phase_deadline = min(phase_start + seconds, whole_deadline - cleanup_reserve_s)
        row = {"name": name, "argv": argv, "deadline_monotonic": phase_deadline,
               "started_monotonic": phase_start, "actual_exit": None,
               "ownership": "not_spawned", "closure": "no_actor_created",
               "privileged_namespace_wrapper": privileged_namespace_wrapper}
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
                        row["natural_terminal_observed"] = True
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
                    # sudo may retain a root monitor even after its dropped
                    # command exits. A naturally exited owned guardian needs a
                    # wait, not a signal an unprivileged parent cannot deliver.
                    if privileged_namespace_wrapper and row.get("natural_terminal_observed"):
                        row["signal_scope"] = "natural_guardian_exit_wait_only_no_descendant_claim"
                    else:
                        for sig in (signal.SIGTERM, signal.SIGKILL):
                            try:
                                os.killpg(proc.pid, sig)
                            except ProcessLookupError:
                                row.setdefault("signals", []).append([int(sig), "group_not_found"])
                            except PermissionError:
                                # No sudo kill, privilege fallback or late retry.
                                # Retain UNKNOWN; hosted runner owns final disposal.
                                row["ownership"] = "privileged_guardian_signal_refused_no_retry"
                                raise ControllerFailure(name + "_guardian_cleanup_unknown")
                            else:
                                row.setdefault("signals", []).append([int(sig), "sent_unprivileged_only"])
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
                        row.update(ownership=("naturally_exited_guardian_reaped" if privileged_namespace_wrapper and row.get("natural_terminal_observed")
                                              else "leader_reaped_after_last_signal"),
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
        for relative in (".github/workflows/auth934-writer-fourteen.yml", "tools/auth934/writer-proof/runner.py", "tools/auth934/writer-proof/observer.py"):
            summary.setdefault("controller_file_sha256", {})[relative] = hashlib.sha256((controller / relative).read_bytes()).hexdigest()
        go = shutil.which("go")
        sudo, unshare, setpriv, env_bin, python_bin = ("/usr/bin/sudo", "/usr/bin/unshare", "/usr/bin/setpriv", "/usr/bin/env", "/usr/bin/python3")
        if not go or not all(os.access(path, os.X_OK) for path in (sudo, unshare, setpriv, env_bin, python_bin)):
            raise ControllerFailure("missing_toolchain_or_namespace_drop_tools")
        runner_uid, runner_gid = os.getuid(), os.getgid()
        if runner_uid <= 0 or runner_gid <= 0:
            raise ControllerFailure("controller_must_be_runner_not_root")
        summary["namespace_pattern"] = "sudo_create_then_immediate_setpriv_runner_drop"
        summary["runner_identity"] = {"uid": runner_uid, "gid": runner_gid}
        summary["containment_scope"] = "network_only_not_full_ci_contained_mount_pid_home_sandbox"
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
            # Exact privilege sequence from Clarp scripts/ci_contained.sh:
            # only namespace CREATE is privileged, then immediate runner drop,
            # empty inherited/ambient/bounding caps and no_new_privs. Rebuild the
            # private allowlist AFTER sudo; never run Go/tests as root. No umask
            # change or mount/home sandbox claim is introduced by this variant.
            argv = [sudo, "-n", "--", unshare, "--net", "--", setpriv,
                    "--reuid=" + str(runner_uid), "--regid=" + str(runner_gid),
                    "--clear-groups", "--no-new-privs", "--inh-caps=-all", "--ambient-caps=-all", "--bounding-set=-all", "--",
                    env_bin, "-i", *[key + "=" + value for key, value in sorted(env.items())],
                    python_bin, str(controller / "tools/auth934/writer-proof/runner.py"), "--dropped-exec",
                    str(runner_uid), str(runner_gid), str(out / (package + ".drop-proof.json")),
                    go, "test", "-json", "./internal/" + package, "-count=1", "-timeout=120s", "-run", expr]
            run(package, argv, env, source, 180, privileged_namespace_wrapper=True)
            events = [json.loads(line) for line in (out / (package + ".stdout")).read_text().splitlines() if line]
            try:
                top = terminal_outcomes(events, names)
            except ValueError as exc:
                raise ControllerFailure(package + "_" + str(exc)) from exc
            summary.setdefault("terminal_outcomes", {})[package] = top
        if active[0] is not None or time.monotonic() >= whole_deadline - 3:
            raise ControllerFailure("whole_deadline_or_unresolved_actor_before_terminal")
        summary["executed_tests_claim"] = True
        summary["result"] = "passed_fourteen_public_writer_controls_only"
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
    if len(sys.argv) > 1 and sys.argv[1] == "--dropped-exec":
        dropped_exec()
    else:
        sys.exit(main())
