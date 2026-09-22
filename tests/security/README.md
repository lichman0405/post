# The Master Security/Quality Gate

One command, every check this tree really has:

```bash
bash tests/security/master-security-gate.sh
```

It runs each check, prints its name, its result and the evidence lines that
prove the check did something, and exits 0 only when every row both exited 0
*and* printed its own evidence. A row that exits 0 without printing its
evidence is `UNSUBSTANTIATED` and red: an exit code alone is not a pass, and
a check that quietly stopped checking is exactly the failure this gate exists
to catch.

```bash
bash tests/security/master-security-gate.sh --list        # the inventory
bash tests/security/master-security-gate.sh --list-json   # machine-readable
bash tests/security/master-security-gate.sh --only a11y,vuln-go
bash tests/security/master-security-gate.sh --report /tmp/gate.json
```

## The rows

| id | what it proves | needs |
| --- | --- | --- |
| `secret-scan` | no committed `*.env.example` holds a secret — and the detector's own planted-secret failing mode runs in the same row, so a scanner that stopped scanning goes red | go |
| `permission-negative-e2e` | a private project stays invisible (`TestE2EPrivacyNegative`) | go |
| `owasp-smoke` | headers value for value, CORS refusals, anonymous write 401, rate-limit boundary, fail-closed — against the real API binary, real PostgreSQL, real Redis | go, python3, psql, curl, redis |
| `a11y` | axe (wcag + best-practice) at two viewports plus keyboard focus traversal, in real Chromium | node, `web-deps` (apps/web/node_modules) |
| `deploy-template` | the staging template's 19 rules, including the rule-refuting self-test and the planted-secret mutation check | python3, file |
| `vuln-go` | `govulncheck ./...` — Go dependency CVEs | govulncheck |
| `vuln-node` | `pnpm audit --audit-level=low` — the web workspace | pnpm |
| `vuln-python` | `uv audit` — the scientific adapter | uv |
| `absence-manifest` | `ops/security/absent-checks.json` is complete, every covered item names a check that still exists, and every absence witness still shows the gap | python3 |

Exit codes: `0` all rows green · `1` a row failed (or was unsubstantiated) ·
`2` a row was NOT ASKED · `3` usage/registry error · `4` the gate's own
accounting failed its self-test.

## Two different kinds of "not here"

These are deliberately separate, and conflating them is how a gap goes
unnoticed:

1. **Not here at this moment — per-run, loud.** A check whose host lacks a
   tool it needs is not skipped silently. It prints `NOT ASKED` with the
   reason, is counted, appears in `not_asked[]` in the `--report` JSON, and
   exits 2. `--allow-not-asked` downgrades that to exit 0 but still prints the
   row as NOT ASKED and ends with `NOT A FULL GREEN` — the banner is there so
   the word "green" cannot be read off it.
2. **Not here in this release — static, written down.** SAST, the container
   scan and the SBOM are capabilities nothing in this tree performs. They live
   in `ops/security/absent-checks.json`, each with what stands in for it today,
   what adding it would take, and what the gap costs — and each with an
   `absence_witness` command that the `absence-manifest` row *runs*: add a
   Dockerfile, an SBOM generator or a SAST stage and the gate goes red until
   the manifest says so. Absence is recorded, never omitted, and whether it is
   an accepted V1 risk is the Supervisor's written call (docs/23 §11).

## Proving the instruments can say no

```bash
bash tests/security/master-gate-mutation-check.sh
```

Six mutations, each on a copy of the tree, each requiring the matching
instrument to go red and name what it found: a planted secret in an
`*.env.example`, a permission check removed from the API, a header removed
from `internal/security`, a check that exits 0 while printing nothing, a
manifest with its SAST entry deleted, and — for the dependency audits — the
verdict-deciding input swapped: the real `govulncheck` behind a shim whose
`-db` names a synthetic advisory for a symbol this tree calls, so the
`vuln-go` row has to report it and fail. It hashes the tree before and after
and fails if the working tree changed — a mutation check that leaves a
mutation behind is worse than none.

`bash tests/security/master-security-gate.sh --selftest` does the same for the
gate's own accounting, with decoy rows rather than real code.

CI wiring is `.github/workflows/**` — the Supervisor's surface. The rows are
independent and can be split across jobs; `security-smoke` is already the G3
job that runs `tests/security/owasp-smoke.sh`, and this gate runs it too.
