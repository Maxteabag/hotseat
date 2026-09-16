"""Refreshing an expired profile through the CLI, without touching anything shared."""
import json
import os
import stat
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

from hotseat import refresh
from hotseat.backends.linux import LinuxBackend

NOW = 1_800_000_000.0
HOUR = 3600


def creds(access="old-access", refresh_token="old-refresh", expires=NOW - HOUR):
    return {"claudeAiOauth": {"accessToken": access, "refreshToken": refresh_token,
                              "expiresAt": int(expires * 1000), "subscriptionType": "max",
                              "refreshTokenExpiresAt": int((NOW + 20 * 86400) * 1000)}}


class Base(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.root = self.tmp / "claude"
        self.backend = LinuxBackend(self.root)
        self.profile = self.root / "profiles" / "work"
        self.profile.mkdir(parents=True)
        self.write(self.profile / "credentials.json", creds())
        (self.profile / "oauthAccount.json").write_text(json.dumps({"emailAddress": "dev@example.com"}))
        self.sessions = self.tmp / "sessions"
        self.shared = self.root / ".credentials.json"
        self.write(self.shared, creds("shared-access", "shared-refresh", NOW + 5 * HOUR))
        self.calls = []
        self.env_cache = self.tmp / "cache"
        self._old_env = dict(os.environ)
        os.environ["XDG_CACHE_HOME"] = str(self.env_cache)
        os.environ.pop("HOTSEAT_NO_REFRESH", None)
        os.environ["ANTHROPIC_API_KEY"] = "must-not-leak"

    def tearDown(self):
        os.environ.clear()
        os.environ.update(self._old_env)

    @staticmethod
    def write(path, data):
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(data))

    def stored(self):
        return json.loads((self.profile / "credentials.json").read_text())["claudeAiOauth"]

    def fake_run(self, rotate=True, new_access="new-access", now=NOW):
        """Stand-in for subprocess.run that behaves like the CLI rotating tokens."""
        def run(command, cwd, env, **kwargs):
            self.calls.append({"command": command, "cwd": cwd, "env": env})
            path = Path(env["CLAUDE_CONFIG_DIR"]) / ".credentials.json"
            self.assertTrue(path.is_file())
            seeded = json.loads(path.read_text())
            self.assertLess(seeded["claudeAiOauth"]["expiresAt"] / 1000, now, "copy must be backdated")
            if rotate:
                rotated = creds(new_access, "new-refresh", now + 8 * HOUR)
                path.write_text(json.dumps(rotated))
            return SimpleNamespace(returncode=0, stdout="", stderr="")
        return run


