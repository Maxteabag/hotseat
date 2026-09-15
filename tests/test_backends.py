"""Account discovery. No real credentials are involved anywhere in these tests."""

import json
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from hotseat.backends import for_platform
from hotseat.backends.darwin import DarwinBackend, keychain_account, keychain_service
from hotseat.backends.linux import LinuxBackend

ORG_A = "00000000-0000-4000-8000-00000000000a"
ORG_B = "00000000-0000-4000-8000-00000000000b"


def oauth(token="tok", plan="team"):
    return {"claudeAiOauth": {"accessToken": token, "subscriptionType": plan,
                              "expiresAt": 1800000000000,
                              "refreshTokenExpiresAt": 1802000000000}}


class LinuxBackendTest(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp(prefix="hotseat-test-"))
        self.addCleanup(shutil.rmtree, self.root, ignore_errors=True)
        (self.root / "profiles").mkdir(parents=True)
        self.backend = LinuxBackend(root=self.root)

    def profile(self, alias, email, org_uuid, org="Example Org", token="tok"):
        d = self.root / "profiles" / alias
        d.mkdir(parents=True, exist_ok=True)
        (d / "credentials.json").write_text(json.dumps(oauth(token)))
        (d / "meta.json").write_text(json.dumps(
            {"alias": alias, "email": email, "org": org, "orgUuid": org_uuid}))
        return d

    def live(self, token="live-tok"):
        (self.root / ".credentials.json").write_text(json.dumps(oauth(token)))

    def test_no_credentials_yields_nothing(self):
        self.assertEqual(self.backend.accounts(), [])

    def test_profiles_are_listed(self):
        self.profile("work", "user@example.com", ORG_A)
        self.profile("personal", "user@example.com", ORG_B)
        aliases = [a.alias for a in self.backend.accounts()]
        self.assertEqual(sorted(aliases), ["personal", "work"])

    def test_same_email_different_org_stays_two_accounts(self):
        """Collapsing these is how one organisation's token overwrites another."""
        self.profile("work", "user@example.com", ORG_A)
        self.profile("personal", "user@example.com", ORG_B)
        self.live()
        with mock.patch.object(LinuxBackend, "_identity", return_value={
                "emailAddress": "user@example.com", "organizationUuid": ORG_A}):
            accounts = self.backend.accounts()
        self.assertEqual(len(accounts), 2)
        active = [a.alias for a in accounts if a.is_active]
        self.assertEqual(active, ["work"], "only the matching organisation is active")

    def test_tracker_breaks_ties_between_duplicate_profiles(self):
        self.profile("work", "user@example.com", ORG_A)
        self.profile("work-copy", "user@example.com", ORG_A)
        (self.root / "profiles" / ".current_profile").write_text("work-copy")
        self.live()
        with mock.patch.object(LinuxBackend, "_identity", return_value={
                "emailAddress": "user@example.com", "organizationUuid": ORG_A}):
            accounts = self.backend.accounts()
        self.assertEqual([a.alias for a in accounts if a.is_active], ["work-copy"])

    def test_signed_in_account_appears_without_any_profiles(self):
        self.live()
        with mock.patch.object(LinuxBackend, "_identity", return_value={
                "emailAddress": "user@example.com", "organizationName": "Example Org"}):
            accounts = self.backend.accounts()
        self.assertEqual(len(accounts), 1)
        self.assertTrue(accounts[0].is_active)

    def test_public_view_never_carries_a_token(self):
        self.profile("work", "user@example.com", ORG_A, token="secret-value")
        published = json.dumps([a.public() for a in self.backend.accounts()])
        self.assertNotIn("secret-value", published)
        self.assertNotIn("token", published)


class DarwinBackendTest(unittest.TestCase):
    def test_default_service_name_matches_the_cli(self):
        with mock.patch.dict("os.environ", {}, clear=True):
            self.assertEqual(keychain_service(), "Claude Code-credentials")

    def test_custom_config_dir_scopes_the_service_name(self):
        with mock.patch.dict("os.environ", {"CLAUDE_CONFIG_DIR": "/tmp/elsewhere"}, clear=True):
            service = keychain_service()
        self.assertTrue(service.startswith("Claude Code-credentials-"))
        self.assertEqual(len(service.rsplit("-", 1)[1]), 8)

    def test_unusual_usernames_fall_back(self):
        with mock.patch.dict("os.environ", {"USER": "bad name/with slash"}, clear=True):
            self.assertEqual(keychain_account(), "claude-code-user")

    def test_backend_declares_itself_read_only(self):
        caps = DarwinBackend().capabilities()
        self.assertFalse(caps["profiles"])
        self.assertFalse(caps["switch"], "writing Keychain items is out of scope")


class SelectionTest(unittest.TestCase):
    def test_platform_selection(self):
        self.assertIsInstance(for_platform("darwin"), DarwinBackend)
        self.assertIsInstance(for_platform("linux"), LinuxBackend)


if __name__ == "__main__":
    unittest.main()
