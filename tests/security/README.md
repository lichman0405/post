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
| `owasp-smoke` | headers value for value, CORS refusals, anonymous write 401, rate-limit boundary, fail-closed — against the real API binary, real PostgreSQL, real Redis | go, python3, psql, curl, redis, `redis-cli` (S0 resets its own rate-limit buckets and reads them back), node (S8 asserts the web header set through `node --test`) |
| `a11y` | axe (wcag + best-practice) at two viewports plus keyboard focus traversal, in real Chromium | node, `web-deps` (apps/web/node_modules) |
| `deploy-template` | the staging template's 19 rules, including the rule-refuting self-test and the planted-secret mutation check | python3, `py:yaml` (PyYAML — the template is YAML), go, file |
| `vuln-go` | `govulncheck ./...` — Go dependency CVEs | govulncheck |
| `vuln-node` | `pnpm audit --audit-level=low` — the web workspace | pnpm |
| `vuln-python` | `uv audit` — the scientific adapter | uv |
| `sast-go` | `gosec` over this module's own Go source — the package set `go list ./...` names, minus the package directories git ignores (each one is printed with the rule that drops it): every finding is either new (red) or one line of `ops/ci/gosec-baseline.txt` with a written review — no `-exclude-dir`, no severity floor, no `#nosec`, every scanned file is checked to be under the repository root and not git-ignored before the scanner runs, and a floor on the number of files scanned so a scan that covered nothing is red | go, gosec |
| `sast-python` | `bandit` over the adapter's source under the same per-finding rule (`ops/ci/bandit-baseline.txt`) | python3, bandit |
| `sast-node` | `eslint` with every rule of `eslint-plugin-security/recommended` forced to `error`, over `apps/web` + `packages/ui`, with a floor on the rule count so a plugin upgrade cannot quietly narrow the rule set | node, the `node-tools` toolchain (`make security-tools`) |
| `sbom-go` | a CycloneDX document built from `go.mod` by `cyclonedx-gomod`, licence evidence included: it parses, it has components, and it witnesses the key Go dependencies of `docs/40` | go, cyclonedx-gomod |
| `sbom-node` | the same for the pnpm workspace, from the installed tree (`pnpm sbom`, CycloneDX 1.5) | pnpm, the workspace's `node_modules` |
| `sbom-python` | the same for the adapter: `uv export` plus each installed distribution's own `METADATA` for licences | uv, the adapter's `.venv` |
| `license-audit` | every component of all three SBOMs judged against `ops/security/license-allowlist.json`. Three verdicts, and the same three are written in that file's `policy.no_licence` and in `license_audit.py`: a deny-class or unlisted licence is **red**; a component named in `unlicensed` **with** a checkable witness is counted as licensed by that file; a component named with **no** witness is printed as `LICENCE UNVERIFIED` on every run and stays out of the licence counts — printed, never passed off as fine, and never cleared. `colorama` is the live case (uv.lock locks it to `sys_platform == 'win32'`, so no distribution is installed on Linux and there is no METADATA to read) | python3, the three `.sbom/*.cdx.json` and their `.run` stamps (the `sbom-*` rows write them first — this row's `requires` names them) |
| `container-scan` | a *guarded absence*, not a scan: green only while the tree has no container build file, printing the `no-dockerfile` tree claim it rests on and the directories it does **not** walk with the reason for each — and red the moment a `Dockerfile`/`Containerfile` appears where the walk can see it, because then there is an image to scan and this row has to be replaced | python3, file |
| `absence-manifest` | `ops/security/absent-checks.json` is complete, every covered item names a check that still exists, every named gap still shows its gap, and every guarded absence names a guard the gate really registers | python3 |

Exit codes: `0` all rows green · `1` a row failed (or was unsubstantiated) ·
`2` a row was NOT ASKED · `3` usage/registry error · `4` the gate's own
accounting failed its self-test.

## The directories the container scan does not walk

The `container-scan` row's claim is "no container build file in the tree", and
that claim is about the directories it *does* walk. It prints the ones it
skips, with the reason for each, on every run — a walk with exclusions whose
exclusions nobody sees is a claim nobody can check, and "I put a Dockerfile in
`post-wt/` and the row stayed green" has to be answerable from the row's own
output. So each of these is printed, and each is then walked *on its own* for
container build files: anything found there is printed too, named, as a file
that lives in a directory listed here and was not judged as this tree's. It
does not turn the row red — an installed package's or another checkout's
Dockerfile is not this tree's file — and it does not vanish either.

