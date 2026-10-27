# WEB-01 real-UI acceptance harness (G23)

This directory is the independent UI harness for the V2 web acceptance
journeys (`docs/70_WEB_OPERABILITY_V2_PLAN.md` §6, WEB-01, G23). It exists to
make the following claim true and reproducible:

> A first-time visitor can establish an account and sign in **through the
> product's own web page**, the session persists across a reload and a second
> page, and the harness turns red when that primary control stops working.

The journey is the U01 slice: sign-up through the product's own `/login` on
both of its entries (the in-card toggle, and the home's create-account entry
which opens `/login?mode=signup` — the same auth surface, not a second one),
wrong-password recovery, session persistence across a reload and a second
page, return-to-task after sign-in (`?next=`), the signed-in home's
project-workspace entry (it navigates to `/projects`; creating a project
there is WEB-04), and a recoverable prompt when the API no longer accepts the
session the tab had. On Home the recovery link is plain `/login`, because
`/` is the default destination; the journey asserts that measured value.
Later WEB work packages add their own journeys here as the features land; a
journey that is not implemented yet is not added to the blocking run — but
nothing in this harness is ever reported as passed for a feature that is
absent.

## Run it

```bash
make web-v2          # full isolated real stack + Chromium journey
make web-v2-mutation # required proof: disconnect the primary button -> red; restore -> green
make web-v2-unit     # harness self-tests (negative cases, missing service/entry)
```

The commands are equivalent to:

```bash
bash tests/web-v2/run.sh
bash tests/web-v2/mutation-check.sh
bash scripts/tests/web-v2-unit-test.sh
```

Prerequisites: Go, Node, pnpm, a reachable PostgreSQL (the repository dev stack
via `make infra-up`), Redis, and the web workspace dependencies
(`pnpm install --frozen-lockfile`). **A missing prerequisite fails the run with
its name; it never skips.**

## What is real, and what is isolated

| Piece | Real? | Isolation |
|---|---|---|
| PostgreSQL | yes | one fresh database per run, `webv2_t1302_<run id>`, migrated and dropped by the Go harness |
| Redis | yes | production session store and rate limiter on `WEB_V2_REDIS_DB` (default 15); this run's session tokens are revoked on shutdown |
| Go API | yes | the production `cmd/api/authhttp` handlers, session/CSRF guard and CORS allow-list, composed as `cmd/api` composes them |
| Next web | yes | production `next build` + `next start` against the isolated API |
| Browser | yes | real Chromium through Playwright, one context, no service worker or fixture routing |
| Accounts | **created by the browser** | the harness seeds no user; U01's visitor signs up through `/login` |
| Network | observed, never intercepted | requests/responses are recorded as evidence; nothing is stubbed or fulfilled |

The Go harness lives in `harness/main.go` and prints exactly one
`READY {...}` line with the API origin, database and run id. The shell runner
waits for it before building the web app, so no browser is pointed at a
half-started stack.

## Anti-bypass rules

`docs/70` §6.2 forbids performing a business step with `page.request`,
`APIRequestContext`, a test-side `fetch`, SQL, CLI Git, a seeded account,
cookie/storage injection or a faked response. The harness enforces that in
three layers:

1. **Static scan** — `scripts/tests/web-v2-bypass-scan.mjs` reads every
   journey and helper module and rejects the forbidden APIs: `page.request`,
   `context.request`/`BrowserContext.request`, any `APIRequestContext`,
   `fetch`/XHR/axios, storage/cookie injection, request interception, every
   in-page execution spelling (`page.evaluate`, `locator.evaluate`,
   `locator.evaluateAll`, `page.$eval`/`$$eval`, `evaluateHandle`), SQL/CLI,
   and filesystem access outside the evidence helper. Comments are stripped
   first, so prose may name what is forbidden.
2. **Step accounting** — `lib/steps.mjs` declares the journey's steps up
   front. A step that did not run, ran twice, threw, or was never declared
   fails; `assertComplete()` runs at the end. Reaching the bottom of the script
   is not a pass.
3. **Runtime evidence** — the journey observes every page in the browser
   context (not only the first) and records every request/response, console
   message, page error, failed request and 5xx response; any unexpected one
   fails the run. Signed-in checks only accept a `GET /auth/session` 200
   observed after that specific action, so a sign-up-phase response cannot
   satisfy the sign-in assertion. Screenshots per key step and a Playwright
   trace are written under `evidence/<run id>/`, alongside `steps.json` and
   `metadata.json` (commit, runtime versions, run id, role, entry URL, input
   summary, expected/actual, timings). An evidence write failure — including
   a missing or empty `trace.zip` — fails the run rather than degrading to a
   warning. The account password is never recorded.

The static scan is auxiliary, not the proof: the trace and the real-service
step results are the evidence the Supervisor reviews.

## The three required acceptance checks