class Refresh(Base):
    def test_fresh_token_is_left_alone(self):
        self.write(self.profile / "credentials.json", creds(expires=NOW + 5 * HOUR))
        out = refresh.refresh(self.backend, "work", run=self.fake_run(), now=NOW, session_root=self.sessions)
        self.assertEqual((out["refreshed"], out["source"]), (False, "fresh"))
        self.assertEqual(self.calls, [])

    def test_within_margin_is_refreshed_preemptively(self):
        self.write(self.profile / "credentials.json", creds(expires=NOW + 10 * 60))
        out = refresh.refresh(self.backend, "work", run=self.fake_run(), now=NOW, session_root=self.sessions)
        self.assertTrue(out["refreshed"])

    def test_expired_token_rotates_through_cli(self):
        out = refresh.refresh(self.backend, "work", run=self.fake_run(), now=NOW, session_root=self.sessions)
        self.assertEqual((out["refreshed"], out["source"]), (True, "cli"))
        self.assertAlmostEqual(out["hours_left"], 8.0, places=1)
        stored = self.stored()
        self.assertEqual((stored["accessToken"], stored["refreshToken"]), ("new-access", "new-refresh"))
        self.assertEqual(stat.S_IMODE((self.profile / "credentials.json").stat().st_mode), 0o600)
        # A dated copy of the old file, and a rescue copy of the rotated tokens.
        names = sorted(p.name.split(".")[0] for p in (self.profile / "backups").iterdir())
        self.assertEqual(names, ["credentials", "rotated"])
        # The shared credential file is exactly as it was.
        self.assertEqual(json.loads(self.shared.read_text())["claudeAiOauth"]["accessToken"], "shared-access")

    def test_cli_runs_isolated_with_clean_environment(self):
        refresh.refresh(self.backend, "work", run=self.fake_run(), now=NOW, session_root=self.sessions)
        call = self.calls[0]
        self.assertNotIn("ANTHROPIC_API_KEY", call["env"])
        self.assertNotEqual(call["env"]["CLAUDE_CONFIG_DIR"], str(self.root))
        self.assertEqual(call["cwd"], call["env"]["CLAUDE_CONFIG_DIR"])
        self.assertIn("--safe-mode", call["command"])
        self.assertIn("--no-session-persistence", call["command"])
        self.assertEqual(call["command"][call["command"].index("--max-budget-usd") + 1], "0.02")
        self.assertFalse(Path(call["cwd"]).exists(), "throwaway config dir is removed")

    def test_cli_declining_to_rotate_is_reported_not_faked(self):
        out = refresh.refresh(self.backend, "work", run=self.fake_run(rotate=False), now=NOW, session_root=self.sessions)
        self.assertEqual((out["refreshed"], out["source"]), (False, "unchanged"))
        self.assertEqual(self.stored()["accessToken"], "old-access")
        self.assertFalse((self.profile / "backups").exists())

    def test_cli_failure_leaves_profile_unchanged(self):
        def broken(command, **kwargs):
            raise OSError("claude: not found")
        with self.assertRaises(refresh.RefreshError):
            refresh.refresh(self.backend, "work", run=broken, now=NOW, session_root=self.sessions)
        self.assertEqual(self.stored()["accessToken"], "old-access")

    def test_pinned_session_directory_supplies_newer_token_without_cli(self):
        self.write(self.sessions / "work" / ".credentials.json", creds("session-access", "session-refresh", NOW + 6 * HOUR))
        out = refresh.refresh(self.backend, "work", run=self.fake_run(), now=NOW, session_root=self.sessions)
        self.assertEqual((out["refreshed"], out["source"]), (True, "session"))
        self.assertEqual(self.stored()["accessToken"], "session-access")
        self.assertEqual(self.calls, [])

    def test_rotation_is_propagated_to_stale_pinned_directory(self):
        pinned = self.sessions / "work" / ".credentials.json"
        self.write(pinned, creds("stale", "stale", NOW - 2 * HOUR))
        refresh.refresh(self.backend, "work", run=self.fake_run(), now=NOW, session_root=self.sessions)
        self.assertEqual(json.loads(pinned.read_text())["claudeAiOauth"]["accessToken"], "new-access")

    def test_unknown_profile_and_signed_in_alias(self):
        with self.assertRaises(refresh.RefreshError):
            refresh.refresh(self.backend, "nobody", run=self.fake_run(), now=NOW)
        with self.assertRaises(refresh.RefreshError):
            refresh.refresh(self.backend, "(signed in)", run=self.fake_run(), now=NOW)

    def test_keychain_backend_is_refused(self):
        with self.assertRaises(refresh.RefreshError):
            refresh.refresh(SimpleNamespace(name="keychain"), "work", run=self.fake_run(), now=NOW)

    def test_real_subprocess_path_with_fake_executable(self):
        fake = self.tmp / "bin" / "claude"   # not self.tmp / "claude": that is the config root
        fake.parent.mkdir()
        fake.write_text("#!" + sys.executable + "\n" + "\n".join([
            "import json, os, sys, time",
            "p = os.path.join(os.environ['CLAUDE_CONFIG_DIR'], '.credentials.json')",
            "assert 'ANTHROPIC_API_KEY' not in os.environ",
            "d = json.load(open(p))",
            "d['claudeAiOauth'].update(accessToken='exec-access', refreshToken='exec-refresh', expiresAt=int((time.time()+8*3600)*1000))",
            "json.dump(d, open(p, 'w'))",
            "print(json.dumps({'result': 'OK'}))",
        ]) + "\n")
        fake.chmod(0o700)
        # Real clock here (the fake CLI stamps time.time()), so force past the freshness check.
        out = refresh.refresh(self.backend, "work", force=True, claude=str(fake), session_root=self.sessions)
        self.assertEqual((out["refreshed"], out["source"]), (True, "cli"))
        self.assertEqual(self.stored()["accessToken"], "exec-access")


class Auto(Base):
    def account(self, **over):
        base = dict(alias="work", token="old-access", access_expires_at=int((NOW - HOUR) * 1000), refresh_expires_at=None)
        base.update(over)
        return SimpleNamespace(**base)

    def test_refreshes_and_updates_account_in_place(self):
        account = self.account()
        token = refresh.auto(self.backend, account, now=NOW, run=self.fake_run(), session_root=self.sessions)
        self.assertEqual(token, "new-access")
        self.assertEqual(account.token, "new-access")
        self.assertGreater(account.access_expires_at / 1000, NOW)

    def test_failure_is_remembered_and_not_retried_within_cooldown(self):
        def broken(command, **kwargs):
            raise OSError("boom")
        with self.assertRaises(refresh.RefreshError):
            refresh.auto(self.backend, self.account(), now=NOW, run=broken, session_root=self.sessions)
        # Second attempt inside the cooldown does not call the CLI at all.
        with self.assertRaises(refresh.RefreshError) as ctx:
            refresh.auto(self.backend, self.account(), now=NOW + 60, run=self.fake_run(), session_root=self.sessions)
        self.assertIn("not retried", str(ctx.exception))
        self.assertEqual(self.calls, [])
        # After the cooldown it is tried again and succeeds, clearing the memo.
        later = NOW + refresh.RETRY_COOLDOWN_S + 1
        token = refresh.auto(self.backend, self.account(), now=later,
                             run=self.fake_run(now=later), session_root=self.sessions)
        self.assertEqual(token, "new-access")
        self.assertEqual(refresh._memo_read(), {})

    def test_disabled_by_environment(self):
        os.environ["HOTSEAT_NO_REFRESH"] = "1"
        with self.assertRaises(refresh.RefreshError):
            refresh.auto(self.backend, self.account(), now=NOW, run=self.fake_run(), session_root=self.sessions)
        self.assertEqual(self.calls, [])

    def test_expired_helper(self):
        self.assertTrue(refresh.expired(self.account(), now=NOW))
        self.assertFalse(refresh.expired(self.account(access_expires_at=int((NOW + HOUR) * 1000)), now=NOW))
        self.assertFalse(refresh.expired(self.account(access_expires_at=None), now=NOW))


if __name__ == "__main__":
    unittest.main()
