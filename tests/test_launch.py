"""Launching a pinned session must actually look like a different account.

The original implementation passed the token in CLAUDE_CODE_OAUTH_TOKEN. That
authenticates correctly, but the CLI then reports no account identity at all:
`claude auth status` returns email None and org None. Every launched session
looked anonymous and identical, which is indistinguishable from "it ignored my
choice".

Identity is only reported when a session has its own config directory holding a
credentials file. That directory therefore has to be per account and persistent,
because a session refreshes its own token and rotation supersedes the old one.
A throwaway directory would strand the rotated token and break the account.
"""

import json
import os
import shutil
import stat
import subprocess
import tempfile
import unittest
import unittest.mock
from pathlib import Path

from hotseat import actions, sessiondirs
from hotseat.backends.base import Account

FUTURE = 2_000_000_000_000


def credentials(token="ACCOUNT-TOKEN", expires=FUTURE):
    return {"claudeAiOauth": {"accessToken": token, "refreshToken": f"r-{token}",
                              "expiresAt": expires, "refreshTokenExpiresAt": expires,
                              "subscriptionType": "team"}}


class FakeBackend:
    can_switch = True

    def __init__(self, root: Path, token="ACCOUNT-TOKEN"):
        self.profiles_dir = root / "profiles"
        self.root = root
        self.account = Account(alias="work", email="user@example.com",
                               org="Example Org", org_uuid="org-1", token=token)

    def accounts(self):
        return [self.account]


class SessionDirTest(unittest.TestCase):
    def setUp(self):
        self.home = Path(tempfile.mkdtemp(prefix="hotseat-sess-"))
        self.addCleanup(shutil.rmtree, self.home, ignore_errors=True)
        self.shared = self.home / ".claude"
        (self.shared / "skills").mkdir(parents=True)
        (self.shared / "settings.json").write_text('{"model":"x"}')
        (self.shared / "CLAUDE.md").write_text("shared instructions")
        (self.shared / ".claude.json").write_text(json.dumps(
            {"oauthAccount": {"emailAddress": "default@example.com"},
             "mcpServers": {"keep": {}}}))

        self.profiles = self.shared / "profiles" / "work"
        self.profiles.mkdir(parents=True)
        (self.profiles / "credentials.json").write_text(json.dumps(credentials()))
        (self.profiles / "oauthAccount.json").write_text(json.dumps(
            {"emailAddress": "user@example.com", "organizationName": "Example Org"}))

        self.root = self.home / ".claude-accounts"

    def ensure(self, alias="work", token="ACCOUNT-TOKEN", expires=FUTURE):
        account = Account(alias=alias, email="user@example.com", org="Example Org",
                          token=token, access_expires_at=expires)
        return sessiondirs.ensure(account, credentials(token, expires),
                                  identity={"emailAddress": "user@example.com",
                                            "organizationName": "Example Org"},
                                  shared=self.shared, root=self.root)

    # --- the defect ------------------------------------------------------
    def test_session_directory_carries_the_chosen_account_identity(self):
        d = self.ensure()
        identity = json.loads((d / ".claude.json").read_text())["oauthAccount"]
        self.assertEqual(identity["emailAddress"], "user@example.com",
                         "the session would otherwise report the default account")

    def test_session_directory_has_its_own_credentials(self):
        """Identity is only reported when credentials come from a file."""
        d = self.ensure()
        stored = json.loads((d / ".credentials.json").read_text())
        self.assertEqual(stored["claudeAiOauth"]["accessToken"], "ACCOUNT-TOKEN")

    def test_credentials_are_private(self):
        d = self.ensure()
        mode = (d / ".credentials.json").stat().st_mode
        self.assertFalse(mode & (stat.S_IRGRP | stat.S_IROTH),
                         "credentials must not be readable by other users")

    def test_directory_is_stable_across_launches(self):
        """A throwaway directory would strand the refresh token rotation produces."""
        self.assertEqual(self.ensure(), self.ensure())

    def test_a_rotated_token_is_never_overwritten_by_an_older_one(self):
        d = self.ensure(token="NEW", expires=FUTURE + 10_000)
        # The profile store still holds the pre-rotation copy.
        self.ensure(token="OLD", expires=FUTURE)
        stored = json.loads((d / ".credentials.json").read_text())
        self.assertEqual(stored["claudeAiOauth"]["accessToken"], "NEW",
                         "overwriting a rotated token locks the account out")

    def test_shared_assets_are_linked_not_copied(self):
        d = self.ensure()
        for name in ("skills", "settings.json", "CLAUDE.md"):
            self.assertTrue((d / name).is_symlink(),
                            f"{name} must stay in sync with the shared config")

    def test_existing_project_configuration_is_carried_over(self):
        d = self.ensure()
        config = json.loads((d / ".claude.json").read_text())
        self.assertIn("keep", config.get("mcpServers", {}),
                      "a pinned session should start with the same servers configured")

    def test_shared_credentials_are_never_touched(self):
        live = self.shared / ".credentials.json"
        live.write_text(json.dumps(credentials("DEFAULT-ACCOUNT")))
        self.ensure()
        self.assertEqual(json.loads(live.read_text())["claudeAiOauth"]["accessToken"],
                         "DEFAULT-ACCOUNT", "launching must not disturb other sessions")


