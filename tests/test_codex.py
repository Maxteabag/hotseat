"""Codex account discovery and quota shaping.

Identity comes from JWT claims, so the fixtures build real (unsigned) tokens.
Nothing here runs the live probe or touches the real ~/.codex.
"""

import base64
import json
import shutil
import subprocess
import tempfile
import time
import unittest
import unittest.mock
from pathlib import Path

from hotseat import codex

FUTURE = int(time.time()) + 3600
PAST = int(time.time()) - 3600


def id_token(email, plan="pro", account_id="acc-1", exp=FUTURE):
    claims = {"email": email, "exp": exp,
              "https://api.openai.com/auth": {"chatgpt_plan_type": plan,
                                              "chatgpt_account_id": account_id}}
    body = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
    return f"header.{body}.signature"


class ClaimsTest(unittest.TestCase):
    def test_claims_are_decoded(self):
        out = codex._claims(id_token("user@example.com", plan="pro"))
        self.assertEqual(out["email"], "user@example.com")

    def test_a_malformed_token_is_not_fatal(self):
        for bad in (None, "", "not-a-jwt", "a.b", "a.!!!.c"):
            self.assertEqual(codex._claims(bad), {})


class AccountsTest(unittest.TestCase):
    def setUp(self):
        self.home = Path(tempfile.mkdtemp(prefix="hotseat-codex-"))
        self.addCleanup(shutil.rmtree, self.home, ignore_errors=True)
        self.profiles = self.home / "profiles"
        self.profiles.mkdir()
        for name, value in (("CODEX_HOME", self.home), ("LIVE_AUTH", self.home / "auth.json"),
                            ("PROFILES_DIR", self.profiles)):
            patcher = unittest.mock.patch.object(codex, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)

    def write(self, path, email, account_id="acc-1", exp=FUTURE):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps({"auth_mode": "chatgpt", "tokens": {
            "id_token": id_token(email, account_id=account_id, exp=exp),
            "account_id": account_id}}))

    # --- tests -----------------------------------------------------------
    def test_saved_profiles_are_listed(self):
        self.write(self.profiles / "work" / "auth.json", "work@example.com")
        self.write(self.profiles / "home" / "auth.json", "home@example.com", "acc-2")
        self.assertEqual(codex.saved_profiles(), ["home", "work"])

    def test_a_profile_matching_the_live_credential_is_marked_active(self):
        self.write(self.profiles / "work" / "auth.json", "work@example.com", "acc-1")
        self.write(codex.LIVE_AUTH, "work@example.com", "acc-1")
        found = codex.accounts()
        self.assertEqual(len(found), 1, "the same account must not appear twice")
        self.assertTrue(found[0]["is_active"])
        self.assertTrue(found[0]["saved"])

    def test_an_unsaved_live_account_is_named_rather_than_hidden(self):
        """Losing it to a profile switch is exactly what happened in practice."""
        self.write(self.profiles / "work" / "auth.json", "work@example.com", "acc-1")
        self.write(codex.LIVE_AUTH, "other@example.com", "acc-2")
        found = codex.accounts()
        self.assertEqual(len(found), 2)
        live = found[0]
        self.assertFalse(live["saved"])
        self.assertTrue(live["is_active"])
        self.assertEqual(live["email"], "other@example.com")

    def test_the_live_account_is_not_matched_on_email_alone(self):
        """Two accounts can share an address across different organisations."""
        self.write(self.profiles / "work" / "auth.json", "same@example.com", "acc-1")
        self.write(codex.LIVE_AUTH, "same@example.com", "acc-2")
        found = codex.accounts()
        self.assertEqual(len(found), 2)

    def test_an_expired_stored_token_is_reported_not_hidden(self):
        self.write(self.profiles / "work" / "auth.json", "work@example.com", exp=PAST)
        entry = codex.accounts()[0]
        self.assertLess(entry["token_hours_left"], 0)

    def test_a_damaged_profile_is_skipped_rather_than_crashing(self):
        (self.profiles / "broken").mkdir()
        (self.profiles / "broken" / "auth.json").write_text("{not json")
        self.write(self.profiles / "work" / "auth.json", "work@example.com")
        self.assertEqual([a["alias"] for a in codex.accounts()], ["work"])

    def test_no_installation_is_a_clear_error(self):
        with unittest.mock.patch.object(codex, "available", return_value=False):
            with self.assertRaises(codex.CodexError):
                codex.overview()


