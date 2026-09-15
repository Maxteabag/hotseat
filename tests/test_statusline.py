"""The in-session account indicator."""

import json
import shutil
import tempfile
import unittest
import unittest.mock
from pathlib import Path

from hotseat import statusline


class StatuslineTest(unittest.TestCase):
    def setUp(self):
        self.home = Path(tempfile.mkdtemp(prefix="hotseat-status-"))
        self.addCleanup(shutil.rmtree, self.home, ignore_errors=True)
        patcher = unittest.mock.patch.object(
            statusline, "accounts_root", return_value=self.home / ".claude-accounts")
        patcher.start()
        self.addCleanup(patcher.stop)
        # The default-identity fallback reads the home folder, so the real one must
        # not be visible to these tests.
        home = unittest.mock.patch.object(Path, "home", staticmethod(lambda: self.home))
        home.start()
        self.addCleanup(home.stop)

    def config(self, path: Path, email=None):
        path.mkdir(parents=True, exist_ok=True)
        payload = {"oauthAccount": {"emailAddress": email}} if email else {}
        (path / ".claude.json").write_text(json.dumps(payload))
        return path

    def test_pinned_session_names_its_account(self):
        d = self.config(self.home / ".claude-accounts" / "work", "user@example.com")
        self.assertEqual(statusline.describe(d), "work · user@example.com")

    def test_shared_config_is_marked_as_the_default(self):
        d = self.config(self.home / ".claude", "user@example.com")
        self.assertEqual(statusline.describe(d), "default · user@example.com")

    def test_pinned_session_without_identity_still_names_the_alias(self):
        d = self.config(self.home / ".claude-accounts" / "work")
        self.assertEqual(statusline.describe(d), "work")

    def test_default_identity_is_found_beside_the_config_directory(self):
        """The shared configuration keeps .claude.json in the home folder."""
        (self.home / ".claude").mkdir(parents=True)
        (self.home / ".claude.json").write_text(json.dumps(
            {"oauthAccount": {"emailAddress": "default@example.com"}}))
        self.assertEqual(statusline.describe(self.home / ".claude"),
                         "default · default@example.com")

    def test_unreadable_config_prints_nothing_rather_than_breaking(self):
        missing = self.home / "nowhere"
        self.assertEqual(statusline.describe(missing), "")
        self.assertEqual(statusline.render(missing), "")

    def test_damaged_config_is_survivable(self):
        d = self.home / ".claude-accounts" / "work"
        d.mkdir(parents=True)
        (d / ".claude.json").write_text("{not json")
        self.assertEqual(statusline.describe(d), "work")

    def test_render_adds_a_marker_only_when_there_is_something_to_show(self):
        d = self.config(self.home / ".claude-accounts" / "work", "user@example.com")
        self.assertTrue(statusline.render(d).endswith("work · user@example.com"))


if __name__ == "__main__":
    unittest.main()
