"""POST scientific adapter.

Serves GET /healthz (liveness) and GET /readyz (readiness, no dependency
checks wired yet). Scientific parsing endpoints arrive with the adapter
tasks; the adapter never writes canonical PostgreSQL domain tables directly
(docs/50, docs/65).
"""

__version__ = "0.1.0"
