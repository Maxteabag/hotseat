"""The command line is the primary interface, so its contract is tested directly.

Nothing here touches the network or real credentials: the collector and the action
layer are stubbed.
"""

import io
import json
import unittest
import unittest.mock
from contextlib import redirect_stderr, redirect_stdout

from hotseat import cli

ACCOUNT = {
    "alias": "work", "email": "user@example.com", "org": "Example Org",
    "plan": "team", "is_active": True, "error": None,
    "access_hours_left": 4.0, "signin_days_left": 20.0, "signin_due_soon": False,
    "usage": {"status": "allowed", "available": True, "limited": False,
              "used_5h": 0.36, "used_7d": 0.53, "reset_5h": None, "reset_7d": None,
              "binding": "five_hour", "overage_status": "allowed"},
}
SNAPSHOT = {"generated_at": 0, "capabilities": {}, "sessions": 3,
            "accounts": [ACCOUNT], "model_usage": None, "error": None}


def run(argv):
    """Run the CLI, returning (exit code, stdout, stderr)."""
    out, err = io.StringIO(), io.StringIO()
    with redirect_stdout(out), redirect_stderr(err):
        code = cli.main(argv)
    return code, out.getvalue(), err.getvalue()


class ListTest(unittest.TestCase):
    def setUp(self):
        patcher = unittest.mock.patch.object(cli.Collector, "build", return_value=SNAPSHOT)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_list_shows_the_account(self):
        code, out, _ = run(["list"])
        self.assertEqual(code, 0)
        self.assertIn("work", out)
        self.assertIn("36%", out)
        self.assertIn("3 Claude session(s) running", out)

    def test_list_json_is_parseable(self):
        code, out, _ = run(["list", "--json"])
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(out)["accounts"][0]["alias"], "work")

    def test_json_output_carries_no_token(self):
        _, out, _ = run(["list", "--json"])
        self.assertNotIn("token", json.loads(out)["accounts"][0])

    def test_imminent_signin_is_called_out(self):
        soon = {**SNAPSHOT, "accounts": [{**ACCOUNT, "signin_days_left": 2.0,
                                          "signin_due_soon": True}]}
        with unittest.mock.patch.object(cli.Collector, "build", return_value=soon):
            _, out, _ = run(["list"])
        self.assertIn("sign-in needed soon", out.lower())

    def test_show_reports_one_account(self):
        code, out, _ = run(["show", "work"])
        self.assertEqual(code, 0)
        self.assertIn("Example Org", out)
        self.assertIn("binding limit", out)

    def test_show_unknown_account_fails(self):
        code, _, err = run(["show", "nope"])
        self.assertEqual(code, 1)
        self.assertIn("nope", err)


class SwitchTest(unittest.TestCase):
    """Switching retargets every running session, so it must never be quiet."""

    def setUp(self):
        patcher = unittest.mock.patch.object(cli, "running_sessions", return_value=4)
        patcher.start()
        self.addCleanup(patcher.stop)
        backend = unittest.mock.patch.object(cli, "for_platform")
        backend.start()
        self.addCleanup(backend.stop)

    def test_refuses_without_confirmation_when_not_interactive(self):
        with unittest.mock.patch.object(cli.actions, "switch") as action:
            with unittest.mock.patch("sys.stdin.isatty", return_value=False):
                code, out, err = run(["switch", "other"])
        self.assertEqual(code, 1)
        action.assert_not_called()
        self.assertIn("--yes", err)

    def test_warning_names_the_sessions_and_the_safe_alternative(self):
        with unittest.mock.patch.object(cli.actions, "switch"):
            with unittest.mock.patch("sys.stdin.isatty", return_value=False):
                _, out, _ = run(["switch", "other"])
        self.assertIn("4 running session", out)
        self.assertIn("hotseat use other", out)

    def test_yes_proceeds(self):
        with unittest.mock.patch.object(cli.actions, "switch",
                                        return_value={"switched": True}) as action:
            code, _, _ = run(["switch", "other", "--yes"])
        self.assertEqual(code, 0)
        action.assert_called_once()

    def test_a_failed_switch_reports_failure(self):
        with unittest.mock.patch.object(cli.actions, "switch",
                                        side_effect=cli.actions.ActionError("nope")):
            code, _, err = run(["switch", "other", "--yes"])
        self.assertEqual(code, 1)
        self.assertIn("nope", err)


