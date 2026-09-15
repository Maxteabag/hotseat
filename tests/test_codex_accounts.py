import argparse
import base64
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from hotseat import codex_accounts as accounts


def credential(email, account):
    claims = base64.urlsafe_b64encode(json.dumps({'email': email}).encode()).decode()
    return json.dumps({'auth_mode': 'chatgpt', 'tokens': {
        'id_token': f'x.{claims}.x', 'refresh_token': 'test-only', 'account_id': account}})


class AccountTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.home = Path(self.directory.name)
        for name, value in {
            'AUTH_FILE': self.home / 'auth.json',
            'PROFILES_DIR': self.home / 'profiles',
            'CURRENT_PROFILE_FILE': self.home / 'profiles/.current_profile',
            'SWITCHED_AT_FILE': self.home / 'profiles/.switched_at',
        }.items():
            patcher = patch.object(accounts, name, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        accounts.AUTH_FILE.write_text(credential('live@example.com', 'live'))
        self.original = accounts.AUTH_FILE.read_bytes()

    def login(self, email='new@example.com', rc=0, force=False):
        def fake_login(command, env):
            self.assertEqual(command[-2:], ['login', '--device-auth'])
            self.assertNotEqual(Path(env['CODEX_HOME']), self.home)
            (Path(env['CODEX_HOME']) / 'auth.json').write_text(credential(email, 'new'))
            return rc
        with patch.object(accounts, '_status', side_effect=[argparse.Namespace(returncode=1), argparse.Namespace(returncode=0)]), patch.object(accounts.subprocess, 'call', side_effect=fake_login):
            accounts.cmd_login(argparse.Namespace(name='new', email='new@example.com', force=force))

    def test_login_saves_without_switching(self):
        self.login()
        self.assertEqual(accounts.AUTH_FILE.read_bytes(), self.original)
        self.assertFalse(accounts.CURRENT_PROFILE_FILE.exists())
        auth = accounts.PROFILES_DIR / 'new/auth.json'
        self.assertEqual(auth.stat().st_mode & 0o777, 0o600)
        self.assertEqual(accounts.describe(auth)['email'], 'new@example.com')

    def test_mismatch_and_failure_do_not_save(self):
        for email, rc in [('wrong@example.com', 0), ('new@example.com', 1)]:
            with self.assertRaises(SystemExit):
                self.login(email, rc)
            self.assertFalse((accounts.PROFILES_DIR / 'new/auth.json').exists())
            self.assertEqual(accounts.AUTH_FILE.read_bytes(), self.original)

    def test_existing_profile_preserved(self):
        target = accounts.PROFILES_DIR / 'new/auth.json'
        accounts._atomic_copy(accounts.AUTH_FILE, target)
        with self.assertRaises(SystemExit), patch.object(accounts.subprocess, 'call') as launch:
            self.login()
        launch.assert_not_called()
        self.assertEqual(target.read_bytes(), self.original)

        # But with force=True, it allows re-login and overwriting
        self.login(force=True)
        self.assertEqual(accounts.describe(target)['email'], 'new@example.com')

    def test_failed_isolation_preflight_never_starts_login(self):
        with patch.object(accounts, '_status', return_value=argparse.Namespace(returncode=0)), patch.object(accounts.subprocess, 'call') as launch:
            with self.assertRaises(SystemExit):
                accounts.cmd_login(argparse.Namespace(name='new', email='new@example.com'))
            launch.assert_not_called()
        self.assertEqual(accounts.AUTH_FILE.read_bytes(), self.original)

    def test_reject_path_traversal(self):
        for name in ['../personal', '/tmp/account', '.', '']:
            with self.assertRaises(SystemExit):
                accounts._profile_dir(name)

    def test_switch_roundtrip_preserves_refreshed_credentials(self):
        accounts._snapshot('live')
        accounts.CURRENT_PROFILE_FILE.write_text('live')
        other = accounts.PROFILES_DIR / 'new/auth.json'
        other.parent.mkdir()
        other.write_text(credential('new@example.com', 'new'))
        accounts.cmd_switch(argparse.Namespace(name='new'))
        accounts.cmd_switch(argparse.Namespace(name='live'))
        self.assertEqual(accounts.AUTH_FILE.read_bytes(), self.original)

    def test_stale_marker_cannot_overwrite_wrong_profile(self):
        accounts._snapshot('wrong')
        accounts.CURRENT_PROFILE_FILE.write_text('wrong')
        accounts.AUTH_FILE.write_text(credential('different@example.com', 'different'))
        with self.assertRaises(SystemExit):
            accounts.cmd_switch(argparse.Namespace(name='wrong'))
        self.assertEqual((accounts.PROFILES_DIR / 'wrong/auth.json').read_bytes(), self.original)

    def test_same_workspace_different_user_cannot_overwrite_profile(self):
        accounts._snapshot('first')
        accounts.CURRENT_PROFILE_FILE.write_text('first')
        accounts.AUTH_FILE.write_text(credential('second@example.com', 'live'))
        live = accounts.AUTH_FILE.read_bytes()
        with self.assertRaises(SystemExit):
            accounts.cmd_switch(argparse.Namespace(name='first'))
        self.assertEqual((accounts.PROFILES_DIR / 'first/auth.json').read_bytes(), self.original)
        self.assertEqual(accounts.AUTH_FILE.read_bytes(), live)

    def test_fetch_reset_credits_missing_file_or_token(self):
        self.assertEqual(accounts.fetch_reset_credits(Path('/nonexistent/auth.json')),
                         {'available_count': 0, 'credits': []})
        bad_auth = self.home / 'bad_auth.json'
        bad_auth.write_text('{}')
        self.assertEqual(accounts.fetch_reset_credits(bad_auth),
                         {'available_count': 0, 'credits': []})

    def test_unsaved_live_login_is_not_overwritten_on_switch(self):
        target=self.home/'profiles'/'target';target.mkdir(parents=True)
        (target/'auth.json').write_text(credential('target@example.com','target'))
        with self.assertRaises(SystemExit):accounts.cmd_switch(argparse.Namespace(name='target'))
        self.assertEqual(accounts.AUTH_FILE.read_bytes(),self.original)


if __name__ == '__main__':
    unittest.main()
