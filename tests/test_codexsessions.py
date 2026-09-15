"""Recent Codex sessions, and the actions that get a stuck one moving.

Fixture databases throughout; no test queues a real message or closes a real
process.
"""

import json
import shutil
import sqlite3
import subprocess
import tempfile
import time
import unittest
import unittest.mock
from pathlib import Path

from hotseat import codexsessions as sessions

NOW = int(time.time())


class TextExtractionTest(unittest.TestCase):
    def test_text_is_decoded_rather_than_pattern_matched(self):
        """Hand-decoding JSON escapes turns apostrophes into mojibake."""
        blob = json.dumps({"type": "userMessage",
                           "content": [{"text": "I’ll use the skill"}]})
        self.assertEqual(sessions._first_text(blob), "I’ll use the skill")

    def test_bookkeeping_is_not_mistaken_for_a_topic(self):
        """Taking the first string finds the item type, which reads as a topic."""
        blob = json.dumps({"type": "userMessage", "role": "user",
                           "content": [{"text": "the real prompt"}]})
        self.assertEqual(sessions._first_text(blob), "the real prompt")

    def test_an_item_with_no_text_yields_nothing(self):
        self.assertEqual(sessions._first_text(json.dumps({"type": "toolCall"})), "")

    def test_damaged_json_is_survivable(self):
        self.assertEqual(sessions._first_text("{not json"), "")
        self.assertEqual(sessions._first_text(None), "")


class RecentTest(unittest.TestCase):
    def setUp(self):
        self.home = Path(tempfile.mkdtemp(prefix="hotseat-codexsess-"))
        self.addCleanup(shutil.rmtree, self.home, ignore_errors=True)
        self.db = self.home / "thread_history_1.sqlite"
        for name, value in (("HISTORY_DB", self.db), ("LOCK_DIR", self.home / "locks")):
            patcher = unittest.mock.patch.object(sessions, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        self.holders = {}
        holders = unittest.mock.patch.object(
            sessions, "lock_holders", side_effect=lambda tid: self.holders.get(tid, []))
        holders.start()
        self.addCleanup(holders.stop)

        self.connection = sqlite3.connect(self.db)
        self.connection.executescript("""
            CREATE TABLE thread_turns (thread_id TEXT, turn_id TEXT, status TEXT,
                                       started_at INT);
            CREATE TABLE thread_items (thread_id TEXT, rollout_ordinal INT,
                                       item_json TEXT);
        """)

    def turn(self, thread, status, at=None):
        self.connection.execute("INSERT INTO thread_turns VALUES (?,?,?,?)",
                                (thread, f"t{status}", status, at or NOW))
        self.connection.commit()

    def item(self, thread, text, ordinal=1):
        self.connection.execute(
            "INSERT INTO thread_items VALUES (?,?,?)",
            (thread, ordinal, json.dumps({"type": "userMessage",
                                          "content": [{"text": text}]})))
        self.connection.commit()

    # --- tests -----------------------------------------------------------
    def test_sessions_are_listed_newest_first(self):
        self.turn("aaa", "completed", NOW - 600)
        self.turn("bbb", "completed", NOW)
        self.assertEqual([e["short"] for e in sessions.recent()], ["bbb", "aaa"])

    def test_the_opening_prompt_is_shown_as_the_topic(self):
        self.turn("aaa", "completed")
        self.item("aaa", "how do i enable device code login?")
        self.assertEqual(sessions.recent()[0]["topic"],
                         "how do i enable device code login?")

    def test_failed_turns_are_counted(self):
        self.turn("aaa", "failed", NOW - 10)
        self.turn("aaa", "failed", NOW - 9)
        self.turn("aaa", "completed", NOW)
        entry = sessions.recent()[0]
        self.assertEqual(entry["failed"], 2)
        self.assertEqual(entry["turns"], 3)

    def test_a_held_session_whose_last_turn_failed_is_stuck(self):
        """The holder keeps failing and nothing else can take the conversation."""
        self.turn("aaa", "failed")
        self.holders["aaa"] = [123]
        self.assertEqual(sessions.recent()[0]["state"], "stuck")

    def test_a_failed_session_nobody_holds_is_merely_failed(self):
        self.turn("aaa", "failed")
        self.assertEqual(sessions.recent()[0]["state"], "failed")

    def test_a_running_held_session_is_working(self):
        self.turn("aaa", "inProgress")
        self.holders["aaa"] = [123]
        self.assertEqual(sessions.recent()[0]["state"], "working")

    def test_a_session_with_no_holder_is_closed(self):
        self.turn("aaa", "completed")
        self.assertEqual(sessions.recent()[0]["state"], "closed")

    def test_a_prefix_resolves_to_one_session(self):
        self.turn("abc123", "completed")
        self.assertEqual(sessions.resolve("abc")["thread_id"], "abc123")

    def test_an_ambiguous_prefix_is_refused(self):
        self.turn("abc111", "completed", NOW)
        self.turn("abc222", "completed", NOW - 5)
        with self.assertRaises(sessions.SessionError) as ctx:
            sessions.resolve("abc")
        self.assertIn("several", str(ctx.exception))

    def test_an_unknown_prefix_is_refused(self):
        with self.assertRaises(sessions.SessionError):
            sessions.resolve("zzz")

    def test_a_missing_history_database_is_a_clear_error(self):
        self.db.unlink()
        with self.assertRaises(sessions.SessionError) as ctx:
            sessions.recent()
        self.assertIn("thread history", str(ctx.exception))


class NudgeTest(unittest.TestCase):
    def test_a_message_goes_through_the_documented_queue(self):
        """No terminal, no typing: codex hands it to the live session itself."""
        calls = []

        def run(argv, **kwargs):
            calls.append(argv)
            return subprocess.CompletedProcess(argv, 0, stdout="Queued", stderr="")

        result = sessions.nudge("abc", "continue", run=run)
        self.assertTrue(result["queued"])
        self.assertIn("queue", calls[0])
        self.assertIn("--thread", calls[0])
        self.assertIn("abc", calls[0])
        self.assertIn("continue", calls[0])

    def test_a_refusal_is_reported(self):
        def run(argv, **kwargs):
            return subprocess.CompletedProcess(argv, 1, stdout="", stderr="no such thread")

        with self.assertRaises(sessions.SessionError) as ctx:
            sessions.nudge("abc", "continue", run=run)
        self.assertIn("no such thread", str(ctx.exception))


class ReleaseTest(unittest.TestCase):
    def test_holders_are_terminated_not_killed(self):
        """Codex releases its lock and flushes history on the way out."""
        signals = []
        with unittest.mock.patch.object(sessions, "lock_holders", return_value=[42]):
            with unittest.mock.patch.object(sessions, "_parent_of", return_value=0):
                sessions.release("abc", signal_process=lambda pid, sig: signals.append((pid, sig)))
        self.assertEqual(signals, [(42, 15)])

    def test_the_launcher_is_closed_rather_than_the_inner_process(self):
        """Signalling the child alone orphans the session that launched it."""
        signals = []
        with unittest.mock.patch.object(sessions, "lock_holders", return_value=[42]):
            with unittest.mock.patch.object(sessions, "_parent_of", return_value=7):
                with unittest.mock.patch.object(sessions, "_looks_like_codex", return_value=True):
                    sessions.release("abc", signal_process=lambda pid, sig: signals.append(pid))
        self.assertEqual(signals, [7])

    def test_nothing_to_close_is_not_an_error(self):
        with unittest.mock.patch.object(sessions, "lock_holders", return_value=[]):
            self.assertEqual(sessions.release("abc"), [])


if __name__ == "__main__":
    unittest.main()
