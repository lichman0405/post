"""Minimal HTTP health surface for the POST scientific adapter.

GET /healthz reports process liveness only (identity + version), never
depending on a downstream service. GET /readyz reports readiness: no
dependency is wired yet (the adapter is a stdlib-only scaffold), so it
truthfully answers 200 "ready" with an empty checks map — scientific
endpoints and their dependencies arrive with the adapter tasks, and this
module deliberately implements nothing beyond the health contract (stdlib
only, no product behaviour).

T0007: every request carries a correlation id — a valid incoming
``X-Correlation-ID`` is honoured, otherwise the edge creates one — the id is
echoed in the response and every per-request log line carries it (docs/26
§2). The request line is never logged raw: only the path without the query
string reaches the log (query values are a classic credential carrier).
"""

from __future__ import annotations

import json
import logging
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

from post_scientific_adapter import __version__
from post_scientific_adapter.correlation import (
    CORRELATION_HEADER,
    resolve_correlation_id,
)

# Structured per-request logging; the handler is configured by cli.py (JSON
# formatter) and the correlation id travels via the "extra" record field.
logger = logging.getLogger("post.scientific.adapter")


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

    def do_GET(self) -> None:  # noqa: N802 (stdlib naming)
        # The correlation id is resolved once per request so the logged id
        # and the echoed header can never diverge.
        self._correlation_id = resolve_correlation_id(
            self.headers.get(CORRELATION_HEADER)
        )
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
        # Echo the correlation id so the caller can correlate its request
        # with every adapter log line for it.
        self.send_header(CORRELATION_HEADER, self._correlation_id)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt: str, *args: object) -> None:  # noqa: N802
        # Structured per-request line (T0007). The path is stripped of its
        # query string — never log the raw request line, it may carry
        # credentials in the query.
        del fmt, args
        correlation_id = getattr(self, "_correlation_id", "")
        if not correlation_id:
            correlation_id = resolve_correlation_id(
                self.headers.get(CORRELATION_HEADER)
            )
        path = urlsplit(self.path).path
        logger.info(
            "request completed",
            extra={
                "correlation_id": correlation_id,
                "method": self.command,
                "path": path,
            },
        )


def make_server(host: str, port: int) -> ThreadingHTTPServer:
    return ThreadingHTTPServer((host, port), HealthHandler)
