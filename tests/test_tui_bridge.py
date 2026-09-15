import json
import tempfile
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
from hotseat import tui_bridge as bridge

class TuiBridgeTests(unittest.TestCase):
    def setUp(self):
        cache=tempfile.TemporaryDirectory();self.addCleanup(cache.cleanup)
        p=patch.object(bridge.quota_cache,"root",return_value=Path(cache.name));p.start();self.addCleanup(p.stop)
        self.account = SimpleNamespace(alias="work", email="user@example.com", plan="team", org="Example",
                                       is_active=True, token="SECRET-NEVER-EXPORT")
        self.backend = Mock(can_switch=True)
        self.backend.accounts.return_value = [self.account]
        for name, value in (("for_platform", self.backend), ("running_sessions", 2)):
            p = patch.object(bridge, name, return_value=value); p.start(); self.addCleanup(p.stop)
        p = patch.object(bridge.resetcredits, "balances", return_value=[]); p.start(); self.addCleanup(p.stop)
        p = patch.object(bridge.codex, "overview", return_value={"accounts": []}); self.codex = p.start(); self.addCleanup(p.stop)

    def test_local_snapshot_never_probes_or_exports_tokens(self):
        with patch.object(bridge.usage, "for_token") as probe:
            result = bridge.snapshot()
        probe.assert_not_called()
        self.codex.assert_called_once_with(with_limits=False)
        self.assertNotIn("SECRET", json.dumps(result))
        self.assertEqual(result["accounts"][0]["checked_at"], 0)

    def test_refresh_keeps_unknown_and_model_windows_separate(self):
        limits = {"used_5h": None, "used_7d": 0.4, "limited":False,
                  "scoped":[{"model":"Opus","used":1.1,"reset":123}]}
        with patch.object(bridge.usage, "for_token", return_value=limits): result=bridge.snapshot(True)
        windows=result["accounts"][0]["windows"]
        self.assertEqual([w["used"] for w in windows], [0.4,1.1])
        self.assertTrue(result["accounts"][0]["checked_at"])

    def test_error_is_not_zero_percent(self):
        with patch.object(bridge.usage, "for_token", side_effect=bridge.usage.UsageError("rejected")):
            row=bridge.snapshot(True)["accounts"][0]
        self.assertEqual(row["windows"],[])
        self.assertEqual(row["error"],"rejected")
        self.assertEqual(row["checked_at"],0)

    def test_switch_requires_acknowledgement_and_existing_alias(self):
        with patch.object(bridge.codex,"accounts",return_value=[]), patch.object(bridge.subprocess,"run") as run:
            for ack in (False,True):
                with self.assertRaises(ValueError): bridge.perform("codex","unknown","switch",ack)
            run.assert_not_called()

    def test_unsupported_actions_are_rejected(self):
        for provider,operation in [("other","switch"),("claude","delete")]:
            with self.assertRaises(ValueError): bridge.perform(provider,"work",operation)

    def test_claude_confirmation_delegates_existing_guard(self):
        with patch.object(bridge.actions,"switch",return_value={"ok":True}) as switch:
            bridge.perform("claude","work","switch",False)
        switch.assert_called_once_with(self.backend,"work",2,False)

    def test_codex_reset_balance_reaches_tui(self):
        self.codex.return_value={"accounts":[{"alias":"cx", "saved":True, "usage":{}}]}
        with patch.object(bridge.resetcredits,"balances",return_value=[{"alias":"cx","available_count":2,"error":None}]), patch.object(bridge.usage,"for_token",return_value={}):
            rows=bridge.snapshot(True)["accounts"]
        self.assertEqual(rows[-1]["reset_credits"],2)
        self.assertEqual(rows[-1]["reset_credits_error"],"")

    def test_duplicate_identities_prefer_working_credentials(self):
        rows=[{'provider':'codex','alias':'ai1','email':'one@example.com','workspace':'org','error':'token_revoked','active':True,'checked_at':0},
              {'provider':'codex','alias':'ai1-test','email':'one@example.com','workspace':'org','error':'','active':True,'checked_at':10,'windows':[]},
              {'provider':'codex','alias':'other-workspace','email':'one@example.com','workspace':'other','error':'','active':False,'checked_at':10}]
        grouped=bridge.group_accounts(rows)
        self.assertEqual(len(grouped),2)
        row=next(r for r in grouped if r['workspace']=='org')
        self.assertEqual(row['alias'],'ai1-test')
        self.assertEqual(row['display_alias'],'ai1')
        self.assertEqual(row['aliases'],['ai1','ai1-test'])
        self.assertTrue(row['profile_notes'])

    def test_unknown_identities_never_merge(self):
        rows=[{'provider':'codex','alias':name,'email':'Unknown email','workspace':'','checked_at':0} for name in ['a','b']]
        self.assertEqual(len(bridge.group_accounts(rows)),2)

    def test_expired_access_token_does_not_hit_usage_api(self):
        self.account.access_expires_at=1
        with patch.object(bridge.usage,'for_token') as fetch:
            row=bridge.snapshot(True)['accounts'][0]
        fetch.assert_not_called()
        self.assertIn('Access token expired',row['error'])
