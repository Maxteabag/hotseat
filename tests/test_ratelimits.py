"""Header parsing. Fixtures mirror the shape the API actually returns."""

import unittest

from hotseat import ratelimits

ALLOWED = {
    "anthropic-ratelimit-unified-status": "allowed",
    "anthropic-ratelimit-unified-5h-status": "allowed",
    "anthropic-ratelimit-unified-5h-utilization": "0.36",
    "anthropic-ratelimit-unified-5h-reset": "1800000000",
    "anthropic-ratelimit-unified-7d-status": "allowed",
    "anthropic-ratelimit-unified-7d-utilization": "0.53",
    "anthropic-ratelimit-unified-7d-reset": "1800600000",
}


class SummariseTest(unittest.TestCase):
    def test_utilisation_is_read_as_a_fraction(self):
        out = ratelimits.summarise(ALLOWED)
        self.assertAlmostEqual(out["used_5h"], 0.36)
        self.assertAlmostEqual(out["used_7d"], 0.53)
        self.assertEqual(out["reset_5h"], 1800000000)

    def test_allowed_account_is_not_limited(self):
        out = ratelimits.summarise(ALLOWED)
        self.assertTrue(out["available"])
        self.assertFalse(out["limited"])

    def test_five_hour_rejection_marks_the_account_limited(self):
        headers = {**ALLOWED,
                   "anthropic-ratelimit-unified-status": "rejected",
                   "anthropic-ratelimit-unified-5h-status": "rejected",
                   "anthropic-ratelimit-unified-5h-utilization": "1.06"}
        out = ratelimits.summarise(headers)
        self.assertTrue(out["limited"])
        self.assertGreater(out["used_5h"], 1.0, "over-quota must not be clamped to 100%")

    def test_extra_usage_keeps_an_account_usable(self):
        """A rejected 5-hour window can still serve requests from purchased usage."""
        headers = {**ALLOWED, "anthropic-ratelimit-unified-status": "rejected"}
        self.assertTrue(ratelimits.summarise(headers)["available"])

    def test_missing_headers_degrade_to_unknown(self):
        out = ratelimits.summarise({})
        self.assertIsNone(out["used_5h"])
        self.assertIsNone(out["reset_5h"])
        self.assertEqual(out["status"], "unknown")

    def test_garbage_values_do_not_raise(self):
        out = ratelimits.summarise({
            "anthropic-ratelimit-unified-5h-utilization": "not-a-number",
            "anthropic-ratelimit-unified-5h-reset": "soon"})
        self.assertIsNone(out["used_5h"])
        self.assertIsNone(out["reset_5h"])


if __name__ == "__main__":
    unittest.main()
