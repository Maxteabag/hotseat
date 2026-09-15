"""The local HTTP server.

Two deliberate restrictions, because this process can change which account every
Claude session on the machine uses:

  * it binds to the loopback interface unless told otherwise;
  * every state-changing request must carry a token minted at page load.

Without the token any website open in the browser could post to this server.
"""

from __future__ import annotations

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import secrets
import threading
import time
from importlib.resources import files

from . import actions, plugins
from . import inspect as inspect_mod
from .collect import Collector

WEB_DIR = files("hotseat").joinpath("web")
MAX_BODY_BYTES = 64 * 1024
#: Exit after this long with no requests. An open page polls every 15 seconds, so
#: this only fires once the last tab is gone and the server has nothing to serve.
IDLE_EXIT_S = 1800


class Handler(BaseHTTPRequestHandler):
    server_version = "hotseat"
    collector: Collector
    csrf_token: str

    def handle_one_request(self):
        self.server.last_request_at = time.time()
        super().handle_one_request()

    # --- plumbing ---------------------------------------------------------
    def log_message(self, fmt: str, *args) -> None:
        pass  # the default logger writes a line per poll, which is only noise

    def _send(self, status: int, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def _send_page(self) -> None:
        try:
            page = (WEB_DIR / "index.html").read_text()
        except OSError as exc:
            self._send(500, {"error": f"page missing: {exc}"})
            return
        body = page.replace("__HOTSEAT_TOKEN__", self.csrf_token).replace("__HOTSEAT_PLUGIN_SCRIPTS__", plugins.dashboard_scripts()).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def _read_json(self) -> dict:
        try:
            length = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            return {}
        if length <= 0 or length > MAX_BODY_BYTES:
            return {}
        try:
            return json.loads(self.rfile.read(length) or b"{}")
        except ValueError:
            return {}

    def _authorised(self) -> bool:
        """Reject anything that did not come from the page this server served."""
        if self.headers.get("X-Hotseat-Token") != self.csrf_token:
            return False
        origin = self.headers.get("Origin")
        if origin and not origin.startswith(("http://127.0.0.1", "http://localhost")):
            host = self.headers.get("Host") or ""
            if not origin.endswith(host):
                return False
        return True

    # --- routes -----------------------------------------------------------
    def do_GET(self) -> None:
        if self.path in ("/", "/index.html"):
            self._send_page()
        elif self.path.startswith("/api/inspect/"):
            identifier = self.path.rsplit("/", 1)[-1]
            try:
                self._send(200, inspect_mod.detail(identifier))
            except inspect_mod.InspectError as exc:
                self._send(404, {"error": str(exc)})
        elif self.path == "/api/snapshot":
            self._send(200, self.collector.snapshot())
        else:
            self._send(404, {"error": "not found"})

    def do_POST(self) -> None:
        if not self.path.startswith("/api/"):
            self._send(404, {"error": "not found"})
            return
        if not self._authorised():
            self._send(403, {"error": "missing or invalid request token"})
            return

        payload = self._read_json()
        alias = payload.get("alias")
        backend = self.collector.backend
        try:
            if self.path == "/api/refresh":
                self._send(200, self.collector.refresh())
            elif self.path == "/api/verify":
                self._send(200, actions.verify(backend, alias))
            elif self.path == "/api/launch":
                self._send(200, actions.launch(backend, alias))
            elif self.path == "/api/switch":
                sessions = self.collector.snapshot().get("sessions", 0)
                result = actions.switch(backend, alias, sessions,
                                        bool(payload.get("acknowledged")))
                self.collector.refresh()
                self._send(200, result)
            elif self.path == "/api/login":
                self._send(200, actions.login(alias))
            else:
                self._send(404, {"error": "not found"})
        except actions.ActionError as exc:
            self._send(400, {"error": str(exc)})
        except Exception as exc:
            self._send(500, {"error": f"{type(exc).__name__}: {exc}"})


def build_server(host: str = "127.0.0.1", port: int = 8787,
                 collector: Collector | None = None,
                 idle_exit_s: float = IDLE_EXIT_S) -> ThreadingHTTPServer:
    active = collector or Collector()
    token = secrets.token_urlsafe(32)

    handler = type("BoundHandler", (Handler,), {"collector": active, "csrf_token": token})
    server = ThreadingHTTPServer((host, port), handler)
    server.collector = active
    server.csrf_token = token
    server.last_request_at = time.time()
    if idle_exit_s:
        _watch_for_idle(server, idle_exit_s)
    return server


def _watch_for_idle(server, idle_exit_s: float) -> None:
    """Shut the server down once nothing has asked it for anything.

    A dashboard nobody has open should not be a process that lives forever. The
    page polls while it is open, so this only fires after the last tab closes.
    """
    def watch():
        while True:
            idle = time.time() - getattr(server, "last_request_at", 0)
            if idle >= idle_exit_s:
                print(f"hotseat: no requests for {idle / 60:.0f} minutes, exiting")
                threading.Thread(target=server.shutdown, daemon=True).start()
                return
            time.sleep(min(60.0, idle_exit_s - idle))

    threading.Thread(target=watch, name="hotseat-idle", daemon=True).start()
