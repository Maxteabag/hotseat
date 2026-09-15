"""Finding and continuing work that a usage limit stopped.

No test sends a real prompt or reads the real Clarp database.
"""

import json
import sqlite3
import shutil
import subprocess
import tempfile
import time
import unittest
import unittest.mock
from pathlib import Path

from hotseat_clarp import resume

NOW = time.time()


def account(alias="work", limited=False, blocked=(), usage=True):
    return {"alias": alias, "is_active": True,
            "usage": ({"limited": limited, "blocked_models": list(blocked)}
                      if usage else None)}


class ReadinessTest(unittest.TestCase):
    def item(self, backend="claude"):
        return {"kind": "clarp", "id": "a", "name": "A", "backend": backend}

    def test_a_healthy_account_is_ready(self):
        state = resume.readiness(self.item(), [account()], "work")
        self.assertTrue(state["ready"])
        self.assertIsNone(state["hard"])
        self.assertIsNone(state["warning"])

    def test_a_rate_limited_account_is_a_hard_block(self):
        """Continuing here would reproduce the failure that caused the stop."""
        state = resume.readiness(self.item(), [account(limited=True)], "work")
        self.assertFalse(state["ready"])
        self.assertIn("rate limited", state["hard"])

    def test_a_spent_model_limit_warns_but_does_not_block(self):
        """The resumed work may not use that model at all."""
        state = resume.readiness(self.item(), [account(blocked=["Fable"])], "work")
        self.assertTrue(state["ready"])
        self.assertIsNone(state["hard"])
        self.assertIn("Fable", state["warning"])

    def test_unknown_quota_blocks_rather_than_guessing(self):
        state = resume.readiness(self.item(), [account(usage=False)], "work")
        self.assertFalse(state["ready"])

    def test_a_missing_account_blocks(self):
        state = resume.readiness(self.item(), [], "work")
        self.assertFalse(state["ready"])
        self.assertIn("not saved", state["hard"])

    def test_codex_is_allowed_but_says_its_quota_is_invisible(self):
        state = resume.readiness(self.item(backend="codex"),
                                 [account(limited=True)], "work")
        self.assertTrue(state["ready"], "a Claude limit must not gate an OpenAI agent")
        self.assertIn("OpenAI", state["warning"])


