"""Behavioral regressions from the repository review; synthetic data only."""

from concurrent.futures import Future, ThreadPoolExecutor
from contextlib import redirect_stdout
import io
import json
import os
from pathlib import Path
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

from hotseat import actions, cli, resume, sessiondirs, usage
from hotseat.backends.linux import LinuxBackend
from hotseat.collect import Collector


class CredentialRegressionTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.home = Path(temp.name)
        for patcher in (patch.object(Path, "home", return_value=self.home),
                        patch.dict(os.environ, {}, clear=True)):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.root = self.home / ".claude"
        self.profile = self.root / "profiles" / "work"
        self.profile.mkdir(parents=True)
        self.identity = {"emailAddress": "fixture@example.invalid",
                         "organizationUuid": "org"}
        (self.home / ".claude.json").write_text(json.dumps({
            "oauthAccount": self.identity, "mcpServers": {"fixture": {}}}))
        self.live = {"claudeAiOauth": {"accessToken": "NEW", "refreshToken": "NEW-R",
                                      "expiresAt": 2000}}
        (self.root / ".credentials.json").write_text(json.dumps(self.live))
        (self.profile / "credentials.json").write_text(json.dumps({
            "claudeAiOauth": {"accessToken": "OLD", "refreshToken": "OLD-R",
                              "expiresAt": 1000}}))
        (self.profile / "meta.json").write_text(json.dumps({
            "email": self.identity["emailAddress"], "orgUuid": "org"}))
        (self.profile / "oauthAccount.json").write_text(json.dumps(self.identity))
        self.backend = LinuxBackend(self.root)

    def test_launch_uses_complete_live_credentials_for_active_profile(self):
        target, _ = actions.prepare(self.backend, "work")
        self.assertEqual(json.loads((target / ".credentials.json").read_text()), self.live)
        self.assertEqual(self.backend.accounts()[0].token, "NEW")

    def test_unsaved_live_account_keeps_refresh_token(self):
        (self.profile / "credentials.json").unlink()
        target, _ = actions.prepare(self.backend, "(signed in)")
        self.assertEqual(json.loads((target / ".credentials.json").read_text()), self.live)

    def test_global_settings_carry_over_and_existing_pinned_edits_survive(self):
        target, _ = actions.prepare(self.backend, "work")
        config = target / ".claude.json"
        data = json.loads(config.read_text())
        self.assertEqual(data["mcpServers"], {"fixture": {}})
        data["mcpServers"]["local"] = {}
        config.write_text(json.dumps(data))
        actions.prepare(self.backend, "work")
        self.assertIn("local", json.loads(config.read_text())["mcpServers"])

    def test_custom_config_identity_is_read_from_custom_directory(self):
        custom = self.home / "custom"
        custom.mkdir()
        (custom / ".claude.json").write_text(json.dumps({"oauthAccount": self.identity}))
        self.assertEqual(LinuxBackend(custom)._identity(), self.identity)


class QuotaRegressionTest(unittest.TestCase):
    def test_weekly_exhaustion_and_lock_block_resume(self):
        for weekly in ({"utilization": 100}, {"utilization": 5, "locked_reason": "cap"}):
            with self.subTest(weekly=weekly):
                quota = usage.summarise({"five_hour": {"utilization": 10},
                                         "seven_day": weekly,
                                         "extra_usage": {"is_enabled": False}})
                self.assertFalse(quota["available"])
                state = resume.readiness({"backend": "claude"},
                                         [{"alias": "work", "usage": quota}], "work")
                self.assertFalse(state["ready"])




class ConcurrentSnapshotRegressionTest(unittest.TestCase):
    def test_concurrent_refreshes_share_success_and_failure_and_allow_retry(self):
        for failure in (False, True):
            with self.subTest(failure=failure):
                collector = Collector(backend=object())
                entered, release, joined = threading.Event(), threading.Event(), threading.Event()
                class ObservedFuture(Future):
                    def result(self, timeout=None):
                        joined.set()
                        return super().result(timeout)
                def build():
                    entered.set()
                    if not release.wait(5):
                        raise TimeoutError("test did not release build")
                    if failure:
                        raise ValueError("fixture failure")
                    return {"generated_at": time.time()}
                with patch("hotseat.collect.Future", ObservedFuture), \
                        patch.object(collector, "build", side_effect=build) as mocked, \
                        ThreadPoolExecutor(max_workers=2) as pool:
                    first = pool.submit(collector.snapshot)
                    try:
                        self.assertTrue(entered.wait(5))
                        second = pool.submit(collector.refresh)
                        self.assertTrue(joined.wait(5))
                    finally:
                        release.set()
                    if failure:
                        for future in (first, second):
                            with self.assertRaisesRegex(ValueError, "fixture failure"):
                                future.result(5)
                    else:
                        self.assertIs(first.result(5), second.result(5))
                    self.assertEqual(mocked.call_count, 1)
                with patch.object(collector, "build", return_value={"generated_at": time.time()}) as retry:
                    collector.refresh()
                    retry.assert_called_once()
