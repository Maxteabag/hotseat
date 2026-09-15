"""Parsing the OAuth usage payload.

Fixtures mirror the real shape, including the per-model weekly limit that the
rate-limit headers never expose.
"""

import unittest

from hotseat import usage

PAYLOAD = {
    "five_hour": {"utilization": 36.0, "resets_at": "2026-09-14T16:50:00+00:00",
                  "locked_reason": None},
    "seven_day": {"utilization": 53.0, "resets_at": "2026-09-20T15:00:00+00:00",
                  "locked_reason": None},
    "extra_usage": {"is_enabled": False, "disabled_reason": "out_of_credits"},
    "limits": [
        {"kind": "session", "percent": 36, "severity": "normal",
         "resets_at": "2026-09-14T16:50:00+00:00", "scope": None},
        {"kind": "weekly_all", "percent": 53, "severity": "normal",
         "resets_at": "2026-09-20T15:00:00+00:00", "scope": None},
        {"kind": "weekly_scoped", "percent": 100, "severity": "critical",
         "resets_at": "2026-09-20T15:00:00+00:00",
         "scope": {"model": {"display_name": "Fable", "id": "fable"}}},
    ],
}


class SummariseTest(unittest.TestCase):
    def test_percentages_become_fractions(self):
        out = usage.summarise(PAYLOAD)
        self.assertAlmostEqual(out["used_5h"], 0.36)
        self.assertAlmostEqual(out["used_7d"], 0.53)

    def test_reset_times_parse_to_epochs(self):
        out = usage.summarise(PAYLOAD)
        self.assertIsInstance(out["reset_5h"], int)
        self.assertGreater(out["reset_7d"], out["reset_5h"])

    def test_a_spent_model_limit_is_reported(self):
        """The headline case: plenty of overall quota, nothing left for one model."""
        out = usage.summarise(PAYLOAD)
        self.assertEqual(out["blocked_models"], ["Fable"])
        self.assertTrue(out["available"], "the account itself is still usable")

    def test_scoped_limits_are_worst_first(self):
        payload = {**PAYLOAD, "limits": PAYLOAD["limits"] + [
            {"kind": "weekly_scoped", "percent": 10, "severity": "normal",
             "scope": {"model": {"display_name": "Sonnet"}}}]}
        out = usage.summarise(payload)
        self.assertEqual([s["model"] for s in out["scoped"]], ["Fable", "Sonnet"])

    def test_exhausted_session_marks_the_account_limited(self):
        payload = {**PAYLOAD, "five_hour": {"utilization": 100.0,
                                            "resets_at": None, "locked_reason": None}}
        out = usage.summarise(payload)
        self.assertTrue(out["limited"])
        self.assertEqual(out["status"], "rejected")

    def test_a_locked_window_counts_as_limited_even_below_full(self):
        payload = {**PAYLOAD, "five_hour": {"utilization": 12.0,
                                            "locked_reason": "spend_cap"}}
        self.assertTrue(usage.summarise(payload)["limited"])

    def test_missing_windows_do_not_raise(self):
        out = usage.summarise({})
        self.assertIsNone(out["used_5h"])
        self.assertEqual(out["scoped"], [])
        self.assertFalse(out["limited"])

    def test_malformed_values_are_ignored(self):
        payload = {"five_hour": {"utilization": "lots", "resets_at": "whenever"},
                   "limits": [{"kind": "weekly_scoped", "percent": None,
                               "scope": {"model": {"display_name": "Fable"}}}]}
        out = usage.summarise(payload)
        self.assertIsNone(out["used_5h"])
        self.assertIsNone(out["reset_5h"])
        self.assertEqual(out["scoped"], [])

    def test_scoped_limit_without_a_model_name_is_skipped(self):
        payload = {"limits": [{"kind": "weekly_scoped", "percent": 50, "scope": {}}]}
        self.assertEqual(usage.summarise(payload)["scoped"], [])

    def test_extra_usage_state_is_carried(self):
        out = usage.summarise(PAYLOAD)
        self.assertFalse(out["extra_usage_enabled"])
        self.assertEqual(out["extra_usage_reason"], "out_of_credits")


if __name__ == "__main__":
    unittest.main()