class ClarpDetectionTest(unittest.TestCase):
    def setUp(self):
        self.home = Path(tempfile.mkdtemp(prefix="hotseat-resume-"))
        self.addCleanup(shutil.rmtree, self.home, ignore_errors=True)
        self.db = self.home / "state.sqlite"
        patcher = unittest.mock.patch.object(resume, "CLARP_STATE", self.db)
        patcher.start()
        self.addCleanup(patcher.stop)

        connection = sqlite3.connect(self.db)
        connection.executescript("""
            CREATE TABLE agents (agent_id TEXT, persona TEXT, backend TEXT,
                                 cwd TEXT, session TEXT, deleted_at INT,
                                 archived_at INT, model TEXT);
            CREATE TABLE state_log (state_id INTEGER PRIMARY KEY, agent_id TEXT,
                                    ts INT, kind TEXT, detail TEXT);
            CREATE TABLE queue_state_revisions (agent_id TEXT, revision INT, paused INT);
            CREATE TABLE queued_turns (queue_id TEXT, agent_id TEXT, status TEXT,
                                       enqueued_at INT, text TEXT);
            CREATE TABLE messages (agent_id TEXT, seq INT, role TEXT, text TEXT);
        """)
        self.connection = connection
        self.addCleanup(connection.close)

    def paused(self, agent_id, paused=1):
        self.connection.execute(
            "INSERT INTO queue_state_revisions VALUES (?,?,?)", (agent_id, 1, paused))
        self.connection.commit()

    def agent(self, agent_id, session, backend="claude", deleted=None, archived=None):
        self.connection.execute(
            "INSERT INTO agents VALUES (?,?,?,?,?,?,?,?)",
            (agent_id, session.title(), backend, "/tmp", session, deleted, archived, None))
        self.connection.commit()

    def event(self, agent_id, kind, ts, reason=None):
        detail = json.dumps({"reason": reason} if reason else {})
        self.connection.execute(
            "INSERT INTO state_log (agent_id, ts, kind, detail) VALUES (?,?,?,?)",
            (agent_id, int(ts * 1000), kind, detail))
        self.connection.commit()

    # --- tests -----------------------------------------------------------
    def test_pause_overrides_usage_stop_and_prevents_prompt(self):
        self.agent("a1", "adam")
        self.event("a1", "interrupted", NOW - 30, reason="usage_limit")
        self.paused("a1")
        with unittest.mock.patch.object(resume, "_stopped_native_sessions", return_value=[]):
            items = resume.stopped()
        self.assertEqual(len(items), 1)
        self.assertEqual(items[0]["cause"], "queue_paused")
        self.assertFalse(resume.readiness(items[0], [account()], "work")["ready"])
        run = unittest.mock.Mock()
        with self.assertRaises(resume.ResumeError):
            resume.continue_item(items[0], run=run)
        run.assert_not_called()

    def test_an_agent_stopped_by_usage_is_listed(self):
        self.agent("a1", "adam")
        self.event("a1", "interrupted", NOW - 3600, reason="usage_limit")
        found = resume._stopped_clarp_agents(7)
        self.assertEqual([f["id"] for f in found], ["adam"])

    def test_an_agent_that_ran_since_is_not_listed(self):
        """It recovered on its own; continuing it would duplicate work."""
        self.agent("a1", "adam")
        self.event("a1", "interrupted", NOW - 7200, reason="usage_limit")
        self.event("a1", "done", NOW - 60)
        self.assertEqual(resume._stopped_clarp_agents(7), [])

    def test_a_stop_for_another_reason_is_not_listed(self):
        self.agent("a1", "adam")
        self.event("a1", "interrupted", NOW - 600, reason="user_stop")
        self.assertEqual(resume._stopped_clarp_agents(7), [])

    def test_deleted_and_archived_agents_are_skipped(self):
        self.agent("a1", "gone", deleted=1)
        self.agent("a2", "shelved", archived=1)
        for agent_id in ("a1", "a2"):
            self.event(agent_id, "interrupted", NOW - 600, reason="usage_limit")
        self.assertEqual(resume._stopped_clarp_agents(7), [])

    def test_the_window_excludes_old_stops(self):
        self.agent("a1", "adam")
        self.event("a1", "interrupted", NOW - 30 * 86400, reason="usage_limit")
        self.assertEqual(resume._stopped_clarp_agents(7), [])
        self.assertEqual(len(resume._stopped_clarp_agents(60)), 1)

    def test_the_last_thing_the_agent_said_is_carried(self):
        """The listing shows it, so detection has to collect it."""
        self.agent("a1", "adam")
        self.event("a1", "interrupted", NOW - 600, reason="usage_limit")
        self.connection.execute(
            "INSERT INTO messages VALUES ('a1', 1, 'assistant', '<speak>Pushed the fix.</speak>')")
        self.connection.commit()
        found = resume._stopped_clarp_agents(7)
        self.assertEqual(found[0]["last_message"], "Pushed the fix.",
                         "speech markup must not reach the listing")

    def test_a_missing_database_is_not_an_error(self):
        self.db.unlink()
        self.assertEqual(resume._stopped_clarp_agents(7), [])

    def test_a_parked_agent_is_found_with_its_model(self):
        self.agent("a1", "domi")
        self.connection.execute(
            "INSERT INTO state_log (agent_id, ts, kind, detail) VALUES (?,?,?,?)",
            ("a1", int(NOW * 1000), "thinking",
             json.dumps({"account_recovery": "waiting", "dispatch": "claude"})))
        self.connection.execute("UPDATE agents SET model='claude-fable-5-1'")
        self.connection.commit()
        found = resume._parked_clarp_agents(7)
        self.assertEqual([f["id"] for f in found], ["domi"])
        self.assertEqual(found[0]["model"], "claude-fable-5-1")
        self.assertEqual(found[0]["cause"], "waiting_for_account")

    def test_a_parked_agent_that_has_since_run_is_not_listed(self):
        self.agent("a1", "domi")
        self.connection.execute(
            "INSERT INTO state_log (agent_id, ts, kind, detail) VALUES (?,?,?,?)",
            ("a1", int((NOW - 600) * 1000), "thinking",
             json.dumps({"account_recovery": "waiting"})))
        self.event("a1", "done", NOW)
        self.assertEqual(resume._parked_clarp_agents(7), [])

    def test_a_paused_agent_is_found(self):
        self.agent("a1", "domi")
        self.paused("a1")
        found = resume._paused_clarp_agents()
        self.assertEqual([f["id"] for f in found], ["domi"])
        self.assertEqual(found[0]["cause"], "queue_paused")

    def test_an_unpaused_agent_is_not_found(self):
        self.agent("a1", "domi")
        self.paused("a1", paused=0)
        self.assertEqual(resume._paused_clarp_agents(), [])

    def test_pending_turns_behind_the_pause_are_counted(self):
        self.agent("a1", "domi")
        self.paused("a1")
        self.connection.execute(
            "INSERT INTO queued_turns VALUES ('q1','a1','queued',?, 'work')",
            (int(NOW * 1000),))
        self.connection.commit()
        self.assertEqual(resume._paused_clarp_agents()[0]["pending"], 1)

    def test_a_usage_stop_is_not_duplicated_as_a_paused_row(self):
        """An agent in both states appears once, under the cause found first."""
        self.agent("a1", "domi")
        self.paused("a1")
        self.event("a1", "interrupted", NOW - 600, reason="usage_limit")
        items = [i for i in resume.stopped(7) if i["id"] == "domi"]
        self.assertEqual(len(items), 1)


