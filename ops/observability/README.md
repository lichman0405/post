# Observability (docs/26) — metrics, rules, logs, trace

This directory holds the **operational** half of T1109. The code half is in
`internal/observability/` (the exposition) and the two binaries' composition
roots (`cmd/api/metrics.go`, `cmd/worker/metrics.go`); this directory holds
what an operator reads: the alert rules, and the contracts the logs and the
trace ids are held to.

V1 has **no log backend and no monitoring stack** (no Grafana, no
Alertmanager, no Collector — T1109's own boundary). So "dashboards" here means
a *contract* an operator can query against whatever backend they point at the
structured JSON on stderr, plus a rules file Prometheus can load. Neither
half pretends the other exists.

---

## 1. The endpoint

`GET /metrics` in Prometheus text exposition format (OpenMetrics when the
scraper asks for it via `Accept`).

* **cmd/api** serves it on its **main address**, on its own mux so a scrape is
  never counted as product traffic (`cmd/api/main.go:1417-1424`: the comment,
  then `root := http.NewServeMux()` with `GET /metrics` as the route registered
  on it and the product handler behind `/`).
* **cmd/worker** serves it on **`-metrics-addr`**, disabled by default
  (`cmd/worker/main.go:160`). Its counters live in that process's memory only:
  queue-read failures, dead letters and outbox publish failures happen inside
  the worker loop and nowhere else.

**The two processes must be scraped as two targets.** They are not replicas —
`post_queue_errors_total` on the API is zero forever, because the API never
pops the queue. A single-target scrape configuration silently measures half
the platform.

**The tree has a third HTTP process, and it is not one of the two targets.**
`cmd/mcp-server` (deployed as the `mcp:` service, `ops/deploy/docker-compose.staging.yml:229`)
builds an `http.Server` around `/healthz`, `/readyz` and `/mcp`
(`cmd/mcp-server/main.go:73-81`, server at `:83-86`) and runs the same request middleware
(`observability.Middleware`, `cmd/mcp-server/main.go:85`), so the requests it
serves do increment that process's counters — but it opens **no `/metrics`
listener**, so those counters are never exposed, and the scrape configuration
below covers `post-api` and `post-worker` only. Nothing in `alerts.yml`
depends on it: its one product route answers `501` today ("MCP protocol wiring
not implemented yet", `cmd/mcp-server/main.go:79-80`). Instrumenting it is
recorded as follow-up work rather than claimed here — a third target would
also need its own family-to-target row in §6.3, and this task's deliverable is
the two product processes.

### Where it is reachable from

`ops/deploy/reverse-proxy/nginx.conf` proxies `/api/`, `/healthz`, `/readyz`
and `/` — there is **no `location /metrics`** (verified:
`grep -cE '^\s*location' ops/deploy/reverse-proxy/nginx.conf` → 8, with
`grep -n location` showing all eight: acme-challenge, `/`, `/api/`,
`/healthz`, `/readyz`, `/`, `/healthz-proxy`, `/` — none of them metrics).
So the endpoint is **not on the public edge**: it is reachable from
the host network the process runs on, which is where Prometheus must sit.
That is deliberate — the exposition carries route patterns and refusal counts
that describe the platform's internals, and nothing in this task's scope
decided that an unauthenticated public metrics endpoint was acceptable. Adding
a proxy location for it is a deployment decision (it changes what the internet
can reach) and is left to whoever owns the edge, with this note as the
evidence that it was a decision rather than an oversight.

### Families, and which docs/26 §3 item each one is

§3 names eleven items. Nine have a source today; the two that do not are in §3
below, not quietly omitted.

| family | docs/26 §3 item | source |
| --- | --- | --- |
| `post_http_requests_total` | HTTP error | `internal/observability/middleware.go:59` |
| `post_http_request_duration_seconds` | HTTP latency | `internal/observability/middleware.go:59` |
| `post_db_pool_connections` | DB pool/query | `cmd/api/metrics.go:40` (scrape-time) |
| `post_db_pool_max_connections` | DB pool/query | `cmd/api/metrics.go:56` (scrape-time) |
| `post_db_pool_acquires_total` | DB pool/query | `cmd/api/metrics.go:76` (scrape-time) |
| `post_db_up` | DB pool/query | `cmd/api/metrics.go:97`, `cmd/worker/metrics.go:69` (scrape-time) |
| `post_queue_depth` | queue depth/failures | `cmd/api/metrics.go:117`, `cmd/worker/metrics.go:26` (scrape-time) |
| `post_queue_errors_total` | queue depth/failures | `internal/worker/worker.go:135` |
| `post_queue_jobs_total` | queue depth/failures | `internal/worker/worker.go:174,194,204,208` |
| `post_outbox_publish_failures_total` | outbox lag | `internal/events/publish.go:194` |
| `post_outbox_pending_events` | outbox lag | `cmd/worker/metrics.go:37` (scrape-time) |
| `post_outbox_oldest_pending_seconds` | outbox lag | `cmd/worker/metrics.go:50` (scrape-time) |
| `post_webhook_deliveries_total` | webhook success | `internal/events/deliver.go:183` |
| `post_search_query_duration_seconds` | search latency | `internal/search/retrieval/retrieval.go:385` |
| `post_planner_runs_total` | LLM planner failures | `internal/search/planner/planner.go:190,202` |
| `post_planner_fallbacks_total` | LLM planner failures | `internal/search/planner/planner.go:202` |
| `post_rsg_reconciliation_passes_total` | RSG reconciliation drift | `cmd/api/reconciliation.go:71` |
| `post_rsg_reconciliation_findings_opened_total` | RSG reconciliation drift | `cmd/api/reconciliation.go:71` |
| `post_rsg_reconciliation_findings_resolved_total` | RSG reconciliation drift | `cmd/api/reconciliation.go:71` |
| `post_rsg_reconciliation_open_findings` | RSG reconciliation drift | `cmd/api/reconciliation.go:71` |
| `post_rsg_reconciliation_refs_checked_total` | RSG reconciliation drift | `cmd/api/reconciliation.go:71` |
| `post_permission_denials_total` | permission denied rates | `cmd/api/authhttp/envelope.go:72` (surface `api`), `internal/events/inbox_store.go:235` (surface `inbox`), `internal/gitprovider/push_ingestion_http.go:83,115,150` (surface `gitprovider`) |
| `post_metrics_collector_errors_total` | **not a §3 item** — this task's own self-observation. See §2. | `internal/observability/collect.go:45` |

Every line number above was checked by grepping the file it names, not from
memory: each one is the line the family's name or the observing call actually
sits on (`grep -n '"post_queue_depth"' cmd/api/metrics.go`), so a reader
verifying one of these sees the thing claimed rather than the line above it.

No family is emitted by more than one call site *for the same meaning*, and no
label value is ever a user-supplied string: statuses are codes the server
chose, routes are Go 1.22+ ServeMux patterns (`http.Request.Pattern`, never
`r.URL.Path` — a project id in a label is unbounded cardinality **and** an
endpoint that answers "does this id exist"), planner causes are
`planner.Reason`'s closed set, and the one label whose input arrives from the
wire — `method`, i.e. `http.Request.Method` — is folded by
`httpMethodLabel` (`internal/observability/metrics.go`) onto the nine methods
`net/http` names plus a single `OTHER` bucket. Before that fold existed this
paragraph was false: `method` was passed through verbatim, so a client sending
`FOO /x HTTP/1.1` with a fresh token per request grew both HTTP families
without bound. `TestHTTPMethodLabelIsAClosedSet` asserts the closed set over a
real socket; the guarantee is stated here because that is where it was claimed
wrongly.

### Why `post_metrics_collector_errors_total` exists

The scrape-time families read their value from PostgreSQL or Redis at scrape
time. When that read fails there is no honest value to publish: emitting `0`
would report "no backlog" during exactly the outage that probably created one.
The collectors therefore emit **no sample** on error.

That is the right call and it has a cost: "the collector could not look" and
"the quantity is zero" are indistinguishable in the exposition. This family is
what pays it — it counts the failed reads, by collector name, so an operator
sees the difference. It is deliberately **not** in §3 because §3 does not ask
for it; it is here because without it the rest of §3 would be lying.

The same rule is applied one level up, to `post_rsg_reconciliation_open_findings`:
a reconciliation pass that fails records `outcome="failed"` **and leaves the
gauge at its last real value**, rather than setting it to 0 and silently
clearing the P1 drift alert during the outage most likely to have caused the
drift (`internal/observability/metrics.go:435`, `ObserveReconciliationFailure`
— the function that deliberately does not touch the gauge).

One measured caveat on this family: **the count appears in the following
scrape, not the one that failed.** `client_golang` gathers a registry through a
worker pool (its `collectWorker`s pull collectors from a channel), so the
failing collector and the error counter are read concurrently and this scrape's
increment may land after the counter was read. Asserting the same-scrape
visibility failed five runs in five when this was first written; asserting it by
the following scrape has since held for thirty consecutive runs under `-race`.
The lag is at most one scrape interval (15s here), which is invisible to a
`rate()` over ten minutes. It is recorded because the difference between "the
number is late" and "the number is missing" is the whole point of the family.

### When a series is absent, what a rule sees

Measured, not assumed (this surprised the harness that found it):

> Prometheus carries a series' **last sample forward for the lookback delta**
> (5m by default) before treating it as gone, and a series merely missing from
> a scrape *body* is never marked stale — only a failed *scrape* is.

So a collector that stops emitting (because its source went away) does **not**
clear its alert immediately; the last value keeps it firing for up to five
minutes. For the backlog rule that is the right direction — the backlog is
very probably still there — but it has a consequence worth stating in the
alert's own comment: **that alert going quiet is not evidence the backlog
cleared.** Its silence is only trustworthy with `post_db_up 1` beside it.

---

## 2. The alert rules: `alerts.yml`

Prometheus `groups`/`rules` shape. Validate and test it with Prometheus's own
tool, never with a re-implementation:

```sh
PROMTOOL="$(bash tests/observability/promtool.sh)"
"$PROMTOOL" check rules ops/observability/alerts.yml
"$PROMTOOL" test  rules tests/observability/<generated>.yml
```

`tests/observability/promtool.sh` fetches a **pinned** promtool (3.14.0) into
`/tmp/post-promtool` and verifies it against the release's published SHA-256.
It is deliberately **not** a Go module dependency: the rules are consumed by a
Prometheus server, which is not a Go module of this repository, and pulling a
200-package module graph into `go.mod` to check three YAML files would put a
build dependency on every `go build`. Same reasoning as
`tests/web-smoke/run.sh` installing Chromium.

| alert | class (docs/26 §6) | expression | why this threshold |
| --- | --- | --- | --- |
| `PostDatabaseUnavailable` | P1 database unavailable | `post_db_up == 0` | `post_db_up` is a scrape-time `ping`, so 0 is the database answering nothing. `for: 1m` = four scrapes at the default 15s. |
| `PostRSGGitDrift` | P1 RSG/Git drift | `post_rsg_reconciliation_open_findings > 0` | `cmd/api/reconciliation.go:29` says any drift is high severity; the gauge counts what a completed pass found open. `for: 5m` = exactly one `reconciliationInterval`, so a drift cannot fire on the pass that found it before a second pass confirms it is still open. |
| `PostMetricsTargetMissing` | P1 (blind ≠ healthy) | `up{job=~"post-api\|post-worker"} == 0 or absent(up{job="post-api"}) or absent(up{job="post-worker"})` | **One** target gone, which the rule below cannot see: `post_db_up` is emitted by both binaries, so `absent(post_db_up)` needs both of them dead. Two branches because a failed scrape leaves `up` at 0 (per instance) while a job deleted from the config leaves no series at all. `for: 5m`, the same hold as the rule below and deliberately not shorter. **Assumes the scrape jobs are named `post-api` / `post-worker`** — see §6.3, which ships that configuration and the table of what each target feeds. |
| `PostMetricsEndpointMissing` | P1 (blind ≠ healthy) | `absent(post_db_up)` | The total-blackout backstop: neither process reporting. Kept beside the rule above rather than replaced by it, because it reads a series POST emits and therefore depends on no scrape-configuration naming — if the jobs are ever renamed, this is the one that still answers. `for: 5m` is beyond any scrape blip. |
| — **no rule** — | P1 blob integrity mismatch | 本版无规则，因为无指标源 | see §3 |
| — **no rule** — | P1 data visibility leak suspicion | 本版无规则，因为无指标源 | see §3 |
| `PostOutboxBacklog` | P2 event/outbox backlog | `post_outbox_oldest_pending_seconds > 300` | The **age**, not the count: a deep outbox is normal during a burst and drains itself; only the oldest row's age grows when the publisher is not moving. 300s against `DefaultPollInterval = 1s` (`internal/events/publish.go:48`) is 300 skipped cycles. |
| `PostOutboxPublishFailing` | P2 event/outbox backlog | `increase(post_outbox_publish_failures_total[10m]) >= 3` | The ladder's own rhythm: the failure path backs off 1s→30s, so three failures land within ~10s of an outage starting, while a single transient failure (fixed by the next pass) does not reach 3. |
| `PostSearchUnavailable` | P2 search unavailable | error ratio of the search histogram > 10% over 10m | A ratio, not a count — one failed query in a quiet period is not an outage and an absolute count cannot tell the two apart. |
| `PostSearchLatencySlow` | (docs/26 §3 "search latency", no §6 class) | p99 of `outcome="ok"` > 2s | p99, not mean: search is interactive and the tail is what users notice. Errored queries are excluded — fast failures would *lower* the mean. 2s is the one threshold here that is a product judgement rather than derived from a cadence constant; it is marked as such in the file so it can be renegotiated with evidence. |
| `PostWebhookFailuresSurge` | P2 webhook failures surge | failing share of attempts > 20% over 10m | Numerator is exactly the two outcomes `internal/events/deliver.go` classifies as failures (`terminal`, `retry`); `retry_no_streak` is excluded because a 1xx/3xx is not evidence the endpoint is failing. |
| `PostJobQueueUnavailable` | (docs/26 §3 queue failures) | `increase(post_queue_errors_total{operation="read"}[5m]) >= 2` | The worker's read path is a blocking pop in a loop: a read error means Redis is unreachable and *every* job stalls — there is no partial version. `>= 2` so a single reconnect blip does not page. |
| `PostJobDeadLettered` | (docs/26 §3 queue failures) | `increase(post_queue_jobs_total{outcome="dead_lettered"}[15m]) > 0` | No threshold and **no `for:`**: the state is already terminal, so waiting only delays the page. |
| `PostPermissionDenialsElevated` | (docs/26 §3 permission denied rates) | `sum(rate(post_permission_denials_total{decision="forbidden"}[10m])) > 1` | Only `decision="forbidden"`: `unauthenticated` (401) is a steady hum from expired browser sessions, and `filtered` is the platform working correctly. 1/s sustained is not human use. **Known limit, stated in the file**: a slow low-rate probe stays under it, and this is the one rule whose number has no cadence constant to derive it from. |

### Holds, and the one rule that broke the pattern

Most rules here are **windowed** expressions (`rate`, `increase`, a ratio). For
those, the window does the smoothing and `for:` only confirms — so the hold is
short (2m/5m) and **never as long as its own window**. Stacking a 10-minute
hold on a 10-minute rate window puts detection ~20 minutes out, which is
longer than most of the outages worth reporting on.

`PostOutboxBacklog` is the exception and is commented as such: a bare gauge
comparison has no window, but its *threshold* is not instantaneous either —
`300s` of an undrained row is 300 dispatcher cycles that did not happen, which
no blip can produce. The hold there only has to establish that the reading is
repeatable.

### Why the ratio rules use `clamp_min(..., 0.001)`

`PostWebhookFailuresSurge` divides by total attempts. With no traffic the
denominator is 0 and the expression is empty — *not* "no failures", but "no
opinion", and an empty expression cannot fire. `clamp_min` keeps the rule
evaluable so the failure mode is a quiet rule rather than a NaN.

---

## 3. Two classes with no metric source — searched, not assumed

Both are P1 in docs/26 §6, and **this version writes no rule for either**.
A rule that can never fire is worse than a missing rule: it makes the alert
list look complete while measuring nothing, and it teaches whoever reads the
list that these classes are covered.

| class | search performed | result |
| --- | --- | --- |
| **blob integrity mismatch** | `ls internal/storage/` → one file, `doc.go`. `grep -rn 'CreateBlob' --include='*.go' .` → five lines, three files, and no product call site: the sqlc-generated query, its params struct and its method (`internal/persistence/sqlc/blobs.sql.go:42,49,60`), the interface method (`internal/persistence/sqlc/querier.go:121`), and **one caller in the whole tree, a test** (`tests/integration/manifest_test.go:178`). `grep -rn 'PutObject' --include='*.go' cmd/ internal/` → only `cmd/api/backupdr/`, the backup/restore drill, which is not a product path. | **No product code reads, writes or hashes blob bytes.** The asset preview (`cmd/api/assetshttp`) reads blob *metadata rows* to say who would see what; nothing opens the object, and there is no upload route to hang a counter on. The one blob-hash comparison in the tree is the drill's (`cmd/api/backupdr/reconcile.go:521`) — see below. |
| **data visibility leak suspicion** | Traced every place a permission verdict is consumed: `cmd/api/authhttp/envelope.go:72` (the boundary, where a refusal becomes a 403/401), `internal/gitprovider/push_ingestion_http.go:83,115,150` (the one refusal site outside the guard — the webhook receiver, which writes its 401/403 directly) and `internal/events/inbox_store.go:235` (the silent-filter tier, where the decision removes rows from a 200). `grep -rn 'DecisionFiltered' --include='*.go' cmd/ internal/` returns four lines, and they are all accounted for: the constant's declaration and its doc comment (`internal/observability/metrics.go:329,338`) and **one** call site with its comment (`internal/events/inbox_store.go:220,235`) — no other producer. | **No rule — but not because the tier is uncounted, which would be false.** The silent-filter tier IS counted: `internal/events/inbox_store.go:235` records `decision="filtered"`, one increment per READ whose target list the filter shortened (not per hidden row — see the constant's own comment). What that counter measures is the platform **working**: its rate tracks event volume and how much restricted material is in it, so a rule on it would page whenever a project with restricted visibility had traffic, which is why `PostPermissionDenialsElevated` keys on `decision="forbidden"` alone. What it does **not** measure is anything about who saw what, and that gap is what leaves the class unruly: a denied read answers `404` on purpose ("Read existence hiding"), so a refusal and a genuine miss are the same status and the same body — and `ObserveRefusal` deliberately does not count 404 as a denial at all; and the filter's counter records that rows were withheld, never which rows, from whom, or whether the caller was reaching for something they had no entitlement to. Turning either into a leak signal needs a definition of what "suspicious" looks like — a product/security judgement, not this task's. The direction is worth stating too: `post_permission_denials_total` counts who was **told no**, which is the opposite of who saw too much. |

The rules file states both verbatim (`本版无规则，因为无指标源`) next to the two
P1 rules that do exist, so the omission is visible where an operator looks for
alerts.

**`Git ingestion lag`** (a §3 item with no §6 class) is in the same position:

```
grep -rniE 'ingest[a-z]*[_ ](lag|delay|behind)|lag[_ ]?seconds|last_ingest' \
  --include='*.go' --include='*.sql' cmd/ internal/ infra/
internal/observability/metrics.go:44:// this tree: "Git ingestion lag" and "blob upload/failure". Their absence is
internal/gitprovider/push_ingestion.go:362:// having moved with no ingestion behind it — `ref_head_moved` drift the
```

Two hits, both prose, neither a source: the first is this task's own comment
explaining the absence, the second is the ingestion path's comment about
`ref_head_moved` drift. Ingestion is event-driven at the webhook
(`internal/gitprovider/push_ingestion*.go`): there is no queue holding
unprocessed pushes, and no timestamp recorded for "when this ref should have
been ingested by". A lag needs a promised-by time to be late against; there is
none, so there is no rule.

§3's **`blob upload/failure`** likewise. Its search is the table above plus:

```
grep -rniE 'upload[_ ]?(fail|error)|blob[_ ]?(fail|error)|integrity|checksum' \
  --include='*.go' cmd/ internal/ | grep -v _test.go | grep -v backupdr
```

which returns **376 lines across 103 files** — the word dominates the scientific
review engine (`internal/rsg/integrity`, `internal/rsg/validation`, unrelated to
stored bytes) and `assetshttp`'s `integrity_hash` field, which is carried in a
manifest and never compared against an object on this path. Of the 376, exactly
one compares a stored blob's bytes against a recorded hash, and it is the drill
below. The only
comparison of a stored blob against a recorded hash in the tree is
`cmd/api/backupdr/reconcile.go:521`, inside the backup/restore drill — a drill
path, not a product path. There is no upload route that could fail and no
verification step whose failures a product metric could count.

---

## 4. The log field contract

The logs are structured JSON on stderr (`cmd/api/main.go:185`,
`slog.New(slog.NewJSONHandler(os.Stderr, nil))`; cmd/worker's own copy is at
`cmd/worker/main.go:181`) and there is
**no log backend in V1**. So a "log dashboard" can only be a contract: which
fields are guaranteed present on which events, so that whatever backend is
pointed at them has something dependable to query. Guaranteed means *the code
puts it there*, not *it is usually there*.

### 4.1 The envelope

`correlation_id` (docs/26 §2) is the field every panel groups on, and it is
attached in three places:

| attached at | how | so it is present on |
| --- | --- | --- |
| the HTTP edge | `internal/observability/middleware.go:36` puts it on the request-scoped logger | every line logged through `observability.Logger(ctx)`, including **`http request completed`** |
| job dequeue | `internal/worker/worker.go:149` puts it on the job-scoped logger | every line inside `Loop.process`, including **`worker: job completed`** and the failure lines |
| nothing | the outbox dispatcher's logger is the **process** logger | the `events: outbox …` lines carry no `correlation_id` — they are loop passes, not requests |

That last row is a real gap, stated rather than hidden: an outbox publish
failure tells you *which row* (`outbox_id`) but not which request created it.
Following it further means reading `outbox_events.correlation_id` for that row
— the write path does record it (`internal/events/event.go:181`, the
`insertOutboxEvent` statement whose sixth argument is the correlation id; the
`prepare` function above it falls back to the request's id and, failing that,
to a fresh one, so the column is never empty).

An incoming `X-Correlation-ID` is honoured only if it matches
`^[A-Za-z0-9][A-Za-z0-9._-]{7,63}$` (`internal/observability/correlation.go:27`);
anything else is replaced with a fresh id, so an untrusted header can never
reach a log line.

### 4.2 Guaranteed fields on the four events a panel would query

| event (`msg`) | guaranteed fields | panel that queries it |
| --- | --- | --- |
| `http request completed` | `correlation_id`, `method`, `path`, `status`, `duration` | error-rate by status; slow-request list. `path` is `r.URL.Path` — the **query string is never logged** (it is a classic credential carrier). |
| `api: job enqueued` / `api: job enqueue failed` | `correlation_id`, `job_id`, `job_type`; `error` on the failure | "did this request's work reach the queue?" — and the failure line is where a 503 is explained. **Only when the request reaches the handler**: if the edge's own Redis is down, the limiter refuses the request first and the line that explains that 503 is `security: rate limiter failed; request refused` (see §6.1). Both carry `correlation_id`, so the panel is a search on the id either way. |
| `worker: job completed` | `correlation_id`, `job_id`, `job_type`, `attempt` | the other end of the same question. Tracing one request = grepping these two lines for one id. |
| `worker: queue read failed`, `worker: job failed; scheduling retry`, `worker: job failed permanently; dead-lettering` | `error`, plus the job fields the logger already carries | queue-outage and lost-work panels. |

`error` values pass through `config.RedactForOutput` before they are logged or
returned (`cmd/api/jobs.go:77`), so a DSN cannot leak into a log line.

### 4.3 Verifying the contract against the code

The contract is only worth anything if it matches what the code emits. The
check is mechanical — every field named above exists as a literal in a `slog`
call site:

```sh
# the four events and their fields
grep -rn '"http request completed"' -A 6 internal/observability/middleware.go
grep -rn '"api: job enqueued"\|"api: job enqueue failed"' -A 3 cmd/api/jobs.go
grep -rn 'log := l.log.With' -A 2 internal/worker/worker.go
grep -rn '"worker: job completed"' -B 2 internal/worker/worker.go

# and the whole event vocabulary, from production code only
grep -rhoE '\.(Info|Warn|Error|Debug)\("[^"]+"' --include='*.go' cmd/ internal/ \
  --exclude='*_test.go' | sed 's/.*("//' | sort -u | wc -l
```

If a field is renamed in code and not here, this section is what a reviewer
compares against; the trace harness (§5) fails outright if `correlation_id`
stops being carried from the API into the job it enqueued.

---

## 5. The trace contract: one id, the whole way

docs/26 §2 asks for `request_id`/`correlation_id`/`actor_id`/`project_id` on
each request, command, job and event, and for the trace to survive
API → queue → Git/S3. V1 delivers the first; the evidence is a runnable
harness, not a claim:

```sh
make observability-smoke        # or: bash tests/observability/trace-e2e.sh
```

`tests/observability/trace-e2e.sh` builds real binaries, starts them with
`setsid`, drives real requests and greps the **real log files** for four
things:

1. an id created at the API edge is echoed to the caller and reaches the job
   the same request enqueued — the same id appears in `api.log` and
   `worker.log` on the two ends of the queue;
2. a valid incoming `X-Correlation-ID` is honoured across that path rather
   than replaced;
3. with Redis down, the failure is still correlated: the enqueue failure line
   carries the request's id, and go-redis's own chatter arrives through the
   structured logger rather than raw;
4. the web app propagates the id it received to **both** the Go API and the
   Python scientific adapter, and renders it on the page.

It is wired into `make observability-smoke` (before T1109 it was called by
nothing: not the Makefile, not `scripts/ci.sh`, not CI).

---

## 6. Running it

```sh
make observability-smoke
```

Two harnesses, both of which build real binaries and read real output; neither
uses a mock.

* `tests/observability/metrics-alerts-e2e.sh` — starts a real PostgreSQL
  (the same `pgvector/pgvector:0.8.6-pg16` image `docker-compose.yml` uses,
  with the real migrations applied), real `cmd/api`, real `cmd/worker` and the
  in-repo `cmd/devredis`; scrapes **both** `/metrics` endpoints on a fixed 15s
  grid; breaks the platform on purpose (kills devredis, stops PostgreSQL);
  asserts the counters moved from the raw scrape text; then feeds **those
  really-captured samples** to `promtool test rules` so "the fault moved the
  metric" and "the rule fires" are one chain over one set of samples. The
  harness prints which rules were proven on real samples and which were not.
* `tests/observability/trace-e2e.sh` — §5.

Wall clock, measured: **7m26s** for the whole target on the development machine
with warm caches, twice on 2026-09-21 (09:43:01Z → 09:50:28Z and 09:51:00Z →
09:58:26Z, `time make observability-smoke`: 7m26.82s and 7m26.79s — the two runs
agree to 0.03s). Its two halves, each timed on its own in an earlier session:
the metrics harness 7m10s, the trace harness 16s — those add to
the 7m26s above to within a second, so the split is where the time is rather than
a budget. The metrics harness's share is its timeline: 29 scrapes on a 15s grid,
because `for:` is measured in real time and cannot be compressed without changing
what is being measured. The trace harness's one-time cost is `next build`: budget
~4 minutes for it on a cold cache, and take the numbers above as the floor rather
than the expectation.

Needs: Docker (the metrics harness stops a real PostgreSQL), the `pnpm`
toolchain and `uv` (the trace harness's web and adapter phases). It is
deliberately **not** part of `make check` or `make test`, which are documented
as infrastructure-free.

### 6.1 The fault window: what it shows, and what the edge now hides

This subsection exists because the harness's 5xx evidence changed under it
when T1106 landed (`internal/security/ratelimit.go`), and the change is a
property of the platform rather than a preference of the harness. It is
stated here so the next reader does not rediscover it from a red gate.

The edge chain is, outermost first, `security.Headers` →
`observability.Middleware` → `security.RateLimit` → product mux
(`cmd/api/main.go:1412-1414`, where `handler` is composed in that nesting).
Two consequences follow directly from that order:

* the limiter runs **inside** the metrics middleware, so its refusals are
  counted like any other response — the numbers below are real;
* when the limiter's backing store (Redis) is unreachable it **fails
  closed**: 503 `SERVICE_UNAVAILABLE`, written before `next.ServeHTTP`
  (`internal/security/ratelimit.go:138-150`). Every path except the two in
  `security.ExemptProbePaths()` is therefore answered by the limiter while
  Redis is gone and **never reaches the mux**.

So during a Redis outage the product handlers' own 5xx **cannot happen**,
and no assertion about them can be honest. What the harness asserts instead,
each labelled with the route it lands on:

| asserted | route label | what it really is |
| --- | --- | --- |
| the readiness probe answered 503 | `/readyz` | a real 5xx **on a real product route**: the handler is limiter-exempt, so it runs, probes PostgreSQL, finds it down and answers 503 (`internal/health/health.go:105-137`). This is the product-path 5xx the request counter can still see. |
| the edge's fail-closed refusals were counted | `/` | the limiter's own 503, on the root mux's catch-all pattern — the whole-tree refusal described above. It is counted with `route="/"` because `http.Request.Pattern` is the root mux's, the limiter having refused before any nested mux dispatched. |
| the enqueue route pattern reached the counter | `POST /internal/jobs` | asserted healthy-to-healthy: the same request under the fault never reaches the mux, so this is the only window in which the ServeMux pattern (not the raw path, see §1) can be observed on that route. |
| no 429 anywhere in the capture | — | the negative control on the load itself, see below. |

**The uncovered surface, named rather than hidden:** a Redis outage is
observable here as *edge refusals plus an unready probe*, not as a product
handler's error response. An operator whose alerting is built on
`post_http_requests_total{route=~"/api/.*",status=~"5.."}` will see nothing
during that outage — correctly, because nothing on those routes failed —
and should alert on the `route="/"` 503 rate, on `post_db_up`, and on the
queue families (`post_queue_errors_total`, `post_queue_depth`) instead.
Whether the edge should also **count** its fail-closed refusals in a family
of its own (rather than only in the request counter's `route="/"` series) is
a product decision this task does not own; it is recorded in the RESULT as a
follow-up. There is deliberately **no `post_redis_up`**: the edge only
reports Redis through the refusals above and through the worker's queue
families, which is why the rule set has no Redis-liveness alert.

The refusal generator changed for the same reason, and the change is the
opposite of a weakening. The 403s the denial rule counts used to be driven
through `POST /api/v1/auth/login` with a foreign `Origin` — the pre-session
`checkOrigin` refusal. T1106 classifies that route `credential` with a
20/min budget (`internal/security/config.go:37`), so one IP can put at most
20 refusals in a minute: 0.33/s against a rule threshold of 1/s, unreachable
however hard the harness drives it. The harness now creates one real account,
keeps its session cookie, and POSTs a session-carrying write with no
`X-CSRF-Token` — the guard's `checkCSRF` refusal, 403 `CSRF_FAILED`, in the
`authenticated` class whose budget is 1200/min
(`internal/security/config.go:32`). It is a refusal the platform really
makes, to a real session, and the harness refuses to proceed unless a probe
confirms it: a 201 signup, a `post_session` cookie, and exactly one 403
`CSRF_FAILED` before the flood starts. The load stays at 400/min against the
1200/min budget, and "no 429 in any sample" is asserted — so the refusals
counted are the guard's, not the limiter's.

That faster flood then changed what the *promtool* section may honestly
claim, and the assertion was made more precise rather than weaker. On the
new load the counter crosses the denial rule's 1/s threshold at t=90s, so at
the last healthy sample (t=135s) the rule is **pending**, not quiet: the
expression is true and its own `for: 2m` is what holds it back until t=210s.
`ALERTS{alertname="PostPermissionDenialsElevated"}` with no samples — the old
"not firing" control — asserts that the series is in *no* state, which is a
claim about the traffic shape rather than about the rule. The rule is now
asserted in three states over the same samples: no series at all at t=75s
(the condition genuinely false — the always-firing control), **pending** at
t=135s, **firing** at t=330s. `tests/observability/gen-rule-tests.py` takes
`:0`/`:1`/`:2` for that, and `:0` still uses the alertname-only selector, so a
pending state is never silently tolerated where a test did not ask for it.
The generator also prints the census of what it wrote — for the 2026-09-21 run,
`15 assertions: 6 firing, 8 not-firing (exp_samples: []), 1 pending` — counted
from the same list the assertions are written from, so the three states stay
three numbers instead of being rolled into one a reader has to re-count. The
three are different claims and a reader checking one of them needs the number
that belongs to it: the pending assertion names the `pending` selector
explicitly, and the not-firing ones are the controls a rule that always fires
fails.

**The same consequence reaches the trace harness.** `trace-e2e.sh`'s Phase 3
("Redis down") used to assert the enqueue handler's own failure line,
`"msg":"api: job enqueue failed"`, carrying the caller's correlation id. That
line is now unreachable for the same reason: with Redis gone the request is
refused at the edge and the handler never runs. The phase asserts the same
promise against the line that does exist — the refusal is logged with the
caller's id and names its cause, the completion line records 503 under that
id, and the error body returns `request_id` equal to it — so a caller that
loses a request to a Redis outage can still find and quote its trace id. What
is lost is the *handler's* own explanation of a Redis failure, which is
covered by `cmd/api`'s unit tests rather than by the end-to-end run.

### 6.2 CI wiring

Not done by this task: `.github/workflows/ci.yml` is the Supervisor's file, and
the job count in it is asserted by a test outside this task's write scope
(`internal/devorchestrator/gate_spec_test.go:30` — "ci.yml declares %d jobs,
want 7"). Adding a job without updating that assertion reddens the `go` job.
This is the job as it would be pasted, and it is the whole wiring:

```yaml
  observability:
    runs-on: ubuntu-latest
    needs: [go]
    timeout-minutes: 30
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - uses: pnpm/action-setup@v4
      - uses: astral-sh/setup-uv@v5
      - run: make observability-smoke
```

Cost, measured: **7m26s** for the target on this machine with warm caches (§6).
The part that cannot be shortened is the metrics harness's 29-scrape 15s grid —
`for:` is measured in real time. On a cold GitHub runner add the Go build, the
image pulls and a cold `next build`; 30 minutes is the honest ceiling, not 15.

Two things CI needs that `make check` does not: a Docker daemon, and network
access to `github.com/prometheus/prometheus/releases` for `promtool`
(`tests/observability/promtool.sh`, version pinned, archive verified against
the release's own SHA-256). The 120 MB download is cached in `/tmp` and is not
a repository dependency on purpose: validation of three YAML files must not put
Prometheus's module graph into every `go build` in this repository.

### 6.3 Deploying it: the scrape configuration, and which target feeds which rule

§1 states the requirement (the two processes must be scraped as **two targets**)
and until now this README never wrote the configuration down. Here it is,
pasteable as-is into a Prometheus `scrape_configs:` list. The two job names are
not decorative: `PostMetricsTargetMissing` matches them by name.

```yaml
scrape_configs:
  - job_name: post-api
    metrics_path: /metrics
    static_configs:
      - targets: ["post-api:8080"]      # cmd/api's main address (POST_API_ADDR, default :8080 —
                                        # /metrics is served on it, on its own mux, §1)
  - job_name: post-worker
    metrics_path: /metrics
    static_configs:
      - targets: ["post-worker:9091"]   # cmd/worker's -metrics-addr; NO default — the flag is
                                        # what opens the listener (cmd/worker/main.go:294)
```

The worker's port is the deployment's to choose; 9091 is an example. The
harness picks a free port on 127.0.0.1 for the same flag
(`tests/observability/metrics-alerts-e2e.sh:302`). Three things about this
configuration are not obvious and each one costs something real:

* **The worker's endpoint is closed until `-metrics-addr` is passed.** The
  flag's default is the empty string (`cmd/worker/main.go:160`) and the
  listener is only started when it is non-empty (`cmd/worker/main.go:294`).
  A deployment that does not pass it publishes nothing for the worker, and
  that is not a small loss — see the table below.
* **The job names are load-bearing for exactly one rule.**
  `PostMetricsTargetMissing` fires on `up{job="post-api"}` /
  `up{job="post-worker"}`; under different job names it is inert, and the
  naming-independent backstop (`PostMetricsEndpointMissing`) is all that
  remains. That is a deliberate trade, stated in the rule's own comment: PromQL
  has no "any target that should exist", only named targets, so a rule that
  sees one target die must name the targets.
* **A one-target scrape configuration makes `PostMetricsTargetMissing` fire
  permanently.** Its `absent()` branch is true for the job you never
  configured. That is the rule telling the truth about a half-configured
  scraper, but it is a page, so a deployment that genuinely wants one target
  must delete the other job's two branches from the rule rather than leave it
  to howl.

#### What the shipped staging stack does today

`ops/deploy/docker-compose.staging.yml` is the one deployment file in this
tree, and as it stands it exposes **one** scrapeable target, not two:

* its `worker:` service (line 204) passes no `command:`, so the process runs
  with the flag's default and its metrics listener never opens — verified by
  `grep -n 'command:' ops/deploy/docker-compose.staging.yml`, whose only hits
  are the migrate job, redis and minio;
* its `api:` service does serve `/metrics` on its main address, but the file
  publishes exactly one port — the TLS proxy's (`ports:` appears once, at line
  320) — so the API's endpoint is reachable from inside the stack's network
  and from nowhere else.

Turning the worker's endpoint on there is one line in the worker service,
`command: ["-metrics-addr", ":9091"]`, and **this task does not make that
change**: no Dockerfile exists anywhere in this repository (the compose says
so itself at the top), so the shape of that image's entrypoint — whether extra
`command:` words reach the worker binary as arguments — cannot be checked from
here, and a deployment file that cannot be parsed, let alone run, is not
something to edit on an unverifiable assumption. Whoever builds the images
owns that line and the scrape job beside it; §6.2's CI job is the model for
how it should be verified once they exist.

#### Which rules the shipped configuration costs

Every row below is a rule in `alerts.yml`, the family it reads, and which
process actually **moves** that family. "Moves" is the word, not "carries":
several families are plain counters and gauges, so both endpoints emit them —
at a constant 0 from the process that does not produce them. That distinction
is the whole reason five rules can be dead while their series are present.

| rule | family it reads | which target feeds it | under the shipped stack (API only) | with both targets |
| --- | --- | --- | --- | --- |
| `PostDatabaseUnavailable` | `post_db_up` | both (`cmd/api/metrics.go:97`, `cmd/worker/metrics.go:69`) | can fire | can fire |
| `PostRSGGitDrift` | `post_rsg_reconciliation_open_findings` | api — the sweep's only call site is `cmd/api/reconciliation.go:71`, which runs in cmd/api | can fire | can fire |
| `PostMetricsTargetMissing` | `up` | Prometheus itself, one series per configured target | **fires continuously** (the worker job is not configured, so its `absent()` branch is permanently true) | can fire, quiet while both report |
| `PostMetricsEndpointMissing` | `absent(post_db_up)` | both — needs *both* copies gone | can fire, but only if the API is gone too; a dead worker alone is invisible | can fire |
| `PostOutboxBacklog` | `post_outbox_oldest_pending_seconds` | worker only (`cmd/worker/metrics.go:50`) | **can never fire** | can fire |
| `PostOutboxPublishFailing` | `increase(post_outbox_publish_failures_total[10m])` | worker only — the dispatcher runs there (`cmd/worker/main.go:286`); the API emits the series at a constant 0 | **can never fire** | can fire |
| `PostSearchUnavailable` | `post_search_query_duration_seconds` | api (`internal/search/retrieval/retrieval.go:385`) | can fire | can fire |
| `PostSearchLatencySlow` | same family | api | can fire | can fire |
| `PostWebhookFailuresSurge` | `post_webhook_deliveries_total` | worker only — the deliverer runs there (`cmd/worker/main.go:322`, `internal/events/deliver.go:183`) | **can never fire** | can fire |
| `PostJobQueueUnavailable` | `increase(post_queue_errors_total{operation="read"}[5m])` | worker only (`internal/worker/worker.go:135`) | **can never fire** | can fire |
| `PostJobDeadLettered` | `increase(post_queue_jobs_total{outcome="dead_lettered"}[15m])` | worker only (`internal/worker/worker.go:174,194,204,208`) | **can never fire** | can fire |
| `PostPermissionDenialsElevated` | `sum(rate(post_permission_denials_total{decision="forbidden"}[10m]))` | api — `decision="forbidden"` is only ever written by the HTTP boundary (`cmd/api/authhttp/envelope.go:72`) and the webhook receiver | can fire | can fire |

Counting the left-hand column of the "shipped stack" column above: **5 of the
12 rules can fire**, one more (`PostMetricsEndpointMissing`) can fire but only
in the narrower case where the API is gone as well, one
(`PostMetricsTargetMissing`) would be permanently firing because the worker job
it names does not exist in that configuration, and **5 can never fire at all**.
Those five are exactly the worker-only families, and losing them is not visible
from the alert list: a rule that cannot fire is a rule that stays quiet, which
is what a healthy platform looks like.

That table is measured, not asserted. Both columns of "which target feeds it"
come from a real run's scrapes — the harness prints the sample directory it
kept — and any reader can re-derive the family map from one:

```sh
# $SAMPLES is the directory the harness printed ("samples kept at: ...")
python3 - "$SAMPLES" <<'PY'
import glob, re, sys
def fams(p):
    out = set()
    for f in glob.glob(p + "-*.prom"):
        for line in open(f, encoding="utf-8"):
            if line.startswith("#") or not line.strip():
                continue
            out.add(re.sub(r"_(bucket|sum|count)$", "", line.split("{")[0].split()[0]))
    return out
a, w = fams(sys.argv[1] + "/api"), fams(sys.argv[1] + "/worker")
for n in sorted(a | w):
    print(f"{n:46} {'both' if n in a and n in w else ('api' if n in a else 'worker')}")
PY
```

Presence in a body is not movement, which is why the table's "which target"
column is decided by the producer's call site (cited per row) and confirmed
against the values where a run exercises them. The confirmation is visible in
the same samples: `grep -h '^post_outbox_publish_failures_total ' "$SAMPLES"/api-*.prom`
shows `0` in every scrape of the run while the worker's copy climbs into the
double digits. `post_db_up` is the other direction of the same test: it is
emitted by both bodies, and the harness asserts its own flagged samples — `1`
healthy, `0` at the fault, `1` after the restore
(`tests/observability/metrics-alerts-e2e.sh:681-685`). Between those flags the
series is not tidy: **13 of the 29 samples read `0` on each endpoint** in the
2026-09-21 run, which is what the rule's `for: 1m` is for (a 0 has to survive
four scrapes), and the generated rule test asserts `PostDatabaseUnavailable`
stays quiet at 135s and 420s with exactly those samples as its input.
