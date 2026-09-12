"""Command-line entry point for the POST scientific adapter.

The adapter validates its own environment (config.load_config_from_cwd):
POST_ENV selects the layer and a missing or ambiguous layer refuses to
start. --host/--port override the environment when given.

T0007: request logs are structured JSON lines on stderr, each carrying the
request's correlation id (see observability.configure_request_logging).
"""

from __future__ import annotations

import argparse
import sys

from post_scientific_adapter import __version__
from post_scientific_adapter.app import make_server
from post_scientific_adapter.config import (
    ConfigError,
    load_config_from_cwd,
)
from post_scientific_adapter.observability import configure_request_logging


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="scientific-adapter",
        description="POST scientific adapter (/healthz liveness, /readyz readiness).",
    )
    parser.add_argument(
        "--host",
        default=None,
        help=("listen host (default: POST_SCIENTIFIC_ADAPTER_HOST "
              "or 127.0.0.1)"),
    )
    parser.add_argument(
        "--port",
        type=int,
        default=None,
        help=("listen port (default: POST_SCIENTIFIC_ADAPTER_PORT "
              "or 9100)"),
    )
    parser.add_argument(
        "--version",
        action="version",
        version=f"scientific-adapter {__version__}",
    )
    args = parser.parse_args(argv)

    # The configuration is validated before anything starts: a missing or
    # ambiguous layer (or a malformed value) fails fast with an error naming
    # the offending variable. Never a silent fallback.
    try:
        cfg = load_config_from_cwd()
    except ConfigError as exc:
        print(f"scientific-adapter: config error: {exc}", file=sys.stderr)
        return 2

    host = args.host if args.host is not None else cfg.host
    port = args.port if args.port is not None else cfg.port

    configure_request_logging()

    server = make_server(host, port)
    print(
        f"scientific-adapter {__version__} listening on {host}:{port} "
        f"({cfg.describe()}; healthz: /healthz)"
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nscientific-adapter shutting down")
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":  # pragma: no cover
    sys.exit(main())
