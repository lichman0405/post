"""Command-line entry point for the POST scientific adapter."""

import argparse

from post_scientific_adapter import __version__
from post_scientific_adapter.app import make_server


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="scientific-adapter",
        description="POST scientific adapter (T0002 scaffold: /healthz only).",
    )
    parser.add_argument(
        "--host",
        default="127.0.0.1",
        help="listen host (default: 127.0.0.1)",
    )
    parser.add_argument(
        "--port",
        type=int,
        default=9000,
        help="listen port (default: 9000)",
    )
    parser.add_argument(
        "--version",
        action="version",
        version=f"scientific-adapter {__version__}",
    )
    args = parser.parse_args(argv)

    server = make_server(args.host, args.port)
    print(
        f"scientific-adapter {__version__} listening on {args.host}:{args.port}"
        " (healthz: /healthz)"
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nscientific-adapter shutting down")
    finally:
        server.server_close()
    return 0
