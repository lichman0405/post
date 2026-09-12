#!/usr/bin/env bash
# T0007 evidence, phase 2: the canary sweep.
#
# Two canaries are configured as real secrets, every real path is exercised
# (startup config logging, health checks, job enqueue success and failure,
# config-validation error paths, go-redis failure chatter, shutdown), and
# the actual log output is swept for them:
#
#   CANARY_PLAIN   an arbitrary distinctive string (NOT secret-shaped). It
#                  proves the structural masking of Secret-typed config
#                  fields — the value never reaches any output regardless
#                  of its shape.
#   CANARY_SECRET  a secret-shaped token (matches the shared token regex).
#                  It additionally travels through non-secret error paths,
#                  where config.RedactForOutput is the only thing standing
#                  between it and a log line.
#
# The sweep is proven capable of failing FIRST: a planted log line carrying
# the raw canary is detected (the sweep fails), then removed, then the real
# sweep must find nothing.
#
# Run from anywhere:  bash tests/observability/canary-sweep.sh
set -euo pipefail

cd "$(dirname "$0")/../.."   # repo root
BIN="$(mktemp -d)/bin"
LOG="$(mktemp -d)"
mkdir -p "$BIN"

API_PORT=18191 REDIS_PORT=16391
API="http://127.0.0.1:$API_PORT"

CANARY_PLAIN="t0007-canary-9f7a3c21b5e8"
CANARY_SECRET="ghp_t0007canarya1b2c3d4e5f6"

# The canaries are configured as real secrets (plus the URL-shaped Gitea
# base URL so the RedactURL path runs against the live config log).
API_ENV=(POST_ENV=test POST_API_ADDR="127.0.0.1:$API_PORT"
  POST_MCP_ADDR=127.0.0.1:19291 POST_REDIS_ADDR="127.0.0.1:$REDIS_PORT"
  POST_DB_HOST=127.0.0.1 POST_DB_PORT=15433 POST_DB_PASSWORD="$CANARY_SECRET"
  POST_DB_SSLMODE=disable POST_BLOB_ACCESS_KEY="$CANARY_PLAIN"
  POST_BLOB_SECRET_KEY="$CANARY_SECRET" POST_GITEA_TOKEN="$CANARY_PLAIN"
  POST_GITEA_BASE_URL="http://gitea-user:$CANARY_SECRET@127.0.0.1:3000")