class UseTest(unittest.TestCase):
    def test_use_replaces_the_process_with_a_pinned_session(self):
        with unittest.mock.patch.object(cli, "for_platform"):
            with unittest.mock.patch.object(cli.actions, "exec_session") as exec_session:
                run(["use", "work"])
        exec_session.assert_called_once()
        self.assertEqual(exec_session.call_args[0][1], "work")

    def test_extra_arguments_pass_through_to_claude(self):
        with unittest.mock.patch.object(cli, "for_platform"):
            with unittest.mock.patch.object(cli.actions, "exec_session") as exec_session:
                run(["use", "work", "--model", "haiku"])
        self.assertEqual(exec_session.call_args[0][2], ["--model", "haiku"])

    def test_failure_is_reported_rather_than_raised(self):
        with unittest.mock.patch.object(cli, "for_platform"):
            with unittest.mock.patch.object(cli.actions, "exec_session",
                                            side_effect=cli.actions.ActionError("no token")):
                code, _, err = run(["use", "work"])
        self.assertEqual(code, 1)
        self.assertIn("no token", err)


class VerifyTest(unittest.TestCase):
    def test_success(self):
        with unittest.mock.patch.object(cli, "for_platform"):
            with unittest.mock.patch.object(cli.actions, "verify",
                                            return_value={"alias": "work", "ok": True}):
                code, out, _ = run(["verify", "work"])
        self.assertEqual(code, 0)
        self.assertIn("live", out)

    def test_failure_exits_nonzero_and_says_why(self):
        with unittest.mock.patch.object(cli, "for_platform"):
            with unittest.mock.patch.object(cli.actions, "verify",
                                            side_effect=cli.actions.ActionError("expired")):
                code, _, err = run(["verify", "work"])
        self.assertEqual(code, 1)
        self.assertIn("expired", err)


class ModelsTest(unittest.TestCase):
    USAGE = {"days": 7, "as_of": "2026-01-01", "stale": False, "total_tokens": 1_500_000,
             "models": [{"model": "claude-fable-5-1", "label": "Fable 5.1",
                         "family": "fable", "tokens": 1_500_000, "share": 1.0}]}

    def test_models_renders(self):
        with unittest.mock.patch.object(cli.modelstats, "recent_by_model",
                                        return_value=self.USAGE):
            code, out, _ = run(["models"])
        self.assertEqual(code, 0)
        self.assertIn("Fable 5.1", out)

    def test_small_counts_are_not_rounded_away_to_zero(self):
        tiny = {**self.USAGE, "total_tokens": 225_000,
                "models": [{**self.USAGE["models"][0], "tokens": 225_000, "share": 0.0001}]}
        with unittest.mock.patch.object(cli.modelstats, "recent_by_model",
                                        return_value=tiny):
            _, out, _ = run(["models"])
        self.assertIn("225.0K", out)
        self.assertNotIn("0M", out)

    def test_absent_statistics_fail_cleanly(self):
        with unittest.mock.patch.object(cli.modelstats, "recent_by_model", return_value=None):
            code, _, err = run(["models"])
        self.assertEqual(code, 1)
        self.assertIn("No local usage statistics", err)


class PathDisplayTest(unittest.TestCase):
    def test_the_home_directory_is_shortened(self):
        from pathlib import Path as P
        self.assertEqual(cli._home(str(P.home() / "work" / "repo")), "~/work/repo")

    def test_a_path_outside_home_is_left_alone(self):
        self.assertEqual(cli._home("/var/tmp/thing"), "/var/tmp/thing")

    def test_no_path_is_safe(self):
        self.assertEqual(cli._home(None), "")


class UsageTest(unittest.TestCase):
    def test_bare_invocation_prints_help_and_signals_usage_error(self):
        code, out, _ = run([])
        self.assertEqual(code, 2)
        self.assertIn("usage:", out)

    def test_every_command_is_reachable(self):
        parser = cli.build_parser()
        actions_available = parser._subparsers._group_actions[0].choices
        for expected in ("list", "show", "verify", "use", "window", "switch",
                         "models", "statusline", "serve"):
            self.assertIn(expected, actions_available)


if __name__ == "__main__":
    unittest.main()
