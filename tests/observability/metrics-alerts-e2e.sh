#!/usr/bin/env bash
# T1109 evidence: the instrument moves when the platform breaks, and the
# alert rules fire on the numbers it really reported.
#
# WHAT THIS PROVES, AND HOW
#
# A metrics endpoint that returns 200 proves nothing about whether it is
# measuring. So this harness does not check that /metrics answers; it BREAKS
# the platform and checks that the numbers move, in this order:
#
#   1. start the real cmd/api and cmd/worker against a real PostgreSQL
#      (docker, the same pgvector image docker-compose.yml uses) and the
#      in-repo cmd/devredis, and scrape both /metrics endpoints on a fixed
#      15s grid — the grid is the timeline the rule tests are evaluated on;
#   2. while everything is healthy, assert the alert rules do NOT fire on
#      the real samples (the negative control — without it, a rule that
#      always fires would pass this harness);
#   3. break it for real: kill devredis and stop the PostgreSQL container;
#   4. assert the counters actually moved, by reading the raw scrape text;
#   5. restart the dependency and assert the rules go quiet again;
#   6. hand the numbers scraped in steps 1-5 to promtool as input_series and
#      assert ALERTS{alertname="..."} is empty in step 2 and present in step
#      4 — so "the fault moved the metric" and "the rule fires" are one
#      chain over one set of samples, not two unrelated pieces of evidence.
#
# The backlog chain is broken a third way, and honestly: a real row is
# inserted into outbox_events with a 10-minute-old created_at and its row
# lock is held open in another session, so the dispatcher (whose claim uses
# FOR UPDATE SKIP LOCKED — internal/events/publish.go) genuinely cannot
# publish it. The row is real, the age is real, and the metric reads it.
#
# WHAT IS *NOT* REAL, STATED UP FRONT
#
# Four rules have no production caller to break: search retrieval has no
# production entry point in this tree, the webhook deliverer needs
# subscriber rows, and dead-lettering needs a job that exhausts its retries.
# Those rules are checked separately (rule expressions against synthesised
# series) and this harness PRINTS which rules were proven on real samples
# and which were not. It never claims more than it did.
#
# Run from anywhere:  bash tests/observability/metrics-alerts-e2e.sh
set -euo pipefail

cd "$(dirname "$0")/../.."   # repo root
ROOT="$(pwd)"
BIN="$(mktemp -d)/bin"
LOG="$(mktemp -d)"
SAMPLES="$(mktemp -d)"
mkdir -p "$BIN"

# Ports are chosen free at run time rather than fixed. This repository is
# developed on machines where other worktrees (and other people's containers)
# are running at the same time, and a fixed port turns "someone else is using
# 18192" into a red gate that has nothing to do with the change under test.
# Each can be pinned with POST_OBS_SMOKE_*_PORT when reproducing a specific
# run.
pick_free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}
API_PORT="${POST_OBS_SMOKE_API_PORT:-$(pick_free_port)}"
REDIS_PORT="${POST_OBS_SMOKE_REDIS_PORT:-$(pick_free_port)}"
PG_PORT="${POST_OBS_SMOKE_PG_PORT:-$(pick_free_port)}"
METRICS_PORT="${POST_OBS_SMOKE_METRICS_PORT:-$(pick_free_port)}"
MCP_PORT="$(pick_free_port)"
API="http://127.0.0.1:$API_PORT"
WORKER_METRICS="http://127.0.0.1:$METRICS_PORT"

# One scrape per INTERVAL, and the rule tests declare the same interval, so
# the test clock and the wall clock agree. Everything below is expressed in
# scrape indices; index * INTERVAL is the evaluation time.
# The timeline is the sum of the holds the rules demand, and no shorter: the
# longest `for:` that can overlap is 2m (the queue and publish rules), the
# backlog rule needs its own 2m of PostgreSQL-up time before the fault, and
# the database rule needs 1m to clear after the restore. 29 scrapes is that
# arithmetic with margin, not a round number.
#
# The last two slots are the BACKLOG rule's quiet half. Once PostgreSQL is
# back the row unblocks and the dispatcher publishes it, which it does on its
# next pass — and while PostgreSQL was down that pass was backing off under
# the ladder capped at events.DefaultBackoffMax (30s). So the gauge can still
# be high one slot after the restore and the rule can still be firing then;
# what has to hold is that it is quiet by the LAST sample, and 75s of
# PostgreSQL-up time after the restore is what makes that a statement about
# the rule rather than about where the dispatcher happened to be in its
# backoff when the database came back.
INTERVAL=15
IDX_LAST=28          # 29 scrapes = 7m0s of timeline
IDX_FAULT=10         # devredis killed, PostgreSQL stopped, after this scrapes (t=2m30s)
IDX_RESTORE=23       # devredis restarted, PostgreSQL started again (t=5m45s)
# The fault takes effect from slot 11, so the last fully-faulted sample is
# slot 22 (t=5m30s). That is: 2m for the database rule, 3m45s for the queue
# and publish rules, which is the 2m hold they ask for plus better than a
# minute of margin against a slot being dropped. The restore leaves 75s to
# the last sample, enough for the database rule's 1m hold to be seen to clear
# (the hold only has to ELAPSE for a firing alert; clearing is immediate) and
# for the backlog row to drain once its lock is released.

PG_NAME="t1109pg-$$"
PG_IMAGE="pgvector/pgvector:0.8.6-pg16"
PG_DSN="postgres://postgres:postgres_dev_pw@127.0.0.1:${PG_PORT}/post?sslmode=disable"
SQL_FILE="$(mktemp)"
LOCK_SQL="$(mktemp)"

# ---------------------------------------------------------------------------
# plumbing
# ---------------------------------------------------------------------------
APP_ENV=(POST_ENV=test
  POST_API_ADDR="127.0.0.1:$API_PORT"
  POST_MCP_ADDR="127.0.0.1:$MCP_PORT"
  POST_REDIS_ADDR="127.0.0.1:$REDIS_PORT"
  POST_DB_HOST=127.0.0.1 POST_DB_PORT="$PG_PORT" POST_DB_USER=postgres
  POST_DB_PASSWORD=postgres_dev_pw POST_DB_NAME=post POST_DB_SSLMODE=disable
  POST_BLOB_ACCESS_KEY=obs-ak POST_BLOB_SECRET_KEY=obs-sk
  POST_GITEA_TOKEN=obs-tok)

