# `ops/seed-demo.sh` — the seed demo, built and verified

One command turns an **empty** database into the demo project
`docs/34_SEED_DEMO_PROJECT.md` describes (slug `demo-mof-humidity-separation`),
and measures the result. It is repeatable: the second run finds what the first
one left instead of colliding with it.

```bash
ops/seed-demo.sh --reset                 # drop + recreate the DB, migrate, build, verify
ops/seed-demo.sh                         # run again on what the first run left
ops/seed-demo.sh --external 0            # without the fork-dependent part
ops/seed-demo.sh --db post               # into the repository's default dev database
```

The plan is `examples/seed-demo/demo-plan.json`; what it means is documented in
`examples/seed-demo/README.md`. Every payload in the demo is synthetic.

## What it needs

PostgreSQL, Redis, Gitea and MinIO must be up (they are infrastructure:
`docker compose up -d`, docs/66). The application itself runs host-native —
`ops/seed-demo.sh` builds `cmd/api`, starts it on a free port against the
target database, and drives that process over HTTP.

| flag | default | meaning |
| --- | --- | --- |
| `--reset` | off | drop and recreate the target database before building |
| `--db NAME` | `post_seed_demo` | the database to build into |
| `--external N` | `1` | `0` skips the fork, the external user's contribution and its PR |
| `--plan PATH` | `examples/seed-demo/demo-plan.json` | the plan to build |
| `--api-addr HOST:PORT` | a free port | where the API listens |

| environment | meaning |
| --- | --- |
| `SEED_DEMO_DB`, `POST_DB_*`, `POST_REDIS_ADDR`, `POST_BLOB_*`, `POST_GITEA_*` | connection settings (dev defaults match `docker-compose.yml`) |
| `SEED_DEMO_SUMMARY` | where the JSON summary lands (default `./.seed-demo-summary.json`, in the tree the script is run from — the same JSON is the last line of stdout, so `ops/seed-demo.sh \| jq .` needs no file) |
| `SEED_DEMO_API_LOG` | keep the API's own stdout/stderr — the first place to look when a stage fails |

## What each stage does

1. **Preflight** — the four infrastructure ports. Nothing is started for you.
2. **Gitea service token** — minted through Gitea's API; the fork path is
   disabled without one, and `--external 1` fails rather than silently skipping.
3. **`--reset`** — `seeddemo reset` drops and recreates the database, saying
   exactly what it deletes. Without the flag the existing database is used.
4. **Migrations** — `rddev db migrate` applies `infra/migrations/**`.
5. **API** — `go build ./cmd/api`, start, wait for `/healthz`.
6. **Build** — `seeddemo build` drives the plan through that API (below).
7. **Verify** — `seeddemo verify` measures the result **by querying
   PostgreSQL**, never by reading the builder's return value. Every check
   carries the SQL it ran.
8. **Summary** — the API is stopped and one JSON object goes to stdout, last
   line, with stderr carrying the narration: `ops/seed-demo.sh | jq .` works.

**Exit code** is 0 when every stage completed and every verifier check passed.
A refusal the plan *asked for* does not change it: docs/34 wants the demo to
carry a protocol scientific conflict, and the product's refusal to merge it
(`409 MERGE_BLOCKED` / `MERGE_CONFLICT_UNDECIDED`) is what proves the conflict
exists. The summary reports it under `build_refusals`, apart from
`build_failures`, so "the demo worked" stays readable.

## The four paths a seeded item arrives by

The build report names one per item, so "through the product" is a claim the
reader can check item by item.

| path label | what it means |
| --- | --- |
| `product API (HTTP)` | an HTTP call a human could have made from a browser — objects, relations, evidence assertions, pull requests, reviews, merges, publications, releases, assets, forks |
| `product application service, in-process` | the product's own service, composed the way `cmd/api/main.go` composes it, called in-process — used **only** for review routing, which this build exposes no HTTP route for (follow-up issue) |
| `database (read-only discovery of an earlier run)` | a read that recognises what an earlier run created (the write was an API call) |
| `database (blob fixture rows)` | the only direct write: blob rows, because this build has no blob upload/attach route at all (follow-up issue) |

The verifier independently checks the API claim: every object version in the
project must be backed by a `state_commits` row whose `via` is `'api'`. A row
written by a script or a fixture loader fails that check.

## Reading a result

The summary lists every check with `required`, `actual` and `status`
(`pass` / `fail` / `unavailable`), plus `build_failures`, `build_refusals` and
`notes`. `unavailable` is not a pass: with `--external 0` the fork-dependent
checks are reported as not checked, by name.

