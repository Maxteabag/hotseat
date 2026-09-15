import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import urllib.error
from hotseat import resetcredits, cli

class ResetCreditTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory();self.addCleanup(self.tmp.cleanup)
        self.path=Path(self.tmp.name)/'auth.json'
        self.path.write_text(json.dumps({'tokens':{'access_token':'SENSITIVE','account_id':'workspace'}}))
    def test_get_balance_never_redeems_or_exports_tokens(self):
        with patch.object(resetcredits.urllib.request,'urlopen',return_value=io.StringIO('{"available_count":2}')) as request:
            row=resetcredits.read_balance(self.path)
        self.assertEqual(row['available_count'],2)
        self.assertEqual(request.call_args.args[0].get_method(),'GET')
        self.assertIsNone(request.call_args.args[0].data)
        self.assertNotIn('SENSITIVE',json.dumps(row))
    def test_zero_is_a_successful_balance(self):
        with patch.object(resetcredits.urllib.request,'urlopen',return_value=io.StringIO('{"available_count":0}')):
            row=resetcredits.read_balance(self.path)
        self.assertEqual(row['available_count'],0);self.assertIsNone(row['error'])
    def test_http_failure_is_unknown_not_zero(self):
        with patch.object(resetcredits.urllib.request,'urlopen',side_effect=urllib.error.HTTPError('url',401,'unauthorized',{},None)):
            row=resetcredits.read_balance(self.path)
        self.assertIsNone(row['available_count']);self.assertEqual(row['error'],'HTTP 401')
    def test_bad_payloads_are_unknown(self):
        for value in ['{}','[]','{"available_count":true}','{"available_count":-1}','{"available_count":"2"}']:
            with self.subTest(value=value),patch.object(resetcredits.urllib.request,'urlopen',return_value=io.StringIO(value)):
                row=resetcredits.read_balance(self.path)
            self.assertIsNone(row['available_count']);self.assertTrue(row['error'])
    def test_unknown_alias_never_queries_endpoint(self):
        with patch.object(resetcredits.codex,'accounts',return_value=[]),patch.object(resetcredits,'read_balance') as read:
            with self.assertRaises(ValueError):resetcredits.balances(['../other'])
        read.assert_not_called()
    def test_cli_json_partial_failure(self):
        rows=[{'alias':'work','available_count':None,'error':'HTTP 401'}]
        with patch.object(resetcredits,'balances',return_value=rows) as balances,contextlib.redirect_stdout(io.StringIO()) as out:
            code=cli.main(['resets','work','--json'])
        self.assertEqual(code,1);balances.assert_called_once_with(['work'])
        self.assertEqual(json.loads(out.getvalue())['accounts'],rows)