PIDS=()
PG_STARTED=0
cleanup() {
  # Process groups, not pids: the API and worker spawn goroutines that a
  # plain kill of the parent can orphan, and an orphan holding the metrics
  # port would serve the NEXT run's scrapes into a stale sample directory.
  for pid in "${PIDS[@]:-}"; do
    kill -- -"$pid" 2>/dev/null || kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
  if [ "$PG_STARTED" = 1 ]; then
    # rm -f, not stop: the fault injection stops and restarts the container,
    # so cleanup must remove it rather than leave a stopped one behind with a
    # name the next run would collide with.
    timeout 60 docker rm -f "$PG_NAME" >/dev/null 2>&1 || true
  fi
  rm -f "$SQL_FILE" "$LOCK_SQL"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# A port already in use means a leaked process from a previous run; every
# scrape below would then quietly describe the WRONG process and the evidence
# would be a fiction. bind() is the check, not a connect: a port that is
# bound but not yet listening must still count as taken.
require_free_port() {
  python3 - "$1" <<'PY' || fail "port $1 already in use (leaked process from a previous run?); free it before rerunning"
import socket, sys
s = socket.socket()
# SO_REUSEADDR, because that is how a server actually binds: Go's net.Listen
# and docker's port proxy both set it, so a socket left in TIME_WAIT by a
# previous run does NOT block them. Without it this probe is stricter than
# reality and fails on a port that is genuinely available — measured here:
# a finished run left a TIME_WAIT socket on the pg port and the probe
# reported "in use" while nothing was listening.
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
try:
    s.bind(("127.0.0.1", int(sys.argv[1])))
except OSError:
    sys.exit(1)
finally:
    s.close()
PY
}
for p in "$API_PORT" "$REDIS_PORT" "$PG_PORT" "$METRICS_PORT"; do require_free_port "$p"; done

wait_for() { # desc cmd...
  local desc="$1"; shift
  for _ in $(seq 1 120); do "$@" >/dev/null 2>&1 && return 0; sleep 0.5; done
  fail "timed out waiting for $desc"
}

pg() { timeout 30 docker exec -i "$PG_NAME" psql -U postgres -d post -tAq "$@"; }

# The postgres image's entrypoint starts a TEMPORARY server to run its init
# scripts and then restarts into the real one, so `pg_isready` (and even a
# successful connection) can succeed during the first boot and then be reset
# under you — which is exactly how this harness failed its first migration
# with "connection reset by peer". Readiness here means a real query
# succeeding several times in a row, not once.
wait_for_stable_pg() { # desc
  local desc="$1" ok=0 i
  for i in $(seq 1 60); do
    if timeout 10 docker exec "$PG_NAME" psql -U postgres -d post -tAq -c 'select 1' >/dev/null 2>&1; then
      ok=$((ok + 1))
      [ "$ok" -ge 3 ] && return 0
    else
      ok=0
    fi
    sleep 2
  done
  fail "PostgreSQL never became stably ready ($desc)"
}

echo ">> building api, worker, devredis, rddev"
# rddev is built here rather than `go run` later: it is the migration runner,
# and its compile time would otherwise land between "PostgreSQL is ready" and
# the first scrape.
go build -o "$BIN" ./cmd/api ./cmd/worker ./cmd/devredis ./cmd/rddev

PROMTOOL="$(bash tests/observability/promtool.sh)"
echo ">> promtool: $PROMTOOL"

# ---------------------------------------------------------------------------
# real PostgreSQL + real migrations
# ---------------------------------------------------------------------------
# No Redis-only fallback, and deliberately none: the sentence that used to sit
# here offered POST_OBS_SMOKE_NO_PG=1, a variable no line of this file ever
# read — advice that would have cost a reader a run to disprove. Half the
# evidence below IS the PostgreSQL half (the container is stopped and started
# on purpose), so a mode that skipped it would be a different, weaker check
# wearing this one's name; it would have to be a separate harness with its own
# timeline, not a flag on this one.
command -v docker >/dev/null 2>&1 || fail "docker is required: this harness stops and restarts a real PostgreSQL container to inject the database fault. There is no Redis-only mode: see the comment above this line."
docker image inspect "$PG_IMAGE" >/dev/null 2>&1 || fail "image $PG_IMAGE is not present locally. Pull it (docker pull $PG_IMAGE) rather than substituting another image: the migrations are applied to it."

echo ">> starting PostgreSQL ($PG_IMAGE)"
# No --rm: --rm removes the container the moment it STOPS, and the fault
# injection stops this container on purpose — with --rm there is nothing left
# to restart, which is how the first version of this harness failed at the
# recovery step. Cleanup removes it explicitly.
timeout 90 docker run -d --name "$PG_NAME" \
  -e POSTGRES_PASSWORD=postgres_dev_pw -e POSTGRES_DB=post \
  -p "127.0.0.1:$PG_PORT:5432" "$PG_IMAGE" >/dev/null
PG_STARTED=1
wait_for_stable_pg "first boot"

echo ">> applying migrations (make migrate)"
env POSTGRES_TEST_ADMIN_URL="$PG_DSN" timeout 300 "$BIN/rddev" db migrate >"$LOG/migrate.log" 2>&1 \
  || { tail -20 "$LOG/migrate.log"; fail "migrations failed"; }
tail -1 "$LOG/migrate.log"

# A real, committed, unpublished, ten-minute-old outbox row, locked in a
# second session so the dispatcher genuinely cannot claim it. Insert and lock
# are SEPARATE transactions: an uncommitted insert is invisible to the
# collector's read, which would make the backlog metric read zero and this
# whole chain a no-op.
cat >"$SQL_FILE" <<'SQL'
INSERT INTO outbox_events (event_type, payload, correlation_id, created_at)
VALUES ('t1109.backlog.probe', '{}'::jsonb, 't1109-backlog', now() - interval '600 seconds');
SQL
cat >"$LOCK_SQL" <<'SQL'
BEGIN;
SELECT id FROM outbox_events WHERE correlation_id = 't1109-backlog' FOR UPDATE;
SELECT pg_sleep(1200);
COMMIT;
SQL
pg -f - <"$SQL_FILE" >/dev/null || fail "could not insert the backlog probe row"

# Take the lock BEFORE any app process exists. An earlier version started the
# apps first and the worker's dispatcher claimed and published the row within
# its first poll, so the backlog read 0 and the chain proved nothing. The row
# must already be unclaimable by the time a dispatcher can run.
echo ">> holding the backlog row's lock (the dispatcher will skip it)"
timeout 1300 docker exec -i "$PG_NAME" psql -U postgres -d post -f - <"$LOCK_SQL" >"$LOG/lock.out" 2>&1 &
LOCK_PID=$!

# Confirm the lock is really held rather than assuming the session won the
# race: the row must be VISIBLE to the collector's read and INVISIBLE to the
# dispatcher's SKIP LOCKED claim at the same moment. Checking both is the
# point — either alone is consistent with the row being published or absent.
wait_for "the backlog row to be locked" bash -c '
  visible=$(docker exec -i '"$PG_NAME"' psql -U postgres -d post -tAq -c \
    "SELECT count(*) FROM outbox_events WHERE correlation_id = '"'"'t1109-backlog'"'"' AND published_at IS NULL")
  claimable=$(docker exec -i '"$PG_NAME"' psql -U postgres -d post -tAq -c \
    "SELECT count(*) FROM (SELECT id FROM outbox_events WHERE published_at IS NULL ORDER BY created_at, id LIMIT 10 FOR UPDATE SKIP LOCKED) t")
  [ "$visible" = "1" ] && [ "$claimable" = "0" ]
'
echo "   row is visible to the backlog query and unclaimable by the dispatcher"

# ...and the lock has to END. A lock held for the whole timeline makes the
# rule's "should be quiet" half unprovable: every sample would read the same
# stuck row, the rule would fire in all of them, and an assertion that it fires
# would be satisfied by a rule that can only fire. The lock is released once
# PostgreSQL is back, and the LAST sample is where the quiet half is asserted.
#
# Two things can have released it before this runs — the container stop at the
# fault ends the session that holds it — so the function is written to be true
# either way: it terminates whatever is still sleeping, then waits for the row
# to be un-blocked, rather than assuming the terminate found something.
release_backlog_lock() {
  local terminated
  terminated=$(pg -c "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
                      WHERE pid <> pg_backend_pid()
                        AND (query LIKE '%pg_sleep(1200)%' OR query LIKE '%t1109-backlog%')" 2>/dev/null) || terminated="?"
  # Reap the holder. Its psql exit status is non-zero by construction (it was
  # killed mid-statement), which is why this is not allowed to be fatal.
  wait "$LOCK_PID" 2>/dev/null || true
  # "Gone" means the row is EITHER already published (the dispatcher won the
  # race with this call) OR claimable by the very query the dispatcher uses.
  # Asserting only the second would fail on the healthy path.
  wait_for "the backlog row to stop being locked" bash -c '
    n=$(docker exec -i '"$PG_NAME"' psql -U postgres -d post -tAq -c \
      "SELECT count(*) FROM outbox_events o WHERE o.correlation_id = '"'"'t1109-backlog'"'"' AND (o.published_at IS NOT NULL OR o.id IN (SELECT id FROM outbox_events WHERE published_at IS NULL ORDER BY created_at, id LIMIT 10 FOR UPDATE SKIP LOCKED))")
    [ "$n" = "1" ]
  '
  echo "   backlog lock released (terminated ${terminated:-?} sleeping session(s))"
}

# ---------------------------------------------------------------------------
# real app processes
# ---------------------------------------------------------------------------
setsid env "${APP_ENV[@]}" "$BIN/devredis" -addr "127.0.0.1:$REDIS_PORT" >"$LOG/devredis.log" 2>&1 &
PIDS+=($!)
setsid env "${APP_ENV[@]}" "$BIN/api" >"$LOG/api.log" 2>&1 &
PIDS+=($!); API_PID="${PIDS[1]}"
setsid env "${APP_ENV[@]}" "$BIN/worker" -metrics-addr "127.0.0.1:$METRICS_PORT" >"$LOG/worker.log" 2>&1 &
PIDS+=($!); WORKER_PID="${PIDS[2]}"

wait_for "api health" curl -fsS "$API/healthz"
wait_for "api metrics" curl -fsS "$API/metrics"
wait_for "worker metrics" curl -fsS "$WORKER_METRICS/metrics"

# ---------------------------------------------------------------------------
# the refusal generator: one real account, one real session, no CSRF token
# ---------------------------------------------------------------------------
# The 403s this harness drives are a refusal the platform really makes: a
# state-changing POST carrying a valid session cookie and no X-CSRF-Token.
# cmd/api/authhttp's guard answers 403 CodeCSRFFailed before the handler runs,
# which is exactly the "permission denied" event docs/26 §3 asks to count.
#
# Why not the pre-session Origin refusal the earlier version of this harness
# used (POST /api/v1/auth/login with a foreign Origin): T1106 put a rate
# limiter in front of the whole tree (internal/security/ratelimit.go), and it
# classifies that route as "credential" with a 20/min budget
# (security.DefaultCredentialPerIP). Measured on this baseline: a single IP
# could only ever put 20 refusals in a minute, so a 10-minute window tops out
# at 0.33/s — under the rule's 1/s threshold no matter how hard the harness
# drives it. The same limiter gives an authenticated session 1200/min
# (DefaultAuthenticatedPerSession), which is the budget a real client — or a
# stolen cookie — actually has, and the flood stays inside it.
COOKIES="$LOG/cookies.txt"
SIGNUP_CODE=$(curl -sS -m 10 -o "$LOG/signup.json" -w '%{http_code}' -c "$COOKIES" \
  -X POST "$API/api/v1/auth/signup" -H 'Content-Type: application/json' \
  -d '{"email":"obs-smoke@example.com","password":"observability-smoke-pw","handle":"obssmoke","display_name":"Observability Smoke"}')
[ "$SIGNUP_CODE" = "201" ] || fail "signup returned $SIGNUP_CODE, not 201: without a session the refusal flood measures nothing ($(cat "$LOG/signup.json"))"
grep -q post_session "$COOKIES" || fail "signup set no session cookie: $(cat "$COOKIES")"

# The flood is only evidence if it really is refused, so the harness refuses to
# proceed on an assumption: one probe, and the status must be exactly 403 with
# the CSRF code. A 200 here would mean the flood is exercising the handler, and
# a 401/404 would mean it is not exercising the guard the rule is about.
PROBE=$(curl -sS -m 10 -o "$LOG/probe.json" -w '%{http_code}' -b "$COOKIES" \
  -X POST "$API/api/v1/projects" -H 'Content-Type: application/json' -d '{}')
[ "$PROBE" = "403" ] || fail "a CSRF-less POST with a session returned $PROBE, not 403: the flood is not measuring permission denials ($(cat "$LOG/probe.json"))"
grep -q CSRF_FAILED "$LOG/probe.json" || fail "the 403 was not the CSRF refusal: $(cat "$LOG/probe.json")"
echo ">> refusal generator: a real session, refused by the guard's CSRF check (403)"

echo ">> /metrics is served at /metrics and nowhere else"
# --path-as-is, not a plain URL. curl resolves ".." in the path before sending,
# so without it this line sent GET /admin and printed it under the label
# "/metrics/../admin" — a request that was never made. With it the path really
# is /metrics/../admin, and the assertion is on the BODY rather than the status
# code: the claim being checked is "the exposition is served at one route and
# nowhere else", and that is a statement about what comes back, not about which
# code the router chose for a path it declines. This run's answer was 307:
# net/http's ServeMux cleans the path first and redirects to the cleaned one
# with StatusTemporaryRedirect (server.go, the cleanPath branch).
code=$(curl -sS -o "$LOG/probe-traversal.out" -w '%{http_code}' "$WORKER_METRICS/metrics")
[ "$code" = "200" ] || fail "GET /metrics answered $code, want 200 — there is no exposition to check the rest of this against"
printf '   GET /metrics             -> %s\n' "$code"
code=$(curl -sS --path-as-is -o "$LOG/probe-traversal-dotdot.out" -w '%{http_code}' "$WORKER_METRICS/metrics/../admin")
printf '   GET /metrics/../admin    -> %s\n' "$code"
if grep -q '^post_' "$LOG/probe-traversal-dotdot.out"; then
  fail "GET /metrics/../admin returned the exposition: the endpoint is a route, not a prefix"
fi

echo
echo ">> the composed root mux: both routes, on this run's API process"
# This asserts the one region of cmd/api/main.go that two changes touched: the
# search wiring (T0906) and the /api/v1 mount + metrics registration (T1109)
# sit in the same block, and it was composed by hand when main moved under
# this task. Both sides compile either way, so the check has to be a request
# against the running process — see route-probe.sh for why the 405/404 pair,
# and not a status code on its own, is what makes it evidence.
bash tests/observability/route-probe.sh "$API" || fail "the composed root mux does not serve both routes (see route-probe.sh output above)"

# ---------------------------------------------------------------------------
# the timeline
# ---------------------------------------------------------------------------
# Broken with two real faults and driven each step with:
#   - a burst of 403 CSRF refusals from the real session (see above)
#   - one enqueue on the product path, POST /internal/jobs
# The burst is sized so the 10-minute refusal RATE clears the rule's threshold;
# the counter values are real even though the load is deliberate.
#
# 100, and the arithmetic is not "one per second". The rule is
# sum(rate(post_permission_denials_total{decision="forbidden"}[10m])) > 1, and
# rate() divides by the WINDOW (10m), not by the span the series happens to
# cover — so clearing a 1/s threshold needs more than 600 refusals inside the
# window, whatever the elapsed time. Measured on this run's own samples, the
# counter reads 1 + 100*slot, so rate at slot i = (100i + 1)/600 and the
# threshold is crossed at slot 6 (t=90s). The rule's `for: 2m` then holds the
# alert pending until t=210s: the last healthy sample (t=135s) is 75s short of
# firing and the mid-fault sample (t=330s) is 120s past it. Driving more would
# eat the T_CTRL margin for nothing; 100/slot is also 400/min against the
# authenticated budget of 1200/min
# (security.DefaultAuthenticatedPerSession), which the harness asserts it
# stays inside by never producing a 429.
BURST="${POST_OBS_SMOKE_BURST:-100}"
FAULT_ACTIVE=0
drive_traffic() {
  local i
  # The flood stops while Redis is gone: the edge limiter fails closed, so a
  # request in that window is refused by the limiter before the guard can see
  # it (measured: the refusals land as 503 on route "/" and never reach
  # authhttp), and every one of them costs a Redis dial. Driving it would add
  # minutes of wall clock and no refusals.
  if [ "$FAULT_ACTIVE" = 0 ]; then
    for i in $(seq 1 "$BURST"); do
      curl -sS -m 5 -o /dev/null -b "$COOKIES" -X POST "$API/api/v1/projects" \
        -H 'Content-Type: application/json' -d '{}' || true
    done
  fi
  # One product-path enqueue every slot, fault or not: healthy it is the 202
  # that proves the ROUTE PATTERN reaches the request counter
  # (route="POST /internal/jobs"), and under the fault it is refused at the
  # edge — which is what the samples show as 503 on route "/" instead.
  curl -sS -m 5 -o /dev/null -X POST "$API/internal/jobs" -H 'Content-Type: application/json' \
    -d '{"type":"smoke","payload":{"ref":"rsg/obs"}}' || true
}

scrape() { # index
  local i; i=$(printf '%03d' "$1")
  # Timestamped at the START, which is the instant Prometheus timestamps a
  # scrape at and, more practically, the only one that is stable: with a
  # dependency down the /metrics handler itself takes seconds (each
  # scrape-time collector probes with its own timeout budget, and the ping
  # has to fail before the page can be written), so stamping the end made a
  # 15s schedule look like an 18s one.
  echo "$1 $(date +%s)" >>"$SAMPLES/times.txt"
  if curl -fsS -m 5 "$API/metrics" -o "$SAMPLES/api-$i.prom" 2>/dev/null; then :; else rm -f "$SAMPLES/api-$i.prom"; : >"$SAMPLES/api-$i.prom"; fi
  if curl -fsS -m 5 "$WORKER_METRICS/metrics" -o "$SAMPLES/worker-$i.prom" 2>/dev/null; then :; else rm -f "$SAMPLES/worker-$i.prom"; : >"$SAMPLES/worker-$i.prom"; fi
  # A real readiness poll every slot, the way a load balancer does it, LAST so
  # it cannot push the two scrapes off the grid (with a dependency down the
  # health handler's own probe has to fail before it can answer, which costs
  # seconds). It is on the product path — route "/readyz", counted by the same
  # middleware as every product request — and exempt from the edge limiter
  # (security.ExemptProbePaths), so during the outage it reaches the handler
  # and answers 503 with the dependency named. That 503 is the 5xx the request
  # counter can still see: with Redis gone the limiter fails closed and every
  # OTHER request is refused before the mux, counted on route "/" instead.
  # Its 503 lands in this slot's counter but is only readable in the NEXT
  # scrape, which is how Prometheus sees it too.
  curl -sS -m 5 -o /dev/null "$API/readyz" 2>/dev/null || true
}

echo
echo ">> scraping both endpoints every ${INTERVAL}s for $((IDX_LAST + 1)) samples"
echo ">> faults at index $IDX_FAULT (t=$((IDX_FAULT * INTERVAL))s), restore at index $IDX_RESTORE (t=$((IDX_RESTORE * INTERVAL))s)"
# The grid is anchored to a start instant rather than paced by `sleep 15`:
# driving traffic costs real time, and an interval that drifts to 17s would
# make the declared `interval: 15s` in the rule test a lie about the elapsed
# test clock — which is exactly the quantity `for:` is measured in.
T0=$(date +%s)
for i in $(seq 0 "$IDX_LAST"); do
  # A slot whose time has already passed is DROPPED, not scraped late. The
  # fault and restore steps cost real seconds (docker stop/start), and
  # scraping them late would mean the samples labelled 3m15s were really taken
  # at 3m05s — so `for: 2m` would be satisfied on a shorter hold than it
  # claims. A dropped slot leaves no file, which the generator turns into `_`,
  # exactly as a real Prometheus records a scrape it missed.
  # 4s, not half the interval: the sample for slot i must land close enough to
  # i*INTERVAL that declaring interval:15s is true. One dropped sample is a
  # `_` in the series and costs nothing; a 3s-late sample is a small lie about
  # how long a rule held.
  slot_target=$((T0 + i * INTERVAL))
  now=$(date +%s)
  if [ "$now" -gt "$((slot_target + 4))" ]; then
    echo "   [slot $i / t=$((i * INTERVAL))s] dropped: previous step overran the grid"
    continue
  fi
  sleep $((slot_target - now))
  scrape "$i"

  # The fault and restore actions run AFTER this slot's scrape. They cost
  # several real seconds (docker stop, container restart, readiness wait), and
  # running them first made the sample for their own slot land late — which
  # shows up as an 18s gap where the rule test declares 15s. Placed here, the
  # slot's sample is on time and the following sleep absorbs the cost.
  if [ "$i" = "$IDX_FAULT" ]; then
    echo "   [t=$((i * INTERVAL))s] FAULT: killing devredis + stopping PostgreSQL (after this scrape)"
    FAULT_ACTIVE=1
    kill -- -"${PIDS[0]}" 2>/dev/null || kill "${PIDS[0]}" 2>/dev/null || true
    # -t 2: a two-second grace, then SIGKILL. The default ten-second graceful
    # stop costs 15s of real time here (PostgreSQL checkpoints on shutdown),
    # which overran the scrape grid; and an abrupt death is the more honest
    # model of "the database went away" than a clean checkpoint.
    timeout 90 docker stop -t 2 "$PG_NAME" >/dev/null || fail "could not stop PostgreSQL"
  fi
  if [ "$i" = "$IDX_RESTORE" ]; then
    echo "   [t=$((i * INTERVAL))s] RESTORE: restarting devredis + PostgreSQL (after this scrape)"
    timeout 90 docker start "$PG_NAME" >/dev/null || fail "could not restart PostgreSQL"
    # Wait for readiness rather than for the container to exist: `docker
    # start` returns as soon as the process is spawned, and a scrape that
    # lands during startup would record post_db_up 0 for a database that is
    # about to be fine — turning the recovery assertion into a race.
    wait_for_stable_pg "after restart"
    # Released BEFORE the Redis restart, and before the remaining slots: the
    # backlog rule's quiet half is the whole point of the release, and every
    # second spent here is a second the dispatcher does not have to drain the
    # row before the last sample. The drain needs PostgreSQL only.
    release_backlog_lock
    setsid env "${APP_ENV[@]}" "$BIN/devredis" -addr "127.0.0.1:$REDIS_PORT" >"$LOG/devredis-restarted.log" 2>&1 &
    PIDS+=($!)
    FAULT_ACTIVE=0
  fi
  drive_traffic
done

# The declared interval has to be TRUE, because every `for:` in the rule file
# is measured against it. Verify the grid instead of trusting the sleeps.
# The check is on (wall clock between two samples) vs (index distance between
# them), which is the quantity the rule test actually relies on — a sample
# pair 2 slots apart must be 2*INTERVAL apart in reality.
echo ">> scrape grid (target ${INTERVAL}s):"
python3 - "$SAMPLES/times.txt" "$INTERVAL" <<'PY'
import sys
rows = [l.split() for l in open(sys.argv[1]) if l.strip()]
idx = [int(r[0]) for r in rows]
ts = [int(r[1]) for r in rows]
bad = []
for a in range(len(rows) - 1):
    want = (idx[a + 1] - idx[a]) * int(sys.argv[2])
    got = ts[a + 1] - ts[a]
    if abs(got - want) > 2:
        bad.append(f"slot {idx[a]}->{idx[a + 1]}: {got}s, expected {want}s")
dropped = (max(idx) if idx else 0) + 1 - len(idx)
print(f"   {len(ts)} scrapes on a {int(sys.argv[2])}s grid, {dropped} slot(s) dropped")
if bad:
    print("   GRID BROKEN — the declared interval does not match reality:", file=sys.stderr)
    for b in bad:
        print(f"     {b}", file=sys.stderr)
    sys.exit(1)
print("   every sample pair is exactly (index distance * interval) apart")
PY

# ---------------------------------------------------------------------------
# raw observations, read out of the scrape bodies rather than asserted from
# memory. These are printed verbatim in the RESULT.
# ---------------------------------------------------------------------------
raw() { # file series-selector
  grep -E "^$2" "$SAMPLES/$1" 2>/dev/null | tail -1 || echo "(absent)"
}

# Slots are dropped when a step overruns, so no sample can be addressed by a
# fixed index. These pick the samples that MATTER by position in the timeline:
# the first scrape, the last one before the fault, the last one still under
# the fault, and the last one overall.
present_indices() {
  # awk drops the zero padding. Without it bash reads "026" as an OCTAL
  # literal — 22 — and every derived time and file name is quietly wrong,
  # which is how this harness first reported its last sample as t=330s.
  ls "$SAMPLES"/worker-*.prom 2>/dev/null | sed 's#.*worker-##; s#\.prom##' \
    | awk '{print $1 + 0}' | sort -n
}
f3() { printf '%03d' "$1"; }
IDX_S_FIRST=$(present_indices | head -1)
IDX_S_PRE=$(present_indices | awk -v f="$IDX_FAULT" '$1 < f' | tail -1)
IDX_S_MID=$(present_indices | awk -v f="$IDX_RESTORE" '$1 < f' | tail -1)
IDX_S_LAST=$(present_indices | tail -1)
T_FIRST=$((IDX_S_FIRST * INTERVAL)); T_PRE=$((IDX_S_PRE * INTERVAL))
T_MID=$((IDX_S_MID * INTERVAL)); T_LAST=$((IDX_S_LAST * INTERVAL))
# Halfway through the healthy window, and only used as an eval time for the
# denial rule's negative control: by then the flood has run in half the slots
# (rate ≈ 0.8/s, measured in the run's own output below), which is under the
# rule's threshold, so the alert must be in NO state at all. That is the
# "always fires" control for the one rule whose T_PRE state is legitimately
# pending (see the promtool section).
T_CTRL=$(( IDX_FAULT / 2 * INTERVAL ))
[ -n "$IDX_S_PRE" ] && [ -n "$IDX_S_MID" ] || fail "no sample at the expected positions around the fault (first=$IDX_S_FIRST pre=$IDX_S_PRE mid=$IDX_S_MID last=$IDX_S_LAST)"

echo
echo "=== RAW SAMPLES: the numbers, as the processes reported them ==="
printf '%-58s %s\n' "series" "healthy(t=${T_FIRST}s) | fault(t=${T_MID}s) | restored(t=${T_LAST}s)"
show() { # label api-selector
  printf '%-58s %s | %s | %s\n' "$1" \
    "$(raw "api-$(f3 "$IDX_S_FIRST").prom" "$2")" \
    "$(raw "api-$(f3 "$IDX_S_MID").prom" "$2")" \
    "$(raw "api-$(f3 "$IDX_S_LAST").prom" "$2")"
}
showv() { # label worker-selector
  printf '%-58s %s | %s | %s\n' "$1" \
    "$(raw "worker-$(f3 "$IDX_S_FIRST").prom" "$2")" \
    "$(raw "worker-$(f3 "$IDX_S_MID").prom" "$2")" \
    "$(raw "worker-$(f3 "$IDX_S_LAST").prom" "$2")"
}
show 'post_db_up' 'post_db_up'
show 'post_http_requests_total{route="/readyz",status="503"}' 'post_http_requests_total\{method="GET",route="/readyz",status="503"\}'
show 'post_http_requests_total{route="POST /internal/jobs",status="202"}' 'post_http_requests_total\{method="POST",route="POST /internal/jobs",status="202"\}'
show 'post_http_requests_total{route="/",status="503"}' 'post_http_requests_total\{method="POST",route="/",status="503"\}'
show 'post_permission_denials_total{decision="forbidden"}' 'post_permission_denials_total\{decision="forbidden"'
showv 'post_queue_errors_total{operation="read"}' 'post_queue_errors_total\{operation="read"\}'
showv 'post_outbox_publish_failures_total' 'post_outbox_publish_failures_total'
showv 'post_outbox_oldest_pending_seconds' 'post_outbox_oldest_pending_seconds'
showv 'post_queue_depth{state="jobs"}' 'post_queue_depth\{queue="post",state="jobs"\}'
# The family that makes "the collector could not read its source" visible.
# Without it, a scrape-time source going away is indistinguishable from the
# quantity being zero, which is the failure mode collect.go exists to avoid.
showv 'post_metrics_collector_errors_total{collector=outbox_age}' 'post_metrics_collector_errors_total\{collector="post_outbox_oldest_pending_seconds"\}'

echo
echo "=== the instrument must have MOVED, not just answered ==="
val() { raw "$1" "$2" | awk '{print $NF}'; }

# An ABSENT counter series is not missing evidence: Prometheus does not
# materialise a counter vector until its first increment, so "absent at t=15s"
# and "zero at t=15s" are the same observation. It is only the PRESENT value
# that has to be real, and the process's liveness is established separately
# (post_db_up, printed above, plus the other families that are present).
# Reading absence as zero here is therefore a statement about the counter
# model, not a tolerance for a broken scrape.
check_moved_up() { # desc before-file after-file series
  local desc="$1" bf="$2" af="$3" sel="$4" before after
  before=$(val "$bf" "$sel"); after=$(val "$af" "$sel")
  [ "$before" = "(absent)" ] && before=0
  [ "$after" != "(absent)" ] || fail "$desc: $sel is absent AFTER the fault (the metric never appeared)"
  python3 -c "
import sys
b, a = float('$before'), float('$after')
sys.exit(0 if a > b else 1)" || fail "$desc: $sel did not rise (before=$before after=$after)"
  echo "   OK  $desc"
  echo "       $sel  $before -> $after"
}
W_FIRST="worker-$(f3 "$IDX_S_FIRST").prom"; W_PRE="worker-$(f3 "$IDX_S_PRE").prom"
W_MID="worker-$(f3 "$IDX_S_MID").prom"; A_FIRST="api-$(f3 "$IDX_S_FIRST").prom"
A_PRE="api-$(f3 "$IDX_S_PRE").prom"; A_MID="api-$(f3 "$IDX_S_MID").prom"

check_moved_up "queue read errors rose while Redis was down" "$W_FIRST" "$W_MID" 'post_queue_errors_total\{operation="read"\}'
check_moved_up "outbox publish failures rose while PostgreSQL was down" "$W_FIRST" "$W_MID" 'post_outbox_publish_failures_total'
# The product-path 5xx. This assertion used to be the enqueue route's own 503
# ("the queue is gone, so enqueue fails"), and on this baseline that is no
# longer reachable: T1106's edge limiter fails closed when Redis is gone, so
# every request except the two probes is refused BEFORE the mux (counted on
# route "/", asserted separately below). The readiness endpoint is exempt from
# the limiter, so its 503 — a real 5xx on a real product route, caused by the
# same outage — is the 5xx the request counter can still see. See
# ops/observability/README.md §6.1 for what this costs and why it is not hidden.
check_moved_up "the readiness probe answered 503 while a dependency was down" "$A_FIRST" "$A_MID" 'post_http_requests_total\{method="GET",route="/readyz",status="503"\}'
check_moved_up "the edge's fail-closed refusals were counted on their own route" "$A_FIRST" "$A_MID" 'post_http_requests_total\{method="POST",route="/",status="503"\}'
# The route pattern, on the product path, while everything is healthy: this is
# the assertion that the ServeMux pattern — not the raw path — is what lands in
# the label (route.go). It runs healthy-to-healthy on purpose; the same
# request under the fault never reaches the mux at all.
check_moved_up "the enqueue route pattern reached the request counter" "$A_FIRST" "$A_PRE" 'post_http_requests_total\{method="POST",route="POST /internal/jobs",status="202"\}'
check_moved_up "403 refusals were counted" "$A_FIRST" "$A_MID" 'post_permission_denials_total\{decision="forbidden"'
# The scrape-time collectors stop emitting while PostgreSQL is down, and this
# is the family that says so. It is the reason a missing sample is legible at
# all: without it, "the collector could not read the backlog" and "the backlog
# is zero" produce the same scrape body.
check_moved_up "collector read failures rose while PostgreSQL was down" "$W_FIRST" "$W_MID" \
  'post_metrics_collector_errors_total\{collector="post_outbox_oldest_pending_seconds"\}'

# Negative control on the LOAD, not on the alerts: if the flood had driven past
# the edge limiter's budget, the denials it counts would be the limiter's 429s
# — answered before the guard runs, so the guard's 403 never happens — and the
# permission-denial rule would be measuring the wrong layer while still
# looking green. A 429 anywhere in the run means the load, not the product, is
# what the numbers describe.
if grep -l 'post_http_requests_total{method="POST",route="/",status="429"}' \
  "$SAMPLES"/api-*.prom >/dev/null 2>&1; then
  fail "a 429 appears in the capture: the flood exceeded the edge budget, so the refusals counted are the limiter's, not the guard's"
fi
echo "   OK  the flood stayed inside the authenticated budget (no 429 in any sample)"

# The backlog is not a "rose" story: the row is already ten minutes old when
# the first scrape happens (it is inserted that way), so the assertion is that
# the AGE is over the rule's threshold AND still growing — a static large
# number could be a stale gauge, a growing one is a real row being read.
b0=$(val "$W_FIRST" 'post_outbox_oldest_pending_seconds')
b1=$(val "$W_PRE" 'post_outbox_oldest_pending_seconds')
[ "$b0" != "(absent)" ] || fail "post_outbox_oldest_pending_seconds was absent while PostgreSQL was up"
python3 -c "
import sys
a, b = float('$b0'), float('$b1')
sys.exit(0 if a > 300 and b > a else 1)" \
  || fail "the backlog age did not clear 300s and keep growing (${b0}s then ${b1}s)"
echo "   OK  outbox backlog age is over the rule's 300s threshold and growing"
echo "       post_outbox_oldest_pending_seconds  ${b0}s -> ${b1}s"
echo "       post_outbox_pending_events          $(val "$W_FIRST" 'post_outbox_pending_events')"

# post_db_up must go 1 -> 0 -> 1. The middle step is the fault; the last is
# the strongest negative control available, because the SAME process reports
# health again without anything being restarted.
A_LAST="api-$(f3 "$IDX_S_LAST").prom"
db() { val "$1" 'post_db_up'; }
echo "   post_db_up: healthy=$(db "$A_FIRST") fault=$(db "$A_MID") restored=$(db "$A_LAST")"
[ "$(db "$A_FIRST")" = "1" ] || fail "post_db_up was not 1 while PostgreSQL was up"
[ "$(db "$A_MID")" = "0" ] || fail "post_db_up was not 0 while PostgreSQL was stopped"
[ "$(db "$A_LAST")" = "1" ] || fail "post_db_up did not return to 1 after PostgreSQL restarted"

# ---------------------------------------------------------------------------
# the same samples, through the rules
# ---------------------------------------------------------------------------
echo
echo "=== promtool: syntax ==="
"$PROMTOOL" check rules ops/observability/alerts.yml || fail "promtool check rules rejected ops/observability/alerts.yml"

echo
echo "=== promtool: ALERTS on the samples really captured above ==="
# Every expectation below is a claim about the REAL series, never about a
# synthesised one. Three of them are negative controls and they are the
# reason this section proves something:
#   - the three rules that must be quiet before the fault are quiet because
#     nothing had broken yet, not because their expression is inert;
#   - PostRSGGitDrift must stay quiet WHILE PostgreSQL is down: the drift
#     gauge reads 0 findings during the outage, and a rule that fired on
#     "the platform is degraded" rather than on measured drift would be
#     caught here;
#   - PostMetricsEndpointMissing is `absent(post_db_up)`, so it must NOT
#     fire while post_db_up is present and merely 0. That is the exact
#     confusion this rule exists to separate (blind vs unhealthy), and it
#     is checked in the state where conflating them is tempting.
#
# PostOutboxBacklog at T_MID is the one assertion here that states a
# Prometheus SEMANTIC rather than a healthy/unhealthy pair, and it was
# measured before it was written down: the backlog gauge has NO sample at
# that instant (postgres is stopped, the collector reads nothing), yet the
# rule is still firing. Prometheus carries a series' last sample forward for
# the lookback delta and a series missing from a scrape BODY is never marked
# stale, so the 750s reading taken at t=2m30s is still the value at t=5m30s.
# Writing this as ":0" would encode the assumption that a missing sample
# clears the alert, which is false, and the rule file's own comment about
# this limit would then be wrong in the same way.
#
# The denial rule is asserted in THREE states over the same samples, and the
# middle one is the interesting one. On this baseline the 403 flood crosses
# the rule's 1/s threshold partway through the healthy window, so at T_PRE the
# alert is PENDING: the expression is true and the rule's own `for: 2m` is what
# is holding it back. Asserting ":0" (no ALERTS series at all) at T_PRE, which
# is what this harness did before the rebaseline, is a claim about the traffic
# shape rather than about the rule, and on a fast enough flood it is simply
# false; the state is asserted explicitly instead, and the strict "no series at
# all" control is kept at T_CTRL, where the condition genuinely does not hold
# yet. Nothing is relaxed: an always-firing rule still fails T_CTRL, a rule
# without its hold fails T_PRE, and an inert rule fails T_MID.
#
# The generator prints a NOTE that
# post_http_requests_total{method="POST",route="POST /internal/jobs",status="503"}
# was never captured. That is EXPECTED on this baseline and is the same fact as
# the assertion change below: the enqueue's own 503 cannot happen while the edge
# refuses first, so the series does not exist to capture. It is left visible
# rather than silenced — a line that says "this series was never seen" is
# exactly what a reader should find.
TEST_YML="$SAMPLES/generated-rules-test.yml"
python3 tests/observability/gen-rule-tests.py \
  --samples "$SAMPLES" --rules ops/observability/alerts.yml --output "$TEST_YML" \
  --note "Timeline: healthy until t=$((IDX_FAULT * INTERVAL))s (devredis killed, PostgreSQL stopped), restored at t=$((IDX_RESTORE * INTERVAL))s, last sample t=$((IDX_LAST * INTERVAL))s. PostPermissionDenialsElevated is pending at t=${T_PRE}s and firing at t=${T_MID}s: the flood crosses its threshold at t=$((T_PRE - 45))s and the rule's for: holds it until t=$((T_PRE + 75))s." \
  --expect "PostDatabaseUnavailable=$((T_PRE))s:0" \
  --expect "PostJobQueueUnavailable=$((T_PRE))s:0" \
  --expect "PostOutboxPublishFailing=$((T_PRE))s:0" \
  --expect "PostPermissionDenialsElevated=$((T_CTRL))s:0" \
  --expect "PostPermissionDenialsElevated=$((T_PRE))s:2" \
  --expect "PostOutboxBacklog=$((T_PRE))s:1" \
  --expect "PostDatabaseUnavailable=$((T_MID))s:1" \
  --expect "PostJobQueueUnavailable=$((T_MID))s:1" \
  --expect "PostOutboxPublishFailing=$((T_MID))s:1" \
  --expect "PostPermissionDenialsElevated=$((T_MID))s:1" \
  --expect "PostRSGGitDrift=$((T_MID))s:0" \
  --expect "PostMetricsEndpointMissing=$((T_MID))s:0" \
  --expect "PostOutboxBacklog=$((T_MID))s:1" \
  --expect "PostOutboxBacklog=$((T_LAST))s:0" \
  --expect "PostDatabaseUnavailable=$((T_LAST))s:0"
cp "$TEST_YML" "$LOG/generated-rules-test.yml"
"$PROMTOOL" test rules "$TEST_YML" || fail "a rule did not fire (or fired when it should not) on the captured samples"
sed -n '/promql_expr_test/,$p' "$TEST_YML" | grep -E '#|expr:|eval_time:|exp_samples|labels:|value:' | sed 's/^/   /'

# ---------------------------------------------------------------------------
echo
echo "=== rules the fault injection above cannot reach: checked, and labelled as such ==="
echo ">> the four rules below are checked against synthesised series, because"
echo ">> nothing in this tree can produce their input: a search retrieval call"
echo ">> (its errors, and the shape of its latency tail), a webhook delivery, a"
echo ">> retry-exhausted job. They are NOT proven by the fault injection above,"
echo ">> and this harness will not imply that they are."
echo ">> Two more rules are checked here too, and their input is synthesised for"
echo ">> a different and stronger reason: PostMetricsTargetMissing and"
echo ">> PostMetricsEndpointMissing read \`up\`, which is not a POST series at all"
echo ">> — Prometheus writes it from the result of each scrape. No scrape body"
echo ">> above can contain it, and this harness never stops either process, so"
echo ">> no real sample of a dead target exists to generate from. What the cases"
echo ">> assert is the pair's SEPARATION: one target gone fires the first rule"
echo ">> and leaves the second silent (the state the second one alone could not"
echo ">> see), and both reporting leaves both silent."
echo ">> Every rule in this section is driven in BOTH directions: one case that"
echo ">> must fire and one that must stay silent. A rule checked only in the"
echo ">> firing direction would pass on an expression that is always true."
SELF_TEST="$(mktemp)"
cat >"$SELF_TEST" <<YAML
rule_files:
  - $ROOT/ops/observability/alerts.yml
evaluation_interval: 1m
tests:
  # search: errors dominate the family.
  - interval: 1m
    input_series:
      - series: 'post_search_query_duration_seconds_count{outcome="error"}'
        values: '0 5 10 15 20 25 30 35 40 45 50 55 60 65'
      - series: 'post_search_query_duration_seconds_count{outcome="ok"}'
        values: '0 1 2 3 4 5 6 7 8 9 10 11 12 13'
      - series: 'post_search_query_duration_seconds_count{outcome="refused"}'
        values: '0 1 2 3 4 5 6 7 8 9 10 11 12 13'
    promql_expr_test:
      - expr: 'ALERTS{alertname="PostSearchUnavailable"}'
        eval_time: 14m
        exp_samples:
          - labels: 'ALERTS{alertname="PostSearchUnavailable", alertstate="firing", severity="P2"}'
            value: 1
  # the same family refusals-only must stay quiet: a refusal is not an outage.
  - interval: 1m
    input_series:
      - series: 'post_search_query_duration_seconds_count{outcome="error"}'
        values: '0 0 0 0 0 0 0 0 0 0 0 0 0 0'
      - series: 'post_search_query_duration_seconds_count{outcome="ok"}'
        values: '0 1 2 3 4 5 6 7 8 9 10 11 12 13'
      - series: 'post_search_query_duration_seconds_count{outcome="refused"}'
        values: '0 20 40 60 80 100 120 140 160 180 200 220 240 260'
    promql_expr_test:
      - expr: 'ALERTS{alertname="PostSearchUnavailable"}'
        eval_time: 14m
        exp_samples: []
      # ...and the latency rule must not fire off the count family either:
      # this case carries no _bucket series at all, so the p99 has no input.
      - expr: 'ALERTS{alertname="PostSearchLatencySlow"}'
        eval_time: 14m
        exp_samples: []
  # search latency, slow tail: every query lands in the (2s, 5s] bucket, so
  # the p99 the rule computes sits just under 5s and the 2s threshold is
  # crossed. All four le series are fed because histogram_quantile needs the
  # buckets to be cumulative and to reach +Inf.
  - interval: 1m
    input_series:
      - series: 'post_search_query_duration_seconds_bucket{outcome="ok",le="0.5"}'
        values: '0 0 0 0 0 0 0 0 0 0 0 0 0 0 0'
      - series: 'post_search_query_duration_seconds_bucket{outcome="ok",le="2"}'
        values: '0 0 0 0 0 0 0 0 0 0 0 0 0 0 0'
      - series: 'post_search_query_duration_seconds_bucket{outcome="ok",le="5"}'
        values: '0 60 120 180 240 300 360 420 480 540 600 660 720 780 840'
      - series: 'post_search_query_duration_seconds_bucket{outcome="ok",le="+Inf"}'
        values: '0 60 120 180 240 300 360 420 480 540 600 660 720 780 840'
    promql_expr_test:
      - expr: >-
          histogram_quantile(0.99,
            sum by (le) (rate(post_search_query_duration_seconds_bucket{outcome="ok"}[10m])))
        eval_time: 14m
        exp_samples:
          - labels: '{}'
            value: 4.97
      - expr: 'ALERTS{alertname="PostSearchLatencySlow"}'
        eval_time: 14m
        exp_samples:
          - labels: 'ALERTS{alertname="PostSearchLatencySlow", alertstate="firing", severity="P2"}'
            value: 1
  # search latency, fast tail: the SAME shape and the same query rate, with
  # every query under 5ms. Nothing changes but the bucket the observations
  # land in — which is what makes this pair a test of the threshold rather
  # than of the traffic.
  - interval: 1m
    input_series:
      - series: 'post_search_query_duration_seconds_bucket{outcome="ok",le="0.005"}'
        values: '0 60 120 180 240 300 360 420 480 540 600 660 720 780 840'
      - series: 'post_search_query_duration_seconds_bucket{outcome="ok",le="0.01"}'
        values: '0 60 120 180 240 300 360 420 480 540 600 660 720 780 840'
      - series: 'post_search_query_duration_seconds_bucket{outcome="ok",le="+Inf"}'
        values: '0 60 120 180 240 300 360 420 480 540 600 660 720 780 840'
    promql_expr_test:
      - expr: >-
          histogram_quantile(0.99,
            sum by (le) (rate(post_search_query_duration_seconds_bucket{outcome="ok"}[10m])))
        eval_time: 14m
        exp_samples:
          - labels: '{}'
            value: 0.00495
      - expr: 'ALERTS{alertname="PostSearchLatencySlow"}'
        eval_time: 14m
        exp_samples: []
  # webhook: terminal + retry dominate. retry_no_streak must NOT count.
  - interval: 1m
    input_series:
      - series: 'post_webhook_deliveries_total{outcome="terminal"}'
        values: '0 3 6 9 12 15 18 21 24 27 30 33 36 39'
      - series: 'post_webhook_deliveries_total{outcome="retry"}'
        values: '0 3 6 9 12 15 18 21 24 27 30 33 36 39'
      - series: 'post_webhook_deliveries_total{outcome="delivered"}'
        values: '0 2 4 6 8 10 12 14 16 18 20 22 24 26'
    promql_expr_test:
      - expr: 'ALERTS{alertname="PostWebhookFailuresSurge"}'
        eval_time: 14m
        exp_samples:
          - labels: 'ALERTS{alertname="PostWebhookFailuresSurge", alertstate="firing", severity="P2"}'
            value: 1
  # ...and a redirecting endpoint (retry_no_streak) alone is not a surge.
  - interval: 1m
    input_series:
      - series: 'post_webhook_deliveries_total{outcome="retry_no_streak"}'
        values: '0 10 20 30 40 50 60 70 80 90 100 110 120 130'
      - series: 'post_webhook_deliveries_total{outcome="delivered"}'
        values: '0 1 2 3 4 5 6 7 8 9 10 11 12 13'
    promql_expr_test:
      - expr: 'ALERTS{alertname="PostWebhookFailuresSurge"}'
        eval_time: 14m
        exp_samples: []
  # a dead letter is terminal on its own; one is enough.
  - interval: 1m
    input_series:
      - series: 'post_queue_jobs_total{outcome="dead_lettered"}'
        values: '0 0 0 0 0 0 0 0 0 0 0 0 0 1'
    promql_expr_test:
      - expr: 'ALERTS{alertname="PostJobDeadLettered"}'
        eval_time: 14m
        exp_samples:
          - labels: 'ALERTS{alertname="PostJobDeadLettered", alertstate="firing", outcome="dead_lettered", severity="P2"}'
            value: 1
  # ...and healthy jobs, at the same rate as the dead letter above, are not a
  # dead letter. This is the case that would catch the rule degenerating into
  # "any traffic at all".
  - interval: 1m
    input_series:
      - series: 'post_queue_jobs_total{outcome="succeeded"}'
        values: '0 60 120 180 240 300 360 420 480 540 600 660 720 780 840'
      - series: 'post_queue_jobs_total{outcome="retried"}'
        values: '0 60 120 180 240 300 360 420 480 540 600 660 720 780 840'
    promql_expr_test:
      - expr: 'ALERTS{alertname="PostJobDeadLettered"}'
        eval_time: 14m
        exp_samples: []
  # ---- the scrape targets themselves -------------------------------------
  # The three cases below are about the "up" series, which is not a POST metric
  # at all:
  # it is Prometheus's own per-target scrape-health series, written by the
  # server from the result of each scrape. Nothing in this tree emits it, so
  # no scrape body above contains it and it cannot come from the generator —
  # synthesised input is the only kind available, and saying so is the point
  # of this block.
  #
  # What they are FOR: PostMetricsEndpointMissing is the absent() of post_db_up, and
  # post_db_up is emitted by both binaries, so it fires only when NEITHER
  # target answers. One process going dark — the state that silently removes
  # every rule fed by that process — left it quiet. PostMetricsTargetMissing
  # is the rule that sees it, and case B is the exact state the old rule could
  # not distinguish from health: it asserts BOTH that the new rule fires for
  # the job that vanished AND that the old one still does not (which is why
  # the old one is kept: it is the naming-independent backstop, not a
  # duplicate).
  #
  # case A — both targets configured and answering: quiet. The negative
  # control without which "it fired" would prove nothing about the threshold.
  - interval: 1m
    input_series:
      - series: 'up{job="post-api",instance="api:8091"}'
        values: '1 1 1 1 1 1 1 1 1 1 1 1 1 1 1'
      - series: 'up{job="post-worker",instance="worker:8092"}'
        values: '1 1 1 1 1 1 1 1 1 1 1 1 1 1 1'
      - series: 'post_db_up{instance="api:8091"}'
        values: '1 1 1 1 1 1 1 1 1 1 1 1 1 1 1'
    promql_expr_test:
      - expr: 'ALERTS{alertname="PostMetricsTargetMissing"}'
        eval_time: 14m
        exp_samples: []
  # case B — exactly one target stopped reporting: the api JOB is gone from
  # the scrape configuration while the worker still reports.
  - interval: 1m
    input_series:
      - series: 'up{job="post-worker",instance="worker:8092"}'
        values: '1 1 1 1 1 1 1 1 1 1 1 1 1 1 1'
      - series: 'post_db_up{instance="worker:8092"}'
        values: '1 1 1 1 1 1 1 1 1 1 1 1 1 1 1'
    promql_expr_test:
      - expr: 'ALERTS{alertname="PostMetricsTargetMissing"}'
        eval_time: 14m
        exp_samples:
          - labels: 'ALERTS{alertname="PostMetricsTargetMissing", alertstate="firing", job="post-api", severity="P1"}'
            value: 1
      # ...and the both-gone rule is still silent here, which is the defect
      # that made this rule necessary. If this assertion ever starts failing
      # because the old rule acquired the job-name dependency, the pair has
      # collapsed into one rule and the backstop is gone.
      - expr: 'ALERTS{alertname="PostMetricsEndpointMissing"}'
        eval_time: 14m
        exp_samples: []
  # case C — one target's SCRAPE is failing (configured, answering nothing).
  # Distinct from case B on purpose: an unreachable endpoint and a deleted job
  # produce different Prometheus state (up=0 vs no series), and only the
  # instance-labelled sample proves the first branch of the expression works.
  - interval: 1m
    input_series:
      - series: 'up{job="post-api",instance="api:8091"}'
        values: '1 1 0 0 0 0 0 0 0 0 0 0 0 0 0'
      - series: 'up{job="post-worker",instance="worker:8092"}'
        values: '1 1 1 1 1 1 1 1 1 1 1 1 1 1 1'
      - series: 'post_db_up{instance="worker:8092"}'
        values: '1 1 1 1 1 1 1 1 1 1 1 1 1 1 1'
    promql_expr_test:
      - expr: 'ALERTS{alertname="PostMetricsTargetMissing"}'
        eval_time: 14m
        exp_samples:
          - labels: 'ALERTS{alertname="PostMetricsTargetMissing", alertstate="firing", instance="api:8091", job="post-api", severity="P1"}'
            value: 1
YAML
"$PROMTOOL" test rules "$SELF_TEST" || fail "a synthesised-series rule check failed"
echo "   OK: the eleven synthesised cases behaved as the rules say they should"
rm -f "$SELF_TEST"

echo
echo ">> metrics-alerts-e2e PASSED"
echo ">> samples kept at: $SAMPLES"
echo ">> logs kept at:    $LOG"
