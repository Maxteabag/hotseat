import json
from types import SimpleNamespace
import unittest
from unittest.mock import patch
from hotseat import codex
class MissingQuotaTests(unittest.TestCase):
    def test_missing_percentage_is_not_zero_usage(self):
        rows=[{'name':'work','windows':[{'limit':'codex','tier':'primary','label':'weekly','used_percent':None},{'limit':'spark','label':'weekly','used_percent':25}]}]
        with patch.object(codex.COMPARE_HELPER.__class__,'exists',return_value=True),patch.object(codex.shutil,'which',return_value='codex'),patch.object(codex.subprocess,'run',return_value=SimpleNamespace(returncode=0,stdout=json.dumps(rows))):
            result=codex.limits()
        self.assertEqual(len(result['work']['windows']),1)
        self.assertEqual(result['work']['windows'][0]['used'],.25)
