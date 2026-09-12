# POST scientific adapter

Isolated Python 3.12+ service for the scientific Python ecosystem (pymatgen,
ASE and friends arrive with the adapter tasks), managed by `uv`.

- **Boundary:** talks to the Go core only via stable API/RPC/queue; never
  writes canonical PostgreSQL domain tables directly (docs/50, docs/65).
- **Health surface:** serves `GET /healthz` (liveness only) and
  `GET /readyz` (readiness; no dependency checks wired yet, so it
  truthfully reports `"checks": {}`). Stdlib only, no product logic.
- **Default port 9100** — not 9000: 9000/9001 are the MinIO S3 API/console
  ports from docker-compose.yml (T0003). Override with
  `POST_SCIENTIFIC_ADAPTER_PORT` (T0006 port-collision fix).

## Commands

```bash
uv sync            # create the locked environment
uv run pytest      # run the smoke tests
uv run scientific-adapter --port 9100   # start the health surface
```
