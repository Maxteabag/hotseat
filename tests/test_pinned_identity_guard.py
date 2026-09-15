import json,tempfile
from pathlib import Path
from types import SimpleNamespace
from unittest import TestCase
from hotseat.sessiondirs import ensure
class PinnedIdentityGuardTests(TestCase):
    def test_wrong_identity_is_rejected_before_any_overwrite(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);target=root/'work';target.mkdir()
            old_config=json.dumps({'oauthAccount':{'emailAddress':'wrong@example.com','organizationUuid':'wrong'}})
            old_creds=json.dumps({'claudeAiOauth':{'accessToken':'dummy','expiresAt':9999999999999}})
            (target/'.claude.json').write_text(old_config);(target/'.credentials.json').write_text(old_creds)
            with self.assertRaises(ValueError):ensure(SimpleNamespace(alias='work'),{'claudeAiOauth':{'accessToken':'new','expiresAt':2}},identity={'emailAddress':'right@example.com','organizationUuid':'right'},shared=root/'shared',root=root)
            self.assertEqual((target/'.claude.json').read_text(),old_config)
            self.assertEqual((target/'.credentials.json').read_text(),old_creds)
