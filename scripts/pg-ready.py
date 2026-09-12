#!/usr/bin/env python3
"""Pre-flight probe for `make test-integration` (T0008).

Checks that the PostgreSQL admin endpoint from a libpq URL accepts a TCP
connection before the integration suite (tests/integration) is started, so a
missing database produces ONE loud, actionable failure instead of a wall of
pgx dial errors.

Usage: python3 scripts/pg-ready.py postgres://user:pass@host:port/db

Exit codes: 0 = reachable; 1 = not reachable (reason printed on stderr);
2 = usage error / malformed URL.
"""
import socket
import sys
from urllib.parse import urlparse

USAGE_ERROR = 2
TIMEOUT_S = 3


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
    try:
        with socket.create_connection((host, port), timeout=TIMEOUT_S):
            pass
    except OSError as exc:
        print(f"pg-ready: no PostgreSQL reachable at {host}:{port} ({exc})",
              file=sys.stderr)
        print("start the infra stack (make infra-up && make infra-init) or "
              "set POSTGRES_TEST_ADMIN_URL to a reachable admin URL",
              file=sys.stderr)
        return 1
    print(f"pg-ready: PostgreSQL reachable at {host}:{port}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