class QuotaCacheTest(unittest.TestCase):
    """Probing spawns one `codex app-server` per saved account, one at a time.

    On a refresh timer that is a process per account every cycle, which is far
    too much for a panel. The windows are hourly and weekly, so a slightly stale
    reading is fine and a fresh probe is not.
    """

    def setUp(self):
        codex._LIMITS_CACHE = None
        self.addCleanup(setattr, codex, "_LIMITS_CACHE", None)
        self.calls = []

        def fake_limits(*args, **kwargs):
            self.calls.append(1)
            return {"work": {"usable": True}}

        patcher = unittest.mock.patch.object(codex, "limits", side_effect=fake_limits)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_repeated_background_reads_probe_once(self):
        for _ in range(5):
            codex.cached_limits(max_age=1800)
        self.assertEqual(len(self.calls), 1, "a refresh timer must not re-probe")

    def test_an_expired_entry_is_refreshed(self):
        codex.cached_limits(max_age=1800)
        codex._LIMITS_CACHE = (time.time() - 3600, {"work": {"usable": True}})
        codex.cached_limits(max_age=1800)
        self.assertEqual(len(self.calls), 2)

    def test_a_zero_max_age_always_probes(self):
        """The command line asks for current numbers, not a cached panel."""
        codex.cached_limits(max_age=0)
        codex.cached_limits(max_age=0)
        self.assertEqual(len(self.calls), 2)

    def test_the_age_of_the_returned_reading_is_reported(self):
        _, age = codex.cached_limits(max_age=1800)
        self.assertEqual(age, 0.0)
        codex._LIMITS_CACHE = (time.time() - 300, {"work": {}})
        _, age = codex.cached_limits(max_age=1800)
        self.assertGreater(age, 299)

    def test_a_failed_probe_is_not_cached(self):
        """Caching a failure would hide a recovery for the rest of the window."""
        with unittest.mock.patch.object(codex, "limits", return_value={}):
            found, age = codex.cached_limits(max_age=1800)
        self.assertEqual(found, {})
        self.assertIsNone(age)
        self.assertIsNone(codex._LIMITS_CACHE)

    def test_a_failed_probe_falls_back_to_the_last_good_reading(self):
        codex.cached_limits(max_age=1800)
        codex._LIMITS_CACHE = (time.time() - 3600, {"work": {"usable": True}})
        with unittest.mock.patch.object(codex, "limits", return_value={}):
            found, age = codex.cached_limits(max_age=1800)
        self.assertEqual(found, {"work": {"usable": True}})
        self.assertGreater(age, 3599, "its age must be reported, not hidden")


class LimitsTest(unittest.TestCase):
    ROWS = [{
        "name": "work", "email": "work@example.com", "plan": "pro",
        "usable": False, "blocked": True, "resets_at": 1789417352,
        "soonest_window": "codex/primary weekly",
        "windows": [
            {"limit": "codex", "tier": "primary", "label": "weekly",
             "used_percent": 100, "resets_at": 1789417352},
            {"limit": "codex", "tier": "primary", "label": "5-hour",
             "used_percent": 12, "resets_at": 1789400000}],
    }]

    def run_with(self, stdout="", code=0):
        completed = subprocess.CompletedProcess([], code, stdout=stdout, stderr="")
        with unittest.mock.patch.object(subprocess, "run", return_value=completed):
            with unittest.mock.patch.object(Path, "exists", return_value=True):
                with unittest.mock.patch.object(shutil, "which", return_value="/usr/bin/codex"):
                    return codex.limits()

    def test_the_worst_window_is_what_constrains_the_account(self):
        """An account carries several windows at once; the highest one binds."""
        out = self.run_with(json.dumps(self.ROWS))
        self.assertAlmostEqual(out["work"]["worst_used"], 1.0)
        self.assertEqual(len(out["work"]["windows"]), 2)

    def test_blocked_state_is_carried(self):
        out = self.run_with(json.dumps(self.ROWS))
        self.assertFalse(out["work"]["usable"])
        self.assertTrue(out["work"]["blocked"])

    def test_a_failed_probe_yields_no_quota_rather_than_an_error(self):
        """Quota is an enrichment; losing it must not hide the accounts."""
        self.assertEqual(self.run_with("", code=1), {})

    def test_unparseable_output_is_survivable(self):
        self.assertEqual(self.run_with("not json"), {})

    def test_rows_without_a_name_are_ignored(self):
        self.assertEqual(self.run_with(json.dumps([{"usable": True}])), {})


if __name__ == "__main__":
    unittest.main()
