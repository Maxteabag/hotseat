import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
from unittest import TestCase
from unittest.mock import patch
from hotseat import work

class WorkActionTests(TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.cwd=self.tmp.name
        self.item={'id':'fixture-session','provider':'codex','state':'failed','updated':1,'cwd':self.cwd,'can_resume':True,'can_reboot':False,'reason':''}
        self.item['revision']=work.revision(self.item,[])
    def test_resume_keeps_exact_conversation_and_directory(self):
        for provider in ['codex','claude']:
            self.item['provider']=provider
            with patch.object(work,'resolve',return_value=self.item),patch.object(work.codexsessions,'lock_holders',return_value=[]),patch.object(work,'claude_holders',return_value=[]),patch.object(work.actions,'launch_command',return_value={'started':True}) as launch:
                result=work.act(provider,'fixture-session','resume',self.item['revision'],True)
            expected=['codex','resume','fixture-session'] if provider=='codex' else ['claude','--resume','fixture-session']
            launch.assert_called_once_with(expected,cwd=self.cwd);self.assertTrue(result['launched'])
    def test_confirmation_required_before_inspecting_or_mutating(self):
        with patch.object(work,'resolve') as resolve,patch.object(work.actions,'launch_command') as launch:
            with self.assertRaises(work.WorkError):work.act('codex','fixture','reboot','anything')
        resolve.assert_not_called();launch.assert_not_called()
    def test_changed_session_cannot_be_rebooted(self):
        with patch.object(work,'resolve',return_value=self.item),patch.object(work.os,'kill') as kill:
            with self.assertRaisesRegex(work.WorkError,'Session changed'):work.act('codex','fixture','reboot','old-revision',True)
        kill.assert_not_called()
    def test_missing_cwd_cannot_stop_any_process(self):
        self.item.update(cwd='/this/path/does/not/exist',can_reboot=True)
        with patch.object(work,'resolve',return_value=self.item),patch.object(work.os,'kill') as kill:
            with self.assertRaisesRegex(work.WorkError,'directory'):work.act('codex','fixture','reboot',self.item['revision'],True)
        kill.assert_not_called()
    def test_shared_app_server_is_refused(self):
        self.item.update(can_reboot=True)
        owner={'pid':9999999,'start':'1','shared':True,'argv':['codex','app-server']}
        with patch.object(work,'resolve',return_value=self.item),patch.object(work.codexsessions,'lock_holders',return_value=[9999999]),patch.object(work,'process_identity',return_value=owner),patch.object(work.os,'kill') as kill:
            with self.assertRaisesRegex(work.WorkError,'shared'):work.act('codex','fixture','reboot',self.item['revision'],True)
        kill.assert_not_called()
    def test_resume_refuses_a_new_lock_holder(self):
        with patch.object(work,'resolve',return_value=self.item),patch.object(work.codexsessions,'lock_holders',return_value=[123]),patch.object(work.actions,'launch_command') as launch:
            with self.assertRaisesRegex(work.WorkError,'owner'):work.act('codex','fixture','resume',self.item['revision'],True)
        launch.assert_not_called()
    def test_process_reuse_is_refused_before_signal(self):
        owner={'pid':9999999,'start':'1','shared':False,'argv':['codex']}
        item={**self.item,'can_reboot':True}
        item['revision']=work.revision(item,[owner])
        with patch.object(work,'resolve',return_value=item),patch.object(work.codexsessions,'lock_holders',return_value=[9999999]),patch.object(work,'process_identity',side_effect=[owner,{**owner,'start':'2'}]),patch.object(work.os,'kill') as kill:
            with self.assertRaisesRegex(work.WorkError,'identity changed'):work.act('codex','fixture','reboot',item['revision'],True)
        kill.assert_not_called()

    def test_reboot_terminates_only_owned_fixture_and_preserves_id(self):
        if not Path('/proc').exists():self.skipTest('Linux process identity test')
        for provider in ['codex','claude']:
            # Real disposable process with exact CLI-shaped argv, no provider requests.
            process=subprocess.Popen(['bash','-c','exec -a "$1" python3 -c "import time;time.sleep(60)" --resume fixture-session','fixture',provider])
            try:
                deadline=time.monotonic()+3;owner=None
                while time.monotonic()<deadline:
                    owner=work.process_identity(process.pid,provider)
                    if owner:break
                    time.sleep(.01)
                self.assertIsNotNone(owner)
                item={**self.item,'provider':provider,'state':'stuck','can_reboot':True,'can_resume':False}
                item['revision']=work.revision(item,[owner])
                with patch.object(work,'resolve',return_value=item),patch.object(work.codexsessions,'lock_holders',side_effect=lambda _: [process.pid] if process.poll() is None else []),patch.object(work,'claude_holders',return_value=[owner]),patch.object(work.actions,'launch_command',return_value={'started':True}) as launch:
                    result=work.act(provider,'fixture-session','reboot',item['revision'],True)
                self.assertTrue(result['launched']);self.assertIsNotNone(process.poll())
                self.assertEqual(launch.call_args.kwargs['cwd'],self.cwd)
                self.assertEqual(launch.call_args.args[0][-1],'fixture-session')
            finally:
                if process.poll() is None:process.terminate()
                process.wait(timeout=3)

class WorkListingTests(TestCase):
    def test_native_error_is_listed_with_original_cwd(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);project=root/'project';project.mkdir()
            (project/'fixture.jsonl').write_text(json.dumps({'type':'user','cwd':directory,'message':{'content':'Finish the fixture task'}})+'\n'+json.dumps({'type':'assistant','isApiErrorMessage':True,'cwd':directory,'message':{'content':'Usage limit reached'}})+'\n')
            with patch.object(work.inspection,'PROJECTS_DIR',root),patch.object(work.codexsessions,'recent',return_value=[]),patch.object(work,'claude_processes',return_value=[]):result=work.listing()
            self.assertEqual(len(result['items']),1)
            item=result['items'][0];self.assertEqual(item['state'],'failed');self.assertEqual(item['cwd'],directory)
            self.assertTrue(item['can_resume']);self.assertFalse(item['can_reboot'])
