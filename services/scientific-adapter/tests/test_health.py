"""Smoke tests for the T0002 scientific-adapter health surface."""

import json
import threading
import urllib.error
import urllib.request

import pytest

from post_scientific_adapter import __version__
from post_scientific_adapter.app import health_payload, make_server


def test_health_payload_is_stable() -> None:
    payload = health_payload()
    assert payload == {
        "service": "scientific-adapter",
        "status": "ok",
        "version": __version__,
    }


def test_healthz_serves_200_json() -> None:
    server = make_server("127.0.0.1", 0)
    port = server.server_address[1]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with urllib.request.urlopen(
            f"http://127.0.0.1:{port}/healthz", timeout=5
        ) as response:
            assert response.status == 200
            assert response.headers["Content-Type"] == "application/json"
            payload = json.load(response)
        assert payload["status"] == "ok"
        assert payload["service"] == "scientific-adapter"
        assert payload["version"] == __version__
    finally:
        server.shutdown()
        thread.join(timeout=5)
        server.server_close()


def test_unknown_path_is_404() -> None:
    server = make_server("127.0.0.1", 0)
    port = server.server_address[1]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with pytest.raises(urllib.error.HTTPError) as exc_info:
            urllib.request.urlopen(f"http://127.0.0.1:{port}/nope", timeout=5)
        assert exc_info.value.code == 404
    finally:
        server.shutdown()
        thread.join(timeout=5)
        server.server_close()