class NativeDetectionTest(unittest.TestCase):
    def setUp(self):
        self.home = Path(tempfile.mkdtemp(prefix="hotseat-native-"))
        self.addCleanup(shutil.rmtree, self.home, ignore_errors=True)
        self.projects = self.home / "projects"
        (self.projects / "-home-u").mkdir(parents=True)
        patcher = unittest.mock.patch.object(resume, "PROJECTS_DIR", self.projects)
        patcher.start()
        self.addCleanup(patcher.stop)

    def transcript(self, name, entries):
        path = self.projects / "-home-u" / f"{name}.jsonl"
        path.write_text("\n".join(json.dumps(e) for e in entries))
        return path

    @staticmethod
    def entry_user(text):
        return {"type": "user", "message": {"content": [{"type": "text", "text": text}]}}

    @staticmethod
    def limit_error():
        return {"type": "assistant", "isApiErrorMessage": True,
                "message": {"content": [{"type": "text",
                                         "text": "You've hit your session limit · resets 6pm"}]}}

    def test_a_transcript_ending_on_a_limit_is_listed(self):
        self.transcript("s1", [{"type": "user", "message": {"content": "hi"}},
                               self.limit_error()])
        found = resume._stopped_native_sessions(7)
        self.assertEqual([f["id"] for f in found], ["s1"])

    def test_a_limit_that_was_worked_through_is_history(self):
        """An earlier limit followed by real work is not waiting to be continued."""
        self.transcript("s1", [self.limit_error(),
                               {"type": "user", "message": {"content": "retry"}},
                               {"type": "assistant", "message": {"content": "done"}}])
        self.assertEqual(resume._stopped_native_sessions(7), [])

    def test_a_different_api_error_is_not_a_usage_stop(self):
        entry = {"type": "assistant", "isApiErrorMessage": True,
                 "message": {"content": [{"type": "text", "text": "Request timed out"}]}}
        self.transcript("s1", [entry])
        self.assertEqual(resume._stopped_native_sessions(7), [])

    def test_the_working_directory_is_recovered(self):
        self.transcript("s1", [self.limit_error()])
        self.assertEqual(resume._stopped_native_sessions(7)[0]["cwd"], "/home/u")

    def test_the_last_reply_is_carried_for_the_listing(self):
        self.transcript("s1", [
            self.entry_user("go"),
            {"type": "assistant", "message": {"content": [{"type": "text",
                                                           "text": "Pushed the fix."}]}},
            self.limit_error()])
        self.assertEqual(resume._stopped_native_sessions(7)[0]["last_message"],
                         "Pushed the fix.")

    def test_the_stopping_error_is_not_used_as_the_last_reply(self):
        """Quoting the limit message back would say nothing about the work."""
        self.transcript("s1", [self.entry_user("go"), self.limit_error()])
        self.assertEqual(resume._stopped_native_sessions(7)[0]["last_message"], "")

    def test_the_recorded_working_directory_wins_over_the_encoded_one(self):
        self.transcript("s1", [dict(self.limit_error(), cwd="/actual/place")])
        self.assertEqual(resume._stopped_native_sessions(7)[0]["cwd"], "/actual/place")

    def test_damaged_lines_do_not_break_the_scan(self):
        path = self.transcript("s1", [self.limit_error()])
        path.write_text("{not json\n" + path.read_text())
        self.assertEqual(len(resume._stopped_native_sessions(7)), 1)