| Claim | Evidence command | What failure looks like |
|---|---|---|
| Login is performed through the DOM and the session persists | `bash tests/web-v2/run.sh` | any declared step missing/failed; no `POST /auth/signup` or `/auth/login`; session lost after reload |
| Disconnecting a primary button turns the journey red | `bash tests/web-v2/mutation-check.sh` | mutated tree passes (bug), or fails for a reason other than the primary button; restored tree is red (bug) |
| Missing service/account entry fails instead of skipping | `bash scripts/tests/web-v2-unit-test.sh` | any negative case exits 0 or fails for the wrong reason |

The mutation check edits
`apps/web/app/(auth)/login/login-card.tsx` **at runtime only**: it snapshots
the file's current bytes, removes the call from the form's `onSubmit` to
`submit()`, runs the journey, restores the exact snapshot in an
`EXIT`/`INT`/`TERM` trap, verifies the bytes are identical to the pre-run
snapshot, and requires the restored run to pass. A dirty target is expected
and allowed — this runs on the G2 task tree where the diff is uncommitted —
and the proof refuses only when the snapshot itself cannot be taken.

## CI wiring (Supervisor-owned)

`.github/**` and `specs/orchestrator/gates.json` are Supervisor scope; this
task delivers the runnable target, not the job. To make G23 blocking, add a
job on `ubuntu-24.04` with **both** service containers (`pgvector/pgvector`
PostgreSQL and `redis:7.4.11-alpine`), Go/Node/pnpm setup, `pnpm install
--frozen-lockfile`, then:

```yaml
      - name: "web-v2: G23 real-UI journey (isolated PG + Redis + API + Next)"
        env:
          POSTGRES_TEST_ADMIN_URL: postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post
          WEB_V2_REDIS_ADDR: 127.0.0.1:6379
          WEB_V2_WEB_PORT: "31119"
        run: make web-v2
      - name: "web-v2: mutation proof (button disconnect -> red, restored -> green)"
        env:
          POSTGRES_TEST_ADMIN_URL: postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post
          WEB_V2_REDIS_ADDR: 127.0.0.1:6379
        run: make web-v2-mutation
      - name: "web-v2: negative guards (missing service/entry fails)"
        env:
          POSTGRES_TEST_ADMIN_URL: postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post
          WEB_V2_REDIS_ADDR: 127.0.0.1:6379
        run: make web-v2-unit
```

The job must be listed in `specs/orchestrator/gates.json` `required_jobs`
(`web-v2`) and in G2/G4's job sets; otherwise a red harness would not block a
merge. The job name is the Supervisor's to choose, but it must run `make
web-v2` (not a similar-looking subset) so the gate and the harness cannot
drift.

The three steps are separate on purpose: the mutation proof rebuilds the web
app and takes about twice as long as the journey, and the negative guards are
fast. Keeping them as named steps means a red result identifies which claim
failed.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `WEB_V2_POSTGRES_ADMIN_URL` / `POSTGRES_TEST_ADMIN_URL` | `run.sh` supplies the documented local compose DSN; the Go harness has **no** default and fails with a named configuration error when neither is set | admin DSN used to create/drop the run's database |
| `WEB_V2_REDIS_ADDR` / `POST_REDIS_ADDR` | `127.0.0.1:6379` | real Redis |
| `WEB_V2_REDIS_DB` | `15` | session namespace (this run's tokens are revoked; no other keys are flushed) |
| `WEB_V2_API_ADDR` | `127.0.0.1:0` | API listen address (kernel-assigned by default) |
| `WEB_V2_WEB_PORT` | `31119` | web server port (busy port = failure, never reuse) |
| `WEB_V2_EVIDENCE_DIR` | `tests/web-v2/evidence/<run id>` | evidence output |
| `WEB_V2_RUN_ID` | timestamp + random | run identity in the database name and evidence |

## Files

```
tests/web-v2/
  run.sh                 isolated stack + scanner + entry check + journey
  mutation-check.sh      the required red/green proof
  journey.mjs            the U01 browser journey (steps, evidence)
  entrypoint-check.mjs   sign-in form + account-establishment entry pre-flight
  harness/main.go        isolated real API (production auth surface)
  lib/steps.mjs          step accounting (a skipped step fails)
  lib/evidence.mjs       screenshots/network/console/metadata/trace record
  lib/steps.test.mjs     unit tests for the step accounting
  lib/evidence.test.mjs  unit tests for the evidence-write failure guard
  package.json           Playwright pinned; own lockfile, outside pnpm workspace
scripts/tests/
  web-v2-bypass-scan.mjs     static anti-bypass scanner
  web-v2-unit-test.sh        negative guards (this task's third required test)
  web-v2-fixture-server.mjs  static fixture server for the negative cases
  fixtures/web-v2-*.html     complete and missing-entry login fixtures
```
