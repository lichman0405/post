"""Tests for the adapter correlation-id path (T0007, docs/26 §2)."""

import json
import logging
import threading
import urllib.request

from unittest import mock

import pytest

from post_scientific_adapter import observability
from post_scientific_adapter.app import make_server
from post_scientific_adapter.correlation import (
    CORRELATION_HEADER,
    is_valid_correlation_id,
    new_correlation_id,
    resolve_correlation_id,
)


def test_generated_ids_are_valid_and_unique() -> None:
    seen: set[str] = set()
    for _ in range(100):
        cid = new_correlation_id()
        assert is_valid_correlation_id(cid), f"generated id {cid} invalid"
        assert cid not in seen, f"duplicate generated id {cid}"
        seen.add(cid)


def test_validation_accepts_shared_shape_and_rejects_hostile_input() -> None:
    valid = [
        "0123456789abcdef0123456789abcdef",  # Go hex form
        "3f9c21e5-b8d4-4c0a-9f1e-7d3b2a91c4f8",  # web UUID form
        "web-trace-abc123",
        "a.b_c-1x",
    ]
    for cid in valid:
        assert is_valid_correlation_id(cid), f"rejected {cid}"

    invalid = [
        "",
        "short",
        "a" * 65,
        "has space",
        "../../etc/passwd",
        "a;log-injection",
        "a\nb",
        "-leading",
        "日本国",
    ]
    for cid in invalid:
        assert not is_valid_correlation_id(cid), (
            f"accepted hostile input {cid!r}"
        )


def test_resolve_honours_valid_incoming_id() -> None:
    assert resolve_correlation_id("web-trace-abc123") == "web-trace-abc123"


def test_resolve_generates_fresh_id_for_absent_or_invalid() -> None:
    for value in (None, "", "bad id with spaces", "x"):
        cid = resolve_correlation_id(value)
        assert is_valid_correlation_id(cid), (
            f"resolve({value!r}) = {cid} invalid"
        )
        assert cid != value


def _serve_once() -> tuple[object, int, threading.Thread]:
    server = make_server("127.0.0.1", 0)
    port = server.server_address[1]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    return server, port, thread


def test_request_echoes_header_and_logs_correlation_id() -> None:
    server, port, thread = _serve_once()
    request_logger = logging.getLogger(observability.REQUEST_LOGGER)
    try:
        with mock.patch.object(
            request_logger, "info"
        ) as info:
            req = urllib.request.Request(
                f"http://127.0.0.1:{port}/healthz",
                headers={CORRELATION_HEADER: "web-trace-abc123"},
            )
            with urllib.request.urlopen(req, timeout=5) as response:
                assert response.status == 200
                assert response.headers[CORRELATION_HEADER] == "web-trace-abc123"
            info.assert_called_once()
            args, kwargs = info.call_args
            assert kwargs["extra"]["correlation_id"] == "web-trace-abc123"
            assert kwargs["extra"]["path"] == "/healthz"
    finally:
        server.shutdown()
        thread.join(timeout=5)
        server.server_close()


def test_request_without_header_gets_fresh_echoed_id() -> None:
    server, port, thread = _serve_once()
    request_logger = logging.getLogger(observability.REQUEST_LOGGER)
    try:
        with mock.patch.object(
            request_logger, "info"
        ) as info:
            with urllib.request.urlopen(
                f"http://127.0.0.1:{port}/readyz", timeout=5
            ) as response:
                assert response.status == 200
                cid = response.headers[CORRELATION_HEADER]
                assert is_valid_correlation_id(cid), f"echoed id {cid} invalid"
            args, kwargs = info.call_args
            assert kwargs["extra"]["correlation_id"] == cid
    finally:
        server.shutdown()
        thread.join(timeout=5)
        server.server_close()


def test_logged_path_never_carries_the_query_string() -> None:
    server, port, thread = _serve_once()
    request_logger = logging.getLogger(observability.REQUEST_LOGGER)
    try:
        with mock.patch.object(
            request_logger, "info"
        ) as info:
            # The scaffold matches paths exactly, so an unknown path with a
            # query answers 404 — the interesting case for the log: the
            # query must be stripped from the logged path either way.
            with pytest.raises(urllib.error.HTTPError) as exc_info:
                urllib.request.urlopen(
                    f"http://127.0.0.1:{port}/nope?token=topsecret", timeout=5
                )
            assert exc_info.value.code == 404
            args, kwargs = info.call_args
            assert kwargs["extra"]["path"] == "/nope"
            assert "token" not in kwargs["extra"]["path"]
            assert "topsecret" not in kwargs["extra"]["path"]
    finally:
        server.shutdown()
        thread.join(timeout=5)
        server.server_close()


def test_json_formatter_output_is_one_object_per_line() -> None:
    formatter = observability.JsonFormatter()
    record = logging.LogRecord(
        name="test", level=logging.INFO, pathname=__file__, lineno=1,
        msg="request completed", args=(), exc_info=None,
    )
    record.correlation_id = "web-trace-abc123"  # type: ignore[attr-defined]
    line = formatter.format(record)
    payload = json.loads(line)
    assert payload["msg"] == "request completed"
    assert payload["correlation_id"] == "web-trace-abc123"
    assert payload["level"] == "info"
    assert "time" in payload
