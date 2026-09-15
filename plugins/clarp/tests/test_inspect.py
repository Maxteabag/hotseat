"""Recovering what a stopped piece of work was doing.

Uses fixture databases and transcripts; never the real ones.
"""

import json
import shutil
import sqlite3
import tempfile
import unittest
import unittest.mock
from pathlib import Path

from hotseat_clarp import inspect as inspector
from hotseat import textutil

PREAMBLE = ("This session is being continued from a previous conversation that ran "
            "out of context.\n\nSummary: 1. Primary Request and Intent: migrate the "
            "warehouse and verify the row counts.")


class CleaningTest(unittest.TestCase):
    def test_speech_markup_is_stripped(self):
        text = '<speak><speed ratio="1.1"/>Done and pushed.<break time="300ms"/> Next.</speak>'
        self.assertEqual(textutil.clean(text), "Done and pushed. Next.")

    def test_whitespace_is_collapsed(self):
        self.assertEqual(textutil.clean("a\n\n  b\tc"), "a b c")

    def test_long_text_is_truncated_with_a_marker(self):
        out = textutil.trim("x" * 900, limit=50)
        self.assertEqual(len(out), 51)
        self.assertTrue(out.endswith("…"))

    def test_a_summary_is_mined_out_of_a_continuation_preamble(self):
        """The preamble is not user intent, but it describes the work."""
        summary = inspector._summary_of(PREAMBLE)
        self.assertIn("migrate the warehouse", summary)
        self.assertNotIn("ran out of context", summary)

    def test_ordinary_text_yields_no_summary(self):
        self.assertEqual(inspector._summary_of("just a normal message"), "")

    def test_empty_input_is_safe(self):
        self.assertEqual(textutil.clean(None), "")
        self.assertEqual(inspector._summary_of(None), "")


class ClarpInspectTest(unittest.TestCase):
    def setUp(self):
        self.home = Path(tempfile.mkdtemp(prefix="hotseat-inspect-"))
        self.addCleanup(shutil.rmtree, self.home, ignore_errors=True)
        self.db = self.home / "state.sqlite"
        patcher = unittest.mock.patch.object(inspector, "CLARP_STATE", self.db)
        patcher.start()
        self.addCleanup(patcher.stop)

        self.connection = sqlite3.connect(self.db)
        self.connection.executescript("""
            CREATE TABLE agents (agent_id TEXT, persona TEXT, backend TEXT, cwd TEXT,
                                 session TEXT, model TEXT, deleted_at INT);
            CREATE TABLE messages (agent_id TEXT, seq INT, role TEXT, text TEXT,
                                   timestamp TEXT);
            CREATE TABLE queued_turns (agent_id TEXT, queue_seq INT, status TEXT,
                                       text TEXT, origin TEXT, enqueued_at INT);
        """)
        self.connection.execute(
            "INSERT INTO agents VALUES ('a1','Domi','claude','/work','domi','fable',NULL)")
        self.connection.commit()

    def message(self, seq, role, text):
        self.connection.execute("INSERT INTO messages VALUES ('a1',?,?,?,?)",
                                (seq, role, text, "2026-09-14T10:00:00Z"))
        self.connection.commit()

    def test_identity_and_location_are_reported(self):
        self.message(1, "user", "do the thing")
        detail = inspector.detail("domi")
        self.assertEqual(detail["name"], "Domi")
        self.assertEqual(detail["cwd"], "/work")
        self.assertEqual(detail["model"], "fable")

    def test_the_last_real_prompt_is_found(self):
        self.message(1, "user", "first ask")
        self.message(2, "assistant", "working")
        self.message(3, "user", "second ask")
        self.assertEqual(inspector.detail("domi")["last_user"], "second ask")

    def test_a_preamble_does_not_masquerade_as_the_users_request(self):
        """It is machine-generated; reporting it as what they asked would mislead."""
        self.message(1, "user", "the real ask")
        self.message(2, "user", PREAMBLE)
        detail = inspector.detail("domi")
        self.assertEqual(detail["last_user"], "the real ask")
        self.assertIn("migrate the warehouse", detail["summary"])

    def test_queued_work_is_surfaced(self):
        self.message(1, "user", "go")
        self.connection.execute(
            "INSERT INTO queued_turns VALUES ('a1',1,'queued','continue please','automation',0)")
        self.connection.commit()
        queued = inspector.detail("domi")["queued"]
        self.assertEqual(len(queued), 1)
        self.assertEqual(queued[0]["origin"], "automation")

    def test_started_turns_are_not_reported_as_waiting(self):
        self.connection.execute(
            "INSERT INTO queued_turns VALUES ('a1',1,'started','ran already','user',0)")
        self.connection.commit()
        self.assertEqual(inspector.detail("domi")["queued"], [])

    def test_an_unknown_name_is_an_error(self):
        with self.assertRaises(inspector.InspectError):
            inspector.detail("nobody")


class NativeInspectTest(unittest.TestCase):
    def setUp(self):
        self.home = Path(tempfile.mkdtemp(prefix="hotseat-native-inspect-"))
        self.addCleanup(shutil.rmtree, self.home, ignore_errors=True)
        self.projects = self.home / "projects"
        (self.projects / "-work").mkdir(parents=True)
        for module, name in ((inspector, "PROJECTS_DIR"),):
            patcher = unittest.mock.patch.object(module, name, self.projects)
            patcher.start()
            self.addCleanup(patcher.stop)
        # Clarp must not answer for a native id.
        missing = unittest.mock.patch.object(inspector, "CLARP_STATE",
                                             self.home / "absent.sqlite")
        missing.start()
        self.addCleanup(missing.stop)

    def transcript(self, name, entries):
        path = self.projects / "-work" / f"{name}.jsonl"
        path.write_text("\n".join(json.dumps(e) for e in entries))
        return path

    @staticmethod
    def entry(kind, text, **extra):
        return {"type": kind, "message": {"content": [{"type": "text", "text": text}]},
                **extra}

    def test_prompt_reply_and_location_are_recovered(self):
        self.transcript("abc123", [
            self.entry("user", "build the report", cwd="/work", gitBranch="main"),
            self.entry("assistant", "starting now", cwd="/work"),
        ])
        detail = inspector.detail("abc123")
        self.assertEqual(detail["last_user"], "build the report")
        self.assertEqual(detail["last_assistant"], "starting now")
        self.assertEqual(detail["cwd"], "/work")
        self.assertEqual(detail["branch"], "main")

    def test_the_error_that_stopped_it_is_not_quoted_as_its_last_words(self):
        self.transcript("abc123", [
            self.entry("user", "go"),
            self.entry("assistant", "on it"),
            {"type": "assistant", "isApiErrorMessage": True,
             "message": {"content": [{"type": "text", "text": "You've hit your session limit"}]}},
        ])
        self.assertEqual(inspector.detail("abc123")["last_assistant"], "on it")

    def test_a_short_id_resolves(self):
        self.transcript("abc123def", [self.entry("user", "hello")])
        self.assertEqual(inspector.detail("abc123")["last_user"], "hello")

    def test_damaged_lines_are_skipped(self):
        path = self.transcript("abc123", [self.entry("user", "hello")])
        path.write_text("{broken\n" + path.read_text())
        self.assertEqual(inspector.detail("abc123")["last_user"], "hello")


if __name__ == "__main__":
    unittest.main()
