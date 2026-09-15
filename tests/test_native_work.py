import json,shutil,tempfile,time,unittest
import unittest.mock
from pathlib import Path
from hotseat import resume,inspect as inspector,plugins
NOW=time.time()

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
        missing = unittest.mock.patch.object(plugins, "installed", return_value=())
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
