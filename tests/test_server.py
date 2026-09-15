"""The security guards around the HTTP layer.

This server can change which account every Claude session on the machine uses,
so these tests matter more than the rendering ones.
"""

import json
import threading
import time
import unittest
import urllib.error
import urllib.request

from hotseat.collect import Collector
from hotseat.server import build_server


class FakeBackend:
    name = "fake"
    has_profiles = True
    can_switch = True

    def accounts(self):
        return []

    def capabilities(self):
        return {"backend": self.name, "profiles": True, "switch": True}


class StubCollector(Collector):
    def __init__(self):
        super().__init__(backend=FakeBackend())
        self._snapshot = {"generated_at": 0, "capabilities": FakeBackend().capabilities(),
                          "sessions": 3, "accounts": [], "error": None}

    def build(self):
        return self._snapshot


class LazinessTest(unittest.TestCase):
    """The server must do nothing while nobody is looking at it."""

    def test_no_background_thread_is_started(self):
        """A timer that refreshes for a closed page probes accounts for nobody."""
        self.assertFalse(hasattr(Collector, "start"),
                         "snapshots are built on request, not on a timer")

    def test_a_fresh_snapshot_is_reused_rather_than_rebuilt(self):
        collector = StubCollector()
        builds = []
        collector.build = lambda: (builds.append(1), {"generated_at": time.time()})[1]
        collector._snapshot = None
        collector.interval = 300
        collector.snapshot()
        collector.snapshot()
        self.assertEqual(len(builds), 1)

    def test_a_stale_snapshot_is_rebuilt_on_request(self):
        collector = StubCollector()
        builds = []
        collector.build = lambda: (builds.append(1), {"generated_at": time.time()})[1]
        collector.interval = 300
        collector._snapshot = {"generated_at": time.time() - 3600}
        collector.snapshot()
        self.assertEqual(len(builds), 1)

    def test_the_server_exits_once_nothing_is_asking_it_for_anything(self):
        server = build_server("127.0.0.1", 0, StubCollector(), idle_exit_s=0.2)
        server.last_request_at = time.time() - 10
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        thread.join(timeout=5)
        self.assertFalse(thread.is_alive(), "an unused dashboard must not live forever")
        server.server_close()

    def test_a_page_being_polled_keeps_it_alive(self):
        server = build_server("127.0.0.1", 0, StubCollector(), idle_exit_s=30)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        port = server.server_address[1]
        with urllib.request.urlopen(f"http://127.0.0.1:{port}/api/snapshot", timeout=5):
            pass
        thread.join(timeout=1)
        self.assertTrue(thread.is_alive())
        server.shutdown()
        server.server_close()


class ServerTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = build_server("127.0.0.1", 0, StubCollector())
        cls.port = cls.server.server_address[1]
        cls.token = cls.server.csrf_token
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()

    def url(self, path):
        return f"http://127.0.0.1:{self.port}{path}"

    def post(self, path, body=None, token=None, origin=None):
        headers = {"Content-Type": "application/json"}
        if token:
            headers["X-Hotseat-Token"] = token
        if origin:
            headers["Origin"] = origin
        request = urllib.request.Request(
            self.url(path), data=json.dumps(body or {}).encode(),
            headers=headers, method="POST")
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                return response.status, json.loads(response.read())
        except urllib.error.HTTPError as exc:
            return exc.code, json.loads(exc.read() or b"{}")

    # --- tests ----------------------------------------------------------
    def test_page_has_the_token_substituted(self):
        with urllib.request.urlopen(self.url("/"), timeout=10) as response:
            page = response.read().decode()
        self.assertIn(self.token, page)
        self.assertNotIn("__HOTSEAT_TOKEN__", page, "placeholder must be replaced")

    def test_snapshot_is_readable_without_a_token(self):
        with urllib.request.urlopen(self.url("/api/snapshot"), timeout=10) as response:
            self.assertEqual(response.status, 200)

    def test_mutating_request_without_a_token_is_refused(self):
        status, _ = self.post("/api/switch", {"alias": "x", "acknowledged": True})
        self.assertEqual(status, 403, "any page in the browser could otherwise post here")

    def test_wrong_token_is_refused(self):
        status, _ = self.post("/api/switch", {"alias": "x"}, token="not-the-token")
        self.assertEqual(status, 403)

    def test_foreign_origin_is_refused_even_with_a_token(self):
        status, _ = self.post("/api/verify", {"alias": "x"},
                              token=self.token, origin="https://evil.example")
        self.assertEqual(status, 403)

    def test_switch_requires_explicit_acknowledgement(self):
        status, body = self.post("/api/switch", {"alias": "x"}, token=self.token)
        self.assertEqual(status, 400)
        self.assertIn("3 running session", body["error"],
                      "the refusal should say what would have been disturbed")

    def test_unknown_endpoint_is_not_found(self):
        status, _ = self.post("/api/nonsense", {}, token=self.token)
        self.assertEqual(status, 404)


if __name__ == "__main__":
    unittest.main()