A red is diagnostic on purpose — the failing check names the class and the
shortfall, e.g.

```
FAIL 6  5  calculation (docs/34 §Objects: 至少 6 Calculations)
      missing 1 calculation
```

## Proving the verifier can say no

A verifier that has never failed proves nothing. `tests/acceptance/seeddemo/mutation-check.sh`
drops one object from a **copy** of the plan, builds that copy into its own
database, and requires the verifier to go red naming the class it came up
short of:

```bash
tests/acceptance/seeddemo/mutation-check.sh                    # drop one calculation
SEED_DEMO_MUTATION_KEY=exp-70-02 tests/acceptance/seeddemo/mutation-check.sh
```

It fails the check unless all three hold: the **build completed**, the
**verifier failed**, and the failure **names the mutated object's class** with
the measured shortfall. A build that stops early produces no verdict about the
verifier, and the script says so rather than counting the red as a success. It
refuses a key the rest of the plan still refers to (printing the referring
paths), because dropping that one leaves a dangling reference instead of one
item missing. The mutant build runs with `--external 0` (see the race below);
`SEED_DEMO_MUTATION_EXTERNAL=1` includes the fork path.

## Known limitations

Honest list, each with what it is and where it came from.

- **Blob rows are a database fixture.** This build has no route that uploads or
  attaches a blob (`internal/storage` is a port with no adapter). The product's
  own integrity engine *blocks* a merge whose payload names a blob the proposal
  does not hold, so the seed writes the minimal real row (content hash and size
  computed from the plan's own file content) and labels the path everywhere.
  Follow-up issue.
- **Review routing is written in-process.** No HTTP route exists for the
  Research Owners rules and responsibility assignments, so a pull request whose
  changes no rule routes could never reach `merge_ready`. The seed calls the
  product's own service over the product's own adapters, in-process.
  Follow-up issue.
- **The merge route maps a GateMain refusal to 500.** A merge the product
  refuses at `main`'s gate comes back as `INTERNAL_ERROR` (`RSG_VALIDATION_FAILED`)
  rather than a 4xx carrying the report. Follow-up issue.
- **Signing up an account whose email *and* handle both exist answers 503.**
  `internal/persistence/auth_credential_store.go`'s handle-collision retry path
  returns its second unique violation untranslated, so the client sees
  `SERVICE_UNAVAILABLE` instead of `EMAIL_ALREADY_REGISTERED`. The builder logs
  in first and only signs up when the credentials are unknown, so the demo does
  not depend on the fixed behaviour.
- **`claims` / `findings` projection tables have no writer** in this build. The
  verifier reads the contested finding from the version payload, which is the
  projection's documented source of truth. Follow-up issue.
- **Auth rate limits.** Login is limited to 5 attempts per minute per email
  (per IP: 20 login, 10 signup). Running the demo several times in a row, plus
  manual probing of the API, can hit that; wait for the window rather than
  retrying. The build stops honestly when it does — the summary carries
  `HTTP 429 RATE_LIMITED: too many attempts`, and
  `tests/acceptance/seeddemo/mutation-check.sh` refuses to read the resulting
  red as proof about the verifier (`PROBLEM: the BUILD did not complete (exit
  1), so the verifier proved nothing about the missing …`). Observed once, when
  two mutant runs followed back-to-back runs.
- **A stale queued job can delay the next run's provisioning.** `ops/seed-demo.sh`
  stops the API at the end of every run, and a job that was in flight then stays
  in Redis's processing list; the next start sweeps it back onto the queue, where
  it fails (its project is gone) and the single worker loop **sleeps in place**
  for its backoff (`internal/worker/worker.go`, `process` → `sleep(ctx, retryIn)`),
  blocking every job behind it. The fork path needs the parent project's
  repository to exist, so a run that reaches the fork inside that window gets
  `503 forks unavailable`. Observed once, reproduced from the API log
  (`worker: job failed; scheduling retry` for a `project-provision` job of a
  dropped database, immediately before the 503). Follow-up issue.
- **The existence checks the builder uses are keyed by object pair, not by
  version pair.** A relation is pinned to the versions that were current when
  it was written; asking by version pair on a re-run would answer "missing" for
  a relation that is present, because later stages revise those objects. So a
  re-run recognises *a* relation of that type between those two objects, and
  would not write a second one. The plan declares no such duplicate.
