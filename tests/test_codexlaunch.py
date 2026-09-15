import json
import os
from pathlib import Path
import subprocess
import tempfile
from unittest import TestCase
from unittest.mock import Mock,patch
from hotseat import codexlaunch,codex,actions

class CodexLaunchTests(TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.shared=Path(self.tmp.name)/'codex';self.profile=self.shared/'profiles'/'work'
        self.profile.mkdir(parents=True)
        (self.profile/'auth.json').write_text('{"test_identity":"work"}')
        (self.shared/'auth.json').write_text('{"test_identity":"global"}')
        (self.shared/'config.toml').write_text('model = "example"')
        for name,value in [('CODEX_HOME',self.shared),('PROFILES_DIR',self.shared/'profiles')]:
            p=patch.object(codex,name,value);p.start();self.addCleanup(p.stop)
        p=patch.object(codex,'accounts',return_value=[{'alias':'work','saved':True}]);p.start();self.addCleanup(p.stop)
    def test_launch_uses_canonical_profile_and_preserves_default(self):
        spawn=Mock()
        with patch.object(codexlaunch.shutil,'which',return_value='/usr/bin/codex'),patch.dict(os.environ,{'OPENAI_API_KEY':'secret-key','CODEX_ACCESS_TOKEN':'secret-token'}):
            result=codexlaunch.launch('work',spawn=spawn,terminal='kitty')
        argv=spawn.call_args.args[0];env=spawn.call_args.kwargs['env']
        self.assertEqual(argv[:3],['kitty','-e','env'])
        self.assertEqual(env['CODEX_HOME'],str(self.profile))
        self.assertIn('CODEX_HOME='+str(self.profile),argv)
        self.assertNotIn('OPENAI_API_KEY',env);self.assertNotIn('CODEX_ACCESS_TOKEN',env)
        self.assertNotIn('secret',str(argv))
        script='import os,json;from pathlib import Path;print(json.loads((Path(os.environ["CODEX_HOME"])/"auth.json").read_text())["test_identity"])'
        child=subprocess.check_output(['python3','-c',script],env=env,text=True)
        self.assertEqual(child.strip(),'work')
        self.assertEqual(json.loads((self.shared/'auth.json').read_text())['test_identity'],'global')
        self.assertTrue((self.profile/'config.toml').is_symlink())
        self.assertEqual(result['config_dir'],str(self.profile))
    def test_existing_profile_config_is_preserved(self):
        (self.profile/'config.toml').write_text('custom')
        codexlaunch.prepare('work')
        self.assertEqual((self.profile/'config.toml').read_text(),'custom')
    def test_unknown_and_traversal_names_are_rejected(self):
        for alias in ['../other','missing']:
            with self.assertRaises(actions.ActionError):codexlaunch.prepare(alias)

    def test_symlinked_auth_cannot_target_global_credentials(self):
        (self.profile/'auth.json').unlink()
        (self.profile/'auth.json').symlink_to(self.shared/'auth.json')
        with self.assertRaises(actions.ActionError):codexlaunch.prepare('work')
