"""Minimal HTTP health surface for the POST scientific adapter.

GET /healthz reports process liveness only (identity + version), never
depending on a downstream service. GET /readyz reports readiness: no
dependency is wired yet (the adapter is a stdlib-only scaffold), so it
truthfully answers 200 "ready" with an empty checks map — scientific
endpoints and their dependencies arrive with the adapter tasks, and this
module deliberately implements nothing beyond the health contract (stdlib
only, no product behaviour).
"""

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from post_scientific_adapter import __version__


def health_payload() -> dict[str, str]:
    """Stable /healthz payload: service identity, status, version."""
    return {
        "service": "scientific-adapter",
        "status": "ok",
        "version": __version__,
    }


def ready_payload() -> dict[str, object]:
    """Stable /readyz payload: ready, with no dependency checks wired yet.

    The empty checks map is the truth: reporting a fake dependency state
    would be a lie (T0006 readiness contract).
    """
    return {
        "service": "scientific-adapter",
        "status": "ready",
        "version": __version__,
        "checks": {},
    }


class HealthHandler(BaseHTTPRequestHandler):
    """HTTP handler exposing GET /healthz and GET /readyz only."""

    def do_GET(self) -> None:
        if self.path == "/healthz":
            self._json(200, health_payload())
        elif self.path == "/readyz":
            self._json(200, ready_payload())
        else:
            self.send_response(404)
            self.end_headers()

    def _json(self, code: int, payload: dict[str, object]) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt: str, *args: object) -> None:
        # Keep the scaffold quiet under tests; restore default logging with
        # the real server task.
        pass


def make_server(host: str, port: int) -> ThreadingHTTPServer:
    return ThreadingHTTPServer((host, port), HealthHandler)
