import contextlib
import io
import os
from types import SimpleNamespace
from unittest import TestCase
from unittest.mock import patch
from hotseat import cli,plugins,inspect

class PluginTests(TestCase):
    def setUp(self):
        plugins.installed.cache_clear();self.addCleanup(plugins.installed.cache_clear)
        p=patch.dict(os.environ,{'HOTSEAT_PLUGINS':''});p.start();self.addCleanup(p.stop)
    def test_core_has_no_clarp_command_without_plugin(self):
        with patch.object(plugins,'entry_points',return_value=[]):
            parser=cli.build_parser()
            self.assertNotIn('clarp',parser.format_help().lower())
            self.assertEqual(plugins.snapshot([]),{})
            self.assertEqual(plugins.dashboard_scripts(),'')
    def test_explicit_disable_does_not_load_installed_code(self):
        loaded=[]
        ep=SimpleNamespace(name='example',load=lambda:loaded.append(True))
        with patch.dict(os.environ,{'HOTSEAT_PLUGINS':'none'}),patch.object(plugins,'entry_points',return_value=[ep]):
            self.assertEqual(plugins.installed(),())
        self.assertEqual(loaded,[])
    def test_plugin_commands_and_snapshots_are_discovered(self):
        class Example:
            api_version=1
            def register_commands(self,add,nodes):add('example',lambda args:0,'Example plugin')
            def snapshot(self,accounts):return {'count':len(accounts)}
        ep=SimpleNamespace(name='example',load=lambda:Example)
        with patch.object(plugins,'entry_points',return_value=[ep]):
            self.assertEqual(cli.main(['example']),0)
            self.assertEqual(plugins.snapshot([{}]),{'example':{'count':1}})
    def test_incompatible_plugin_is_reported_without_breaking_core(self):
        ep=SimpleNamespace(name='bad',load=lambda:lambda:SimpleNamespace(api_version=999))
        with patch.object(plugins,'entry_points',return_value=[ep]),self.assertWarnsRegex(RuntimeWarning,'incompatible'):
            self.assertEqual(plugins.installed(),())
    def test_plugin_snapshot_failure_is_namespaced(self):
        class Broken:
            def snapshot(self,accounts):raise ValueError('offline')
        with patch.object(plugins,'installed',return_value=(('broken',Broken()),)):
            self.assertEqual(plugins.snapshot([]),{'broken':{'error':'offline'}})
    def test_native_inspection_survives_without_plugins(self):
        with patch.object(plugins,'installed',return_value=()),patch.object(inspect,'_native_detail',return_value={'kind':'native'}):
            self.assertEqual(inspect.detail('example'),{'kind':'native'})