class PausedQueueTest(unittest.TestCase):
    """A paused queue is a different stuck state from a usage limit.

    While `paused` is set the dispatcher accepts a prompt and queues it, returning
    success. Treating that as "continued" would report work resumed that never ran.
    """

    ITEM = {"kind": "clarp", "id": "domi", "name": "Domi", "backend": "claude",
            "cause": "queue_paused", "pending": 1}

    def test_a_paused_agent_is_never_ready(self):
        state = resume.readiness(self.ITEM, [account()], "work")
        self.assertFalse(state["ready"])
        self.assertIn("paused", state["hard"])

    def test_the_block_names_the_work_stuck_behind_it(self):
        self.assertIn("1 turn", resume.readiness(self.ITEM, [account()], "work")["hard"])

    def test_a_healthy_account_does_not_unblock_a_paused_queue(self):
        """Quota is irrelevant here; the pause is what stops it."""
        self.assertFalse(resume.readiness(self.ITEM, [account()], "work")["ready"])

    def test_continuing_a_paused_agent_refuses_rather_than_queueing(self):
        calls = []
        with self.assertRaises(resume.ResumeError) as ctx:
            resume.continue_item(self.ITEM, run=lambda *a, **k: calls.append(a))
        self.assertEqual(calls, [], "a prompt here would silently queue")
        self.assertIn("paused", str(ctx.exception))

    def test_the_refusal_says_how_to_clear_it(self):
        with self.assertRaises(resume.ResumeError) as ctx:
            resume.continue_item(self.ITEM)
        self.assertIn("Clarp app", str(ctx.exception))


class StarvationTest(unittest.TestCase):
    """One parked agent wanting an unservable model holds up every other.

    Clarp's account check requires a single account to serve every parked agent's
    model at once. Naming the model that cannot be served is the difference between
    "nothing runs" and knowing which one thing to move.
    """

    HEALTHY = {"alias": "work", "usage": {"limited": False, "blocked_models": []}}
    NO_FABLE = {"alias": "spent", "usage": {"limited": False, "blocked_models": ["Fable"]}}
    LIMITED = {"alias": "out", "usage": {"limited": True, "blocked_models": []}}

    @staticmethod
    def parked(name, model):
        return {"kind": "clarp", "id": name, "name": name, "backend": "claude",
                "cause": "waiting_for_account", "model": model, "pending": 0}

    def test_a_healthy_account_can_serve_anything(self):
        self.assertEqual(resume.servable_by("claude-opus-5", [self.HEALTHY]), ["work"])

    def test_a_rate_limited_account_can_serve_nothing(self):
        self.assertEqual(resume.servable_by("claude-opus-5", [self.LIMITED]), [])

    def test_a_spent_family_limit_rules_out_only_that_family(self):
        """The exact case seen in practice: Opus fine, Fable gone, same account."""
        self.assertEqual(resume.servable_by("claude-opus-5", [self.NO_FABLE]), ["spent"])
        self.assertEqual(resume.servable_by("claude-fable-5-1", [self.NO_FABLE]), [])

    def test_a_dated_or_suffixed_model_id_still_matches_its_family(self):
        self.assertEqual(resume.servable_by("claude-fable-5-1[1m]", [self.NO_FABLE]), [])

    def test_the_unservable_model_is_named_with_who_wants_it(self):
        items = [self.parked("Sindre", "claude-fable-5-1"),
                 self.parked("Domi", "claude-fable-5-1"),
                 self.parked("Ea5a", "claude-opus-5")]
        starving = resume.starving_models(items, [self.NO_FABLE])
        self.assertEqual(starving, {"claude-fable-5-1": ["Sindre", "Domi"]})

    def test_nothing_starves_when_every_model_is_servable(self):
        items = [self.parked("Ea5a", "claude-opus-5")]
        self.assertEqual(resume.starving_models(items, [self.HEALTHY]), {})

    def test_a_parked_agent_whose_model_is_available_is_ready(self):
        state = resume.readiness(self.parked("Ea5a", "claude-opus-5"), [self.NO_FABLE], None)
        self.assertTrue(state["ready"])
        self.assertIn("spent", state["warning"])

    def test_a_parked_agent_with_no_servable_account_is_blocked(self):
        state = resume.readiness(self.parked("Domi", "claude-fable-5-1"), [self.NO_FABLE], None)
        self.assertFalse(state["ready"])
        self.assertIn("claude-fable-5-1", state["hard"])

    def test_only_parked_agents_are_considered(self):
        usage_stopped = {"kind": "clarp", "id": "x", "name": "X", "backend": "claude",
                         "cause": "usage_limit", "model": "claude-fable-5-1"}
        self.assertEqual(resume.starving_models([usage_stopped], [self.NO_FABLE]), {})


