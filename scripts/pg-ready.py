#!/usr/bin/env python3
"""Pre-flight probe for `make test-integration` (T0008).

Checks that the PostgreSQL admin endpoint from a libpq URL is actually
accepting connections before the integration suite (tests/integration) is
started, so a missing database produces ONE loud, actionable failure instead
of a wall of pgx dial errors.

This performs a real PostgreSQL handshake — it sends a protocol 3.0
StartupMessage and requires a protocol reply — rather than only opening a TCP
connection. A bare connect is not the question being asked: PostgreSQL listens
on its port *before* it is ready and then restarts, so a TCP probe reports
ready during initialisation and the caller's next query is reset. That is not
hypothetical — it is how `make test-integration` first failed after
`docker compose up`, with the probe green and the test unable to connect.

What it does NOT do: verify credentials. A server that answers the handshake
is reported ready even if the password is wrong; the suite then fails on its
own terms, which is a better error than this probe could produce without
reimplementing SCRAM.

Usage: python3 scripts/pg-ready.py postgres://user:pass@host:port/db

Exit codes: 0 = accepting connections; 1 = not reachable / not answering as
PostgreSQL (reason printed on stderr); 2 = usage error / malformed URL.
"""
import socket
import struct
import sys
from urllib.parse import urlparse

USAGE_ERROR = 2
TIMEOUT_S = 5
PROTOCOL_VERSION = 196608  # 3.0


def startup_message(user, database):
    """A protocol-3.0 StartupMessage asking to connect as user/database."""
    params = (b"user\x00" + user.encode() + b"\x00"
              b"database\x00" + database.encode() + b"\x00\x00")
    return struct.pack("!ii", len(params) + 8, PROTOCOL_VERSION) + params


def error_message(payload):
    """Extract the human-readable text from an ErrorResponse body.

    The body is a sequence of NUL-terminated fields, each introduced by a type
    byte; 'M' is the primary message. The server's own words are far more
    useful than anything this probe could invent ("role ... does not exist",
    "the database system is starting up").
    """
    text, parts = payload, []
    while len(text) > 1 and text[0:1] not in (b"", b"\x00"):
        kind, _, rest = text[0:1], None, text[1:]
        body, sep, remainder = rest.partition(b"\x00")
        if not sep:
            break
        if kind == b"M":
            parts.append(body.decode("utf-8", "replace"))
        text = remainder
    return "; ".join(parts)


def main(argv):
    if len(argv) != 1:
        print("usage: python3 scripts/pg-ready.py "
              "postgres://user:pass@host:port/db", file=sys.stderr)
        return USAGE_ERROR
    url = argv[0]
    parsed = urlparse(url)
    if parsed.scheme not in ("postgres", "postgresql") \
            or not parsed.hostname or not parsed.port:
        print(f"pg-ready: malformed PostgreSQL URL: {url} "
              f"(want postgres://user:pass@host:port/db)", file=sys.stderr)
        return USAGE_ERROR
    host, port = parsed.hostname, parsed.port
    user = parsed.username or "postgres"
    database = (parsed.path or "").lstrip("/") or user

    def unreachable(reason):
        print(f"pg-ready: no PostgreSQL accepting connections at {host}:{port} "
              f"({reason})", file=sys.stderr)
        print("start the infra stack (make infra-up && make infra-init) or "
              "set POSTGRES_TEST_ADMIN_URL to a reachable admin URL",
              file=sys.stderr)
        return 1

    try:
        with socket.create_connection((host, port), timeout=TIMEOUT_S) as sock:
            sock.settimeout(TIMEOUT_S)
            sock.sendall(startup_message(user, database))
            head = sock.recv(1)
            if not head:
                # Listening but not speaking: the server accepted the socket
                # and closed it — initialising, or not PostgreSQL at all.
                return unreachable("the connection was closed without a "
                                   "PostgreSQL reply (still starting up?)")
            if head == b"E":
                length = struct.unpack("!i", sock.recv(4))[0]
                detail = error_message(sock.recv(max(length - 4, 0)))
                return unreachable(f"the server refused the connection"
                                   f"{': ' + detail if detail else ''}")
            if head != b"R":
                return unreachable(f"unexpected first reply {head!r} — this "
                                   f"does not look like PostgreSQL")
    except OSError as exc:
        return unreachable(exc)

    print(f"pg-ready: PostgreSQL accepting connections at {host}:{port}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
