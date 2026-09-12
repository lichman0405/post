# POST scientific adapter

Isolated Python 3.12+ service for the scientific Python ecosystem (pymatgen,
ASE and friends arrive with the adapter tasks), managed by `uv`.

- **Boundary:** talks to the Go core only via stable API/RPC/queue; never
  writes canonical PostgreSQL domain tables directly (docs/50, docs/65).
- **T0002 scaffold:** serves `GET /healthz` only (stdlib, no product logic).

## Commands

```bash
uv sync            # create the locked environment
uv run pytest      # run the smoke tests
uv run scientific-adapter --port 9000   # start the health surface
```
