import json
import tempfile
from pathlib import Path
from unittest import TestCase
from hotseat.backends.base import Account
from hotseat.backends.linux import LinuxBackend
class SubscriptionTierTests(TestCase):
    def test_max_five_and_twenty_are_distinct(self):
        for tier,label in [('default_claude_max_5x','Max 5×'),('default_claude_max_20x','Max 20×')]:
            a=Account(alias='x',plan='max',rate_limit_tier=tier)
            self.assertEqual(a.plan_label,label)
            self.assertEqual(a.public()['plan_label'],label)
            self.assertNotIn('token',a.public())
    def test_team_seat_keeps_its_plan_name(self):
        a=Account(alias='x',plan='team',rate_limit_tier='default_claude_max_5x')
        self.assertEqual(a.plan_label,'Team 5×')
    def test_unknown_tier_does_not_invent_multiplier(self):
        self.assertEqual(Account(alias='x',plan='max').plan_label,'Max')
    def test_active_metadata_overrides_old_profile_tier(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);profile=root/'profiles'/'work';profile.mkdir(parents=True)
            def creds(tier):return json.dumps({'claudeAiOauth':{'accessToken':'dummy','subscriptionType':'max','rateLimitTier':tier}})
            (profile/'credentials.json').write_text(creds('default_claude_max_5x'))
            (profile/'meta.json').write_text(json.dumps({'email':'test@example.com','orgUuid':'org'}))
            (root/'.credentials.json').write_text(creds('default_claude_max_20x'))
            (root/'.claude.json').write_text(json.dumps({'oauthAccount':{'emailAddress':'test@example.com','organizationUuid':'org'}}))
            a=LinuxBackend(root).accounts()[0]
            self.assertTrue(a.is_active)
            self.assertEqual(a.plan_label,'Max 20×')