class TerminalDetectionTest(unittest.TestCase):
    """Picking the first terminal on PATH opens one the user may never use.

    That happened in practice: kitty was installed, ghostty was the real terminal,
    and the window appeared somewhere the user was not looking.
    """

    def setUp(self):
        self.installed = set()
        self.running = ""
        patches = [
            unittest.mock.patch.object(
                actions.shutil, "which",
                side_effect=lambda name: f"/usr/bin/{name}" if name in self.installed else None),
            unittest.mock.patch.object(actions, "_terminal_from_ancestors", return_value=None),
            unittest.mock.patch.object(
                actions.subprocess, "run",
                side_effect=lambda *a, **k: subprocess.CompletedProcess(
                    [], 0, stdout=self.running, stderr="")),
            unittest.mock.patch.dict(actions.os.environ, {}, clear=True),
        ]
        for patcher in patches:
            patcher.start()
            self.addCleanup(patcher.stop)

    def test_an_explicit_preference_wins(self):
        self.installed = {"kitty", "ghostty"}
        actions.os.environ["HOTSEAT_TERMINAL"] = "ghostty"
        self.assertEqual(actions.detect_terminal(), "ghostty")

    def test_the_xdg_default_launcher_is_preferred_over_guessing(self):
        """It opens whichever terminal the user actually set as their default."""
        self.installed = {"kitty", actions.XDG_LAUNCHER}
        self.assertEqual(actions.detect_terminal(), actions.XDG_LAUNCHER)

    def test_a_running_terminal_beats_a_merely_installed_one(self):
        self.installed = {"kitty", "ghostty"}
        self.running = "bash\nghostty\nfirefox\n"
        self.assertEqual(actions.detect_terminal(), "ghostty",
                         "what is running is what the user is looking at")

    def test_falling_back_to_path_only_when_nothing_else_is_known(self):
        self.installed = {"konsole"}
        self.assertEqual(actions.detect_terminal(), "konsole")

    def test_no_terminal_at_all_is_reported(self):
        self.assertIsNone(actions.detect_terminal())

    def test_each_terminal_gets_its_own_command_convention(self):
        self.assertEqual(actions._terminal_flag("ghostty"), "-e")
        self.assertEqual(actions._terminal_flag("kitty"), "-e")
        self.assertEqual(actions._terminal_flag("gnome-terminal"), "--")
        self.assertEqual(actions._terminal_flag("xfce4-terminal"), "-x")
        self.assertEqual(actions._terminal_flag(actions.XDG_LAUNCHER), "--",
                         "the XDG launcher takes the command directly")

    def test_an_absolute_path_still_resolves_its_convention(self):
        self.assertEqual(actions._terminal_flag("/usr/bin/gnome-terminal"), "--")


class LaunchTest(unittest.TestCase):
    def setUp(self):
        self.calls = []
        self.tmp = Path(tempfile.mkdtemp(prefix="hotseat-launch-"))
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.spawn = lambda argv, **kw: self.calls.append({"argv": argv, "kwargs": kw})

    def launch(self, backend=None, **kw):
        backend = backend or FakeBackend(self.tmp)
        with unittest.mock.patch.object(sessiondirs, "ensure",
                                        return_value=self.tmp / "cfg") as ensure:
            result = actions.launch(backend, "work", spawn=self.spawn,
                                    terminal="kitty", **kw)
        self.ensure_calls = ensure.call_args_list
        return result

    def test_session_is_pointed_at_its_own_config_directory(self):
        self.launch()
        env = self.calls[0]["kwargs"]["env"]
        self.assertEqual(env.get("CLAUDE_CONFIG_DIR"), str(self.tmp / "cfg"))

    def test_environment_token_is_not_used(self):
        """It authenticates but hides the account, which is the bug being fixed."""
        self.launch()
        self.assertNotIn("CLAUDE_CODE_OAUTH_TOKEN", self.calls[0]["kwargs"]["env"])

    def test_token_never_reaches_the_command_line(self):
        self.launch()
        self.assertNotIn("ACCOUNT-TOKEN", " ".join(self.calls[0]["argv"]))

    def test_parent_session_variables_are_stripped(self):
        polluted = {"CLAUDECODE": "1", "CLAUDE_CODE_SESSION_ID": "abc",
                    "ANTHROPIC_API_KEY": "sk-nope", "PATH": "/usr/bin"}
        with unittest.mock.patch.dict(os.environ, polluted, clear=True):
            self.launch()
        env = self.calls[0]["kwargs"]["env"]
        for name in ("CLAUDECODE", "CLAUDE_CODE_SESSION_ID", "ANTHROPIC_API_KEY"):
            self.assertNotIn(name, env, f"{name} must not reach the new session")

    def test_account_without_a_token_is_refused(self):
        with self.assertRaises(actions.ActionError):
            self.launch(FakeBackend(self.tmp, token=None))


if __name__ == "__main__":
    unittest.main()