| directory | why it is not this tree's content |
| --- | --- |
| `.git` | the repository's own object store: compressed objects and refs, and the one directory whose file names say nothing about the checkout |
| `.rddev` | the orchestrator's runtime state — and other Workers' worktrees, each a whole checkout of this repository. A Dockerfile in another Worker's tree is not this tree's file, and walking it would let one branch decide another task's gate |
| `post-wt` | the Supervisor's own worktrees (`rddev branch create --worktree post-wt/...`): separate checkouts, same reason as `.rddev` |
| `node_modules` | installed npm packages: a Dockerfile inside a dependency's published tarball is the dependency's. `ops/runbook-verify.py`'s walk leaves installed trees out for the same reason |
| `.venv` | the scientific adapter's installed Python packages (same reason as `node_modules`) |
| `.next` | the web app's generated build output |
| `.sbom` | the CycloneDX documents this gate writes and reads back (generated, gitignored: `sbom.sh`) |
| `.backup-dr` | the backup/DR drill's artifacts — database dumps, blob copies, git mirrors — a copy of an environment rather than a source change (`docs/37_BACKUP_DR.md`) |
| `.dev` | dev-stack runtime scratch: `POST_MAIL_SINK_DIR` defaults to `.dev/mail`, and the sink writes whole notification bodies there |

The same list, with the same reasons, is printed by `tests/security/container-scan.sh`,
and built from one `PRUNED` array there so the walk and the report cannot drift
apart.

## Documents from an earlier run

Two rows judge artifacts another row produced: `license-audit` reads the three
`.sbom/*.cdx.json` the `sbom-*` rows write, and those rows write them *in the
same run*. The gate exports `POST_GATE_RUN_ID` to every row, each `sbom-*` row
stamps the id of the run that wrote each document beside it (`.sbom/<face>.run`),
and `license-audit` compares the stamps with its own run id. They match in a
full gate run. Under `--only license-audit`, with an `sbom-*` row NOT ASKED, or
with a generator run by hand, they do not — and the row says so on its own `ok`
line and again at the end of its output, because a green line about the tree an
earlier run saw is exactly the kind of green this gate exists to refuse. It does
not fail for it: the verdicts are still about the bytes on disk, and a row that
went red whenever it was run alone would be unusable. To make hand-run pieces
agree, give them one id:
`POST_GATE_RUN_ID=myrun bash tests/security/sbom.sh go && POST_GATE_RUN_ID=myrun python3 tests/security/license_audit.py`.

`POST_SBOM_REUSE` is the mutation hook that validates the document on disk
instead of regenerating it, and its word list is exact: only `1`, `true` or
`yes` mean reuse. Everything else — unset, empty, `0`, `no`, a typo —
regenerates, because `POST_SBOM_REUSE=0` reads as "do not reuse" to a human and
a hook whose off switch turns it on is a hook that lies.

## Two different kinds of "not here"

These are deliberately separate, and conflating them is how a gap goes
unnoticed:

1. **Not here at this moment — per-run, loud.** A check whose host lacks a
   tool it needs is not skipped silently. It prints `NOT ASKED` with the
   reason, is counted, appears in `not_asked[]` in the `--report` JSON, and
   exits 2. `--allow-not-asked` downgrades that to exit 0 but still prints the
   row as NOT ASKED and ends with `NOT A FULL GREEN` — the banner is there so
   the word "green" cannot be read off it.
2. **Not here in this release — static, written down, and guarded.** The
   container scan is the one capability this tree still cannot perform: nothing
   builds an image, so there is nothing to scan (`no-dockerfile`,
   `ops/runbook-steps.json`). It is a row of the gate like any other
   (`container-scan`) rather than a footnote, and it is green only while that
   stays true: it walks the tree for a build file, prints the claim it rests on,
   and goes red the moment a `Dockerfile`/`Containerfile` appears — the red says
   build the image and scan it, or the Supervisor accepts the gap in writing.
   `ops/security/absent-checks.json` records the same thing, with what stands in
   for it today, what adding it would take, what the gap costs, and the
   witnesses the `absence-manifest` row *runs* — so the absence cannot outlive
   the fact. SAST and the SBOM used to be recorded here; both are gate rows now.
   Absence is recorded, never omitted, and whether what is left is an accepted
   V1 risk is the Supervisor's written call (docs/23 §11) — made, for this
   capability, in `docs/adr/ADR-028-v1-ships-no-container-image.md`, which the
   manifest cites as `risk_accepted_in` and witnesses by name.

## Proving the instruments can say no

```bash
bash tests/security/master-gate-mutation-check.sh
```

Six mutations, each on a copy of the tree, each requiring the matching
instrument to go red and name what it found: a planted secret in an
`*.env.example`, a permission check removed from the API, a header removed
from `internal/security`, a check that exits 0 while printing nothing, the
recorded absence of `ops/security/absent-checks.json` rotted in both of its
shapes (deleted outright, and left in place with its `guard_check_id` pointed
at a check id this gate does not register), and — for the dependency audits —
the verdict-deciding input swapped: the real `govulncheck` behind a shim whose
`-db` names a synthetic advisory for a symbol this tree calls, so the
`vuln-go` row has to report it and fail. It hashes the tree before and after
and fails if the working tree changed — a mutation check that leaves a
mutation behind is worse than none.

`bash tests/security/master-security-gate.sh --selftest` does the same for the
gate's own accounting, with decoy rows rather than real code.

CI wiring is `.github/workflows/**` — the Supervisor's surface. The rows are
independent and can be split across jobs; `security-smoke` is already the G3
job that runs `tests/security/owasp-smoke.sh`, and this gate runs it too.