PIDS=()
cleanup() {
  # Whole process groups (setsid below): no leaked process may survive to
  # serve the next run's traffic into a stale log dir.
  for pid in "${PIDS[@]:-}"; do kill -- -"$pid" 2>/dev/null || kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# A port already in use means a leaked process from a previous run — the
# curls below would silently talk to that stale process and the evidence
# would be written into a log dir we are not looking at. Fail fast instead.
require_free_port() { # port
  if (echo >/dev/tcp/127.0.0.1/"$1") 2>/dev/null; then
    fail "port $1 already in use (leaked process from a previous run?); free it before rerunning"
  fi
}
for p in "$API_PORT" "$REDIS_PORT"; do require_free_port "$p"; done

wait_for() {
  local desc="$1"; shift
  for _ in $(seq 1 120); do "$@" >/dev/null 2>&1 && return 0; sleep 0.5; done
  fail "timed out waiting for $desc"
}

echo ">> canaries:"
echo "   CANARY_PLAIN   = $CANARY_PLAIN  (arbitrary string; structural Secret masking must hold)"
echo "   CANARY_SECRET  = $CANARY_SECRET (secret-shaped; RedactForOutput paths must mask it)"

echo ">> building api, worker, devredis"
go build -o "$BIN" ./cmd/api ./cmd/worker ./cmd/devredis

echo ">> starting the real processes with the canaries configured as secrets"
setsid env "${API_ENV[@]}" "$BIN/devredis" -addr "127.0.0.1:$REDIS_PORT" >"$LOG/devredis.log" 2>&1 &
PIDS+=($!)
setsid env "${API_ENV[@]}" "$BIN/api" >"$LOG/api.log" 2>&1 &
PIDS+=($!)
setsid env "${API_ENV[@]}" "$BIN/worker" >"$LOG/worker.log" 2>&1 &
PIDS+=($!)
wait_for "api health" curl -fsS "$API/healthz"

echo ">> exercising the real paths"
curl -fsS "$API/healthz" >/dev/null
curl -fsS "$API/readyz" >/dev/null || true                      # PG password is the canary => probe reports down, truthfully
curl -sS -X POST "$API/internal/jobs" -H 'Content-Type: application/json' \
  -d '{"type":"smoke","payload":{"ref":"rsg/canary"}}' >/dev/null
curl -sS -X POST "$API/internal/jobs" -H 'Content-Type: application/json' \
  -d 'not json' >/dev/null || true                               # 400 path
curl -sS -X POST "$API/internal/jobs" -H 'Content-Type: application/json' \
  -d '{"type":"Bad Type!"}' >/dev/null || true                   # 400 path
curl -sS -X POST "$API/internal/jobs" -H 'Content-Type: application/json' \
  -d '{"type":"nope"}' >/dev/null                                # unknown type: worker dead-letters it
sleep 2                                                          # let the worker process the jobs
kill -- -"${PIDS[0]}" 2>/dev/null || kill "${PIDS[0]}" 2>/dev/null || true   # devredis down
sleep 1
curl -sS -X POST "$API/internal/jobs" -H 'Content-Type: application/json' \
  -d '{"type":"smoke"}' >/dev/null || true                       # 503 path, error text redacted
sleep 2
kill -- -"${PIDS[1]}" 2>/dev/null || kill "${PIDS[1]}" 2>/dev/null || true   # API shutdown against a dead redis
kill -- -"${PIDS[2]}" 2>/dev/null || kill "${PIDS[2]}" 2>/dev/null || true   # worker shutdown against a dead redis

echo ">> config-validation error paths (canary pasted into non-secret fields)"
set +e
env "${API_ENV[@]}" POST_DB_PORT="$CANARY_SECRET" "$BIN/api" >"$LOG/api-badport.log" 2>&1
set -e
set +e
env "${API_ENV[@]}" POST_GITEA_BASE_URL="not a url $CANARY_SECRET" "$BIN/api" >"$LOG/api-badurl.log" 2>&1
set -e
set +e
env "${API_ENV[@]}" POST_ENV="" "$BIN/api" >"$LOG/api-nolayer.log" 2>&1
set -e

echo ">> negative control: plant the raw canary and show the sweep catches it"
PLANTED="$LOG/planted.log"
echo "deliberate raw log of $CANARY_PLAIN and $CANARY_SECRET" >"$PLANTED"
if grep -RqF -e "$CANARY_PLAIN" -e "$CANARY_SECRET" "$LOG"; then
  echo "   sweep FAILED as expected: planted raw canary detected (the check is capable of failing)"
else
  fail "negative control broken: the sweep did not detect a planted raw canary"
fi
rm -f "$PLANTED"

echo ">> real sweep: no log output may contain either canary"
if grep -RqF -e "$CANARY_PLAIN" -e "$CANARY_SECRET" "$LOG"; then
  echo "   LEAK DETECTED:" >&2
  grep -RnF -e "$CANARY_PLAIN" -e "$CANARY_SECRET" "$LOG" >&2
  fail "canary found in real log output"
fi
echo "   sweep clean: neither canary appears in any log file"

echo ">> positive evidence: masking actually happened (values were logged masked, not dropped)"
grep -qF '"token":"***"' "$LOG/api.log" || fail "api startup log missing masked token"
grep -qF '"token":"***"' "$LOG/worker.log" || fail "worker startup log missing masked token"
grep -qF '"password":"***"' "$LOG/api.log" || fail "api startup log missing masked password"
grep -qF '"base_url":"http://gitea-user:***@127.0.0.1:3000"' "$LOG/api.log" \
  || fail "api startup log missing RedactURL'd gitea base URL"
grep -qF 'has an invalid value "***"' "$LOG/api-badport.log" \
  || fail "config error path did not mask the secret-shaped value"
grep -qF 'has an invalid value "***"' "$LOG/api-badurl.log" \
  || fail "config error path did not mask the secret-shaped value in a URL field"
echo "   masked forms present in api.log / worker.log / api-badport.log / api-badurl.log"

echo
echo ">> canary-sweep PASSED: no canary anywhere in real log output; sweep proven capable of failing"
echo ">> logs kept at: $LOG"
