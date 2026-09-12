#!/usr/bin/env bash
#
# Unit tests for scripts/pg-ready.py.
#
# The defect these pin: the probe used to be a bare TCP connect, so it reported
# "PostgreSQL reachable" for anything that accepted a socket — including a
# PostgreSQL that was listening but still initialising, which is exactly when
# `make test-integration` first failed after `docker compose up` (probe green,
# test connection reset). Cases 1 and 2 are that false positive in two forms: a
# listener that closes at once, and one that accepts and stays silent. Both
# must be reported NOT ready.
#
# Case 4 proves the probe did not over-correct: a real server must still be
# reported ready. It runs against POSTGRES_TEST_ADMIN_URL (or the documented
# default), which is why this belongs to the integration stage — like
# `make test-integration` it fails loudly rather than skipping when no server
# is reachable.
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
PROBE="$ROOT/scripts/pg-ready.py"
URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

LISTENERS=()
cleanup() {
  local pid
  for pid in ${LISTENERS[@]+"${LISTENERS[@]}"}; do
    kill "$pid" 2>/dev/null || true
  done
  [[ -n "${WORK:-}" ]] && rm -rf "$WORK"
}
trap cleanup EXIT

# start_fake_listener MODE — starts a fake listener on an ephemeral port,
# echoes its pid, and writes the port to $WORK/$MODE.port.
#
# The listener's own output goes to a file, never to the caller's stdout: a
# background process inheriting a command-substitution pipe keeps it open, so
# capturing it would block until the listener exits.
start_fake_listener() {
  local mode="$1"
  python3 - "$mode" >"$WORK/$mode.port" 2>"$WORK/$mode.err" <<'PY' &
import socket, sys, time
mode = sys.argv[1]
srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
srv.bind(("127.0.0.1", 0))
srv.listen(4)
# Printed after listen(), so a caller that has read this line can connect.
print(srv.getsockname()[1], flush=True)
conn, _ = srv.accept()
if mode == "close":
    conn.close()
else:
    time.sleep(30)   # accept, then never answer: a server mid-startup
PY
  local pid=$!
  LISTENERS+=("$pid")
  echo "$pid"
}

# await_port MODE — blocks until the listener has published its port.
await_port() {
  local mode="$1"
  for _ in $(seq 1 200); do
    local p
    p="$(head -1 "$WORK/$mode.port" 2>/dev/null || true)"
    if [[ "$p" =~ ^[0-9]+$ ]]; then printf '%s' "$p"; return 0; fi
    sleep 0.05
  done
  return 1
}

run_negative_case() {
  local mode="$1" port rc out
  start_fake_listener "$mode" >/dev/null
  if ! port="$(await_port "$mode")"; then
    fail "$mode: the fake listener never published a port ($(cat "$WORK/$mode.err" 2>/dev/null))"
    return
  fi
  out="$(python3 "$PROBE" "postgres://u:p@127.0.0.1:$port/db" 2>&1)"
  rc=$?
  if (( rc == 0 )); then
    fail "$mode listener was reported READY — a socket that never speaks PostgreSQL is not a database: $out"
  else
    ok "$mode listener: rc=$rc, not reported ready"
  fi
}

WORK="$(mktemp -d)"

if [[ ! -f "$PROBE" ]]; then
  fail "scripts/pg-ready.py is missing"
  printf '\npg-ready-unit-test: %d failure(s)\n' "$FAILS"
  exit 1
fi

# 1 + 2: the false positive, in both shapes.
run_negative_case close
run_negative_case silent

# 3: a malformed URL is a usage error, not a readiness verdict.
rc=0
python3 "$PROBE" "not-a-url" >/dev/null 2>&1 || rc=$?
if (( rc == 2 )); then
  ok "malformed URL: rc=2 (usage error)"
else
  fail "malformed URL returned rc=$rc, want 2"
fi

# 4: a real server must still be reported ready.
rc=0
out="$(python3 "$PROBE" "$URL" 2>&1)" || rc=$?
if (( rc != 0 )); then
  fail "a real PostgreSQL at $URL was reported not ready — the probe over-corrected: $out"
else
  ok "real PostgreSQL ($URL): ready"
fi

printf '\n'
if (( FAILS )); then
  printf 'pg-ready-unit-test: %d failure(s)\n' "$FAILS"
  exit 1
fi
printf 'pg-ready-unit-test: all cases passed\n'
