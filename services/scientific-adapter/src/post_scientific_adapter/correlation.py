"""Correlation ids for the scientific adapter (T0007, docs/26 §2).

One id traces a request across every service boundary: the Go API, the web
app and this adapter all speak the same ``X-Correlation-ID`` header and the
same id shape, so a caller (web app or API) can hand its id to the adapter
and every adapter log line carries it.

The accepted shape is the shared monorepo one (Go internal/observability,
web lib/correlation): 8-64 chars of letters, digits, '.', '_' or '-'.
Anything else is rejected and a fresh id is generated — an untrusted
header must never reach a log.

This module is stdlib-only, like the rest of the adapter scaffold.
"""

from __future__ import annotations

import re
import secrets

CORRELATION_HEADER = "X-Correlation-ID"

# Same shape as Go's internal/observability and the web app: first char
# alnum, then 7-63 chars of alnum / '.' / '_' / '-'.
_CORRELATION_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{7,63}$")


def new_correlation_id() -> str:
    """Create a fresh correlation id (32 hex chars, same shape as Go's)."""
    return secrets.token_hex(16)


def is_valid_correlation_id(value: str) -> bool:
    """Validate an incoming correlation id against the shared shape."""
    return bool(_CORRELATION_RE.match(value))


def resolve_correlation_id(header: str | None) -> str:
    """Honour a valid incoming header value, else create a fresh edge id."""
    if header and is_valid_correlation_id(header):
        return header
    return new_correlation_id()
