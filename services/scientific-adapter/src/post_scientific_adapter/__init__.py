"""POST scientific adapter.

T0002 scaffold: serves GET /healthz only. Scientific parsing endpoints arrive
with the adapter tasks; the adapter never writes canonical PostgreSQL domain
tables directly (docs/50, docs/65).
"""

__version__ = "0.1.0"
