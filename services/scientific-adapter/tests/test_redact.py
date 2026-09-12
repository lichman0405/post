"""Tests for output redaction (post_scientific_adapter.redact).

redact_url mirrors scripts/speclib.redact_url() exactly; the canary sweep
proves no secret survives the redaction path.
"""

from __future__ import annotations

import pytest

from post_scientific_adapter.redact import redact_url

CANARY = "canary-py-7f3a9c2e-0004"


@pytest.mark.parametrize(
    ("raw", "want"),
    [
        # user:secret@host -> user:***@host (keeps the user, drops the secret)
        ("https://user:pw@example.com/x", "https://user:***@example.com/x"),
        (
            "postgres://app:p%40ss@db.internal:5432/post",
            "postgres://app:***@db.internal:5432/post",
        ),
        # token-only userinfo -> ***@host
        ("https://token-only@host/path", "https://***@host/path"),
        # last '@' delimits userinfo (passwords may contain '@')
        ("https://u:p@ss@word@host/path", "https://u:***@host/path"),
        # scp-like forms keep their conventional user (no ://); the ssh://
        # form is rewritten exactly as speclib does
        ("git@github.com:owner/name.git", "git@github.com:owner/name.git"),
        (
            "ssh://git@github.com/owner/name.git",
            "ssh://***@github.com/owner/name.git",
        ),
        # no userinfo: unchanged
        ("http://127.0.0.1:9000", "http://127.0.0.1:9000"),
        ("host:9000", "host:9000"),
        ("", ""),
    ],
)
def test_redact_url_mirrors_speclib(raw: str, want: str) -> None:
    assert redact_url(raw) == want


def test_canary_never_survives_redaction() -> None:
    raw = f"https://u:{CANARY}@blob.example.com"
    got = redact_url(raw)
    assert CANARY not in got
    assert got == "https://u:***@blob.example.com"
