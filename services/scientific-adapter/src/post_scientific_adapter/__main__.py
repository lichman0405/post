"""Allow `python -m post_scientific_adapter` as an entry point."""

import sys

from post_scientific_adapter.cli import main

if __name__ == "__main__":
    sys.exit(main())
