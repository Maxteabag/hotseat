"""Snapshot shaping, especially the sign-in deadline."""

import time
import unittest

from hotseat.backends.base import Account
from hotseat.collect import _account_view


class AccountViewTest(unittest.TestCase):
    def view(self, **kwargs):
        now = time.time()
        defaults = dict(
            alias="work", email="user@example.com", org="Example Org", plan="team",
            access_expires_at=int((now + 4 * 3600) * 1000),
            refresh_expires_at=int((now + 20 * 86400) * 1000),
            token="never-published")
        defaults.update(kwargs)
        return _account_view(Account(**defaults), None, None)

    def test_both_clocks_are_reported(self):
        view = self.view()
        self.assertAlmostEqual(view["access_hours_left"], 4.0, places=1)
        self.assertAlmostEqual(view["signin_days_left"], 20.0, places=1)

    def test_distant_deadline_is_not_flagged(self):
        self.assertFalse(self.view()["signin_due_soon"])

    def test_near_deadline_is_flagged(self):
        near = int((time.time() + 3 * 86400) * 1000)
        self.assertTrue(self.view(refresh_expires_at=near)["signin_due_soon"])

    def test_missing_expiry_does_not_raise(self):
        view = self.view(refresh_expires_at=None, access_expires_at=None)
        self.assertIsNone(view["signin_days_left"])
        self.assertFalse(view["signin_due_soon"])

    def test_view_never_includes_the_token(self):
        self.assertNotIn("token", self.view())


if __name__ == "__main__":
    unittest.main()
