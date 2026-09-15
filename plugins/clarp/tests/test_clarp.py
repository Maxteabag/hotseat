"""Clarp agent discovery and account attribution."""

import json
import subprocess
import unittest
import unittest.mock

from hotseat_clarp import clarp

REGISTRY = [
    {"session": "adam", "persona": "Adam", "backend": "claude", "cwd": "/home/u"},
    {"session": "bella", "persona": "Bella", "backend": "claude", "cwd": "/home/u"},
    {"session": "cleo", "persona": "Cleo", "backend": "codex", "cwd": "/home/u"},
]


def completed(stdout="", code=0, stderr=""):
    return subprocess.CompletedProcess([], code, stdout=stdout, stderr=stderr)


class SessionsTest(unittest.TestCase):
    def test_missing_clarp_is_a_clear_message(self):
        with unittest.mock.patch.object(clarp, "available", return_value=False):
            with self.assertRaises(clarp.ClarpError) as ctx:
                clarp.sessions()
        self.assertIn("clarp-admin", str(ctx.exception))

    def test_registry_is_parsed(self):
        with unittest.mock.patch.object(clarp, "available", return_value=True):
            with unittest.mock.patch.object(subprocess, "run",
                                            return_value=completed(json.dumps(REGISTRY))):
                rows = clarp.sessions()
        self.assertEqual([r["session"] for r in rows], ["adam", "bella", "cleo"])

    def test_non_json_output_is_an_error_not_a_crash(self):
        with unittest.mock.patch.object(clarp, "available", return_value=True):
            with unittest.mock.patch.object(subprocess, "run",
                                            return_value=completed("not json")):
                with self.assertRaises(clarp.ClarpError):
                    clarp.sessions()

    def test_failure_exit_is_reported(self):
        with unittest.mock.patch.object(clarp, "available", return_value=True):
            with unittest.mock.patch.object(
                    subprocess, "run", return_value=completed("", 1, "database locked")):
                with self.assertRaises(clarp.ClarpError) as ctx:
                    clarp.sessions()
        self.assertIn("database locked", str(ctx.exception))


class OverviewTest(unittest.TestCase):
    def overview(self, live=None, default="work"):
        with unittest.mock.patch.object(clarp, "sessions", return_value=REGISTRY):
            with unittest.mock.patch.object(clarp, "live_agents", return_value=live or {}):
                return clarp.overview(default_alias=default)

    def test_counts_by_backend(self):
        out = self.overview()
        self.assertEqual(out["by_backend"], {"claude": 2, "codex": 1})
        self.assertEqual(out["claude_backed"], 2)

    def test_idle_agent_is_not_credited_with_spending_an_account(self):
        """An idle agent spends nothing; saying otherwise would misattribute quota."""
        agent = next(a for a in self.overview()["agents"] if a["session"] == "adam")
        self.assertFalse(agent["live"])
        self.assertIsNone(agent["account"])
        self.assertEqual(agent["would_use"], "work")

    def test_live_pinned_agent_reports_its_own_account(self):
        live = {"adam": {"pid": 42, "config_dir": "/home/u/.claude-accounts/other",
                         "account": "other"}}
        agent = next(a for a in self.overview(live)["agents"] if a["session"] == "adam")
        self.assertTrue(agent["live"])
        self.assertEqual(agent["account"], "other")
        self.assertTrue(agent["pinned"])

    def test_live_unpinned_agent_falls_to_the_default_account(self):
        live = {"adam": {"pid": 42, "config_dir": None, "account": None}}
        out = self.overview(live)
        self.assertEqual(out["live_by_account"], {"work": 1})

    def test_codex_agents_are_never_attributed_to_a_claude_account(self):
        agent = next(a for a in self.overview()["agents"] if a["session"] == "cleo")
        self.assertIsNone(agent["account"])
        self.assertIsNone(agent["would_use"])

    def test_live_agents_sort_first(self):
        live = {"cleo": {"pid": 1, "config_dir": None, "account": None}}
        self.assertEqual(self.overview(live)["agents"][0]["session"], "cleo")

    def test_live_count(self):
        live = {"adam": {"pid": 1, "config_dir": None, "account": None}}
        self.assertEqual(self.overview(live)["live"], 1)


class LiveAgentTest(unittest.TestCase):
    def test_a_shell_that_merely_mentions_an_agent_is_not_one(self):
        listing = ("  111 /bin/bash -c echo session id for this agent is `fake`\n"
                   "  222 /usr/bin/claude --append-system-prompt "
                   "the app session id for this agent is `real` --resume x\n")
        with unittest.mock.patch.object(subprocess, "run",
                                        return_value=completed(listing)):
            with unittest.mock.patch.object(clarp, "_config_dir_of", return_value=None):
                found = clarp.live_agents()
        self.assertEqual(list(found), ["real"])

    def test_an_unreadable_process_list_yields_nothing_rather_than_raising(self):
        with unittest.mock.patch.object(subprocess, "run", side_effect=OSError("nope")):
            self.assertEqual(clarp.live_agents(), {})


if __name__ == "__main__":
    unittest.main()