class PriorityTest(unittest.TestCase):
    """Ordering decides what a truncated view shows, so it is part of the contract."""

    @staticmethod
    def item(name, cause="usage_limit", pending=0, when=NOW):
        return {"kind": "clarp", "id": name, "name": name, "backend": "claude",
                "cause": cause, "pending": pending, "stopped_at": when}

    def order(self, items):
        return [i["name"] for i in sorted(items, key=resume._priority)]

    def test_blocked_work_outranks_a_more_recent_stop(self):
        """Someone is waiting on queued work; recency does not outrank that."""
        order = self.order([self.item("recent", when=NOW),
                            self.item("blocked", cause="queue_paused",
                                      pending=1, when=NOW - 86400 * 7)])
        self.assertEqual(order[0], "blocked")

    def test_an_empty_paused_queue_sorts_last(self):
        """Leftover state, not stalled work."""
        order = self.order([self.item("leftover", cause="queue_paused", when=NOW),
                            self.item("usage", when=NOW - 86400)])
        self.assertEqual(order[-1], "leftover")

    def test_recency_breaks_ties_within_a_group(self):
        order = self.order([self.item("older", when=NOW - 600),
                            self.item("newer", when=NOW)])
        self.assertEqual(order, ["newer", "older"])

    def test_a_missing_timestamp_does_not_raise(self):
        item = self.item("nostamp", cause="queue_paused")
        item["stopped_at"] = None
        self.assertEqual(self.order([item]), ["nostamp"])


class ContinueTest(unittest.TestCase):
    CLARP_ITEM = {"kind": "clarp", "id": "adam", "name": "Adam", "backend": "claude"}

    def test_a_clarp_agent_is_prompted(self):
        calls = []

        def run(argv, **kwargs):
            calls.append(argv)
            return subprocess.CompletedProcess(argv, 0, stdout="", stderr="")

        result = resume.continue_item(self.CLARP_ITEM, run=run)
        self.assertTrue(result["continued"])
        self.assertIn("prompt", calls[0])
        self.assertIn("adam", calls[0])
        self.assertIn("automation", calls[0],
                      "a resume is not the user typing, and is recorded as automation")

    def test_the_prompt_warns_against_repeating_finished_work(self):
        self.assertIn("do not repeat work that already completed", resume.RESUME_PROMPT)

    def test_a_failed_prompt_is_reported(self):
        def run(argv, **kwargs):
            return subprocess.CompletedProcess(argv, 1, stdout="", stderr="runtime down")

        with self.assertRaises(resume.ResumeError) as ctx:
            resume.continue_item(self.CLARP_ITEM, run=run)
        self.assertIn("runtime down", str(ctx.exception))

    def test_a_native_session_hands_back_a_command_rather_than_acting(self):
        """There is no supervisor to prompt, so nothing is run behind the user."""
        item = {"kind": "native", "id": "uuid-1", "name": "uuid", "backend": "claude",
                "cwd": "/home/u"}
        called = []
        result = resume.continue_item(item, run=lambda *a, **k: called.append(a))
        self.assertFalse(result["continued"])
        self.assertEqual(result["command"], "claude --resume uuid-1")
        self.assertEqual(called, [], "nothing may be launched without being asked")


if __name__ == "__main__":
    unittest.main()
