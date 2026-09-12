"""Minimal HTTP smoke surface for the POST scientific adapter.

T0002 scaffold only: GET /healthz reports identity and version. Scientific
endpoints arrive with the adapter tasks; this module deliberately implements
nothing beyond the health contract (stdlib only, no product behaviour).
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


class HealthHandler(BaseHTTPRequestHandler):
    """HTTP handler exposing GET /healthz only."""

    def do_GET(self) -> None:  # noqa: N802 (stdlib naming)
        if self.path == "/healthz":
            body = json.dumps(health_payload()).encode("utf-8")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, fmt: str, *args: object) -> None:
        # Keep the scaffold quiet under tests; restore default logging with
        # the real server task.
        pass


def make_server(host: str, port: int) -> ThreadingHTTPServer:
    return ThreadingHTTPServer((host, port), HealthHandler)
