import tempfile
from pathlib import Path
from unittest import TestCase
from unittest.mock import patch
from hotseat import quota_cache,usage

class CooldownTests(TestCase):
    def test_throttled_request_is_not_repeated_during_cooldown(self):
        with tempfile.TemporaryDirectory() as directory,patch.object(quota_cache,'root',return_value=Path(directory)),patch.object(quota_cache.time,'time',return_value=100),patch.object(usage,'for_token',side_effect=usage.UsageError('usage endpoint returned 429')) as fetch:
            for _ in range(2):
                with self.assertRaises(usage.UsageError):quota_cache.read('secret')
            self.assertEqual(fetch.call_count,1)
            self.assertNotIn('secret',''.join(p.read_text() for p in Path(directory).iterdir()))
            with patch.object(quota_cache.time,'time',return_value=161):
                with self.assertRaises(usage.UsageError):quota_cache.read('secret')
            self.assertEqual(fetch.call_count,2)
    def test_non_throttle_error_does_not_create_cooldown(self):
        with tempfile.TemporaryDirectory() as directory,patch.object(quota_cache,'root',return_value=Path(directory)),patch.object(usage,'for_token',side_effect=usage.UsageError('unauthorized')):
            with self.assertRaises(usage.UsageError):quota_cache.read('secret')
            self.assertEqual(list(Path(directory).iterdir()),[])

    def test_success_is_reused_and_keeps_its_original_timestamp(self):
        with tempfile.TemporaryDirectory() as directory,patch.object(quota_cache,'root',return_value=Path(directory)),patch.object(quota_cache.time,'time',return_value=100),patch.object(usage,'for_token',return_value={'used_7d':.25}) as fetch:
            first=quota_cache.read('secret')
            with patch.object(quota_cache.time,'time',return_value=115):second=quota_cache.read('secret')
            self.assertEqual(fetch.call_count,1)
            self.assertFalse(first['_cached']);self.assertTrue(second['_cached'])
            self.assertEqual(second['_checked_at'],100)

    def test_throttle_preserves_last_good_data_and_honors_retry_after(self):
        with tempfile.TemporaryDirectory() as directory,patch.object(quota_cache,'root',return_value=Path(directory)),patch.object(quota_cache.time,'time',return_value=100),patch.object(usage,'for_token',return_value={'used_7d':.25}) as fetch:
            quota_cache.read('secret')
            fetch.side_effect=usage.UsageError('usage endpoint returned 429',retry_after=120)
            with patch.object(quota_cache.time,'time',return_value=161):row=quota_cache.read('secret')
            self.assertEqual(row['used_7d'],.25);self.assertEqual(row['_checked_at'],100)
            self.assertIn('429',row['_warning'])
            with patch.object(quota_cache.time,'time',return_value=250):quota_cache.read('secret')
            self.assertEqual(fetch.call_count,2)

    def test_backoff_increases_and_does_not_mask_auth_failure(self):
        with tempfile.TemporaryDirectory() as directory,patch.object(quota_cache,'root',return_value=Path(directory)),patch.object(quota_cache.time,'time',return_value=100),patch.object(usage,'for_token',side_effect=usage.UsageError('429')) as fetch:
            with self.assertRaises(usage.UsageError):quota_cache.read('secret')
            with patch.object(quota_cache.time,'time',return_value=161):
                with self.assertRaises(usage.UsageError):quota_cache.read('secret')
            with patch.object(quota_cache.time,'time',return_value=240):
                with self.assertRaises(usage.UsageError):quota_cache.read('secret')
            self.assertEqual(fetch.call_count,2)
            fetch.side_effect=usage.UsageError('unauthorized')
            with patch.object(quota_cache.time,'time',return_value=300):
                with self.assertRaisesRegex(usage.UsageError,'unauthorized'):quota_cache.read('secret')
