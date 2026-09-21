#!/usr/bin/env bash
#
# G3 — the edge hardening, against REAL services (T1106).
#
# docs/67 G3: cross-system paths are driven against real containers, not
# substitutes. This script therefore uses, with nothing mocked:
#
#   * the real API binary (cmd/api), built from THIS tree and started as a
#     process on a real socket;
#   * the real PostgreSQL from the infra stack, with THIS tree's own
#     migrations applied to a database this run creates and drops;
#   * the real Redis from the infra stack — the edge's counters are the
#     production adapter (persistence.RedisRateLimiter), not a fake;
#   * real HTTP through curl, so every assertion is about bytes that
#     actually crossed a socket.
#
# It is the whole T1106 surface in one command, which is also the command
# the `security-smoke` G3 job runs (specs/orchestrator/gates.json). Four
# things it is here to prove that no unit test can:
#
#   1. the header set is on every response of a running process, with the
#      exact values, and the Server token carries no version;
#   2. CORS answers the configured web origin and refuses every other one,
#      including a preflight;
#   3. the rate limiter refuses with 429 + Retry-After at the documented
#      boundary, counting in real Redis, and it FAILS CLOSED — a second
#      API instance pointed at a dead Redis answers 503 on every limited
#      route while /healthz keeps answering 200;
#   4. an anonymous write is 401, and a rendered page is a document with
#      the document CSP.
#
# It fails loudly when a service is missing. It never skips: a G3 that
# quietly becomes a no-op is worse than no G3, because the gate then
# reports green.
set -uo pipefail

# ROOT is normally the tree this script lives in, which is what a gate
# wants: `rddev gate run G3` executes steps in the task's worktree, so the
# script builds and starts THAT tree's code. G3_REPO_ROOT exists for the one
# case the gate cannot express — auditing an already-collected worktree
# whose baseline predates this file — and is explicit precisely so it
# cannot be used by accident.
ROOT="${G3_REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
cd "$ROOT"

PG_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"
REDIS_ADDR="${POST_G3_REDIS_ADDR:-127.0.0.1:6379}"
WEB_ORIGIN="${POST_G3_WEB_ORIGIN:-http://127.0.0.1:3000}"
API_PORT="${POST_G3_SECURITY_API_PORT:-18082}"
API_ADDR="127.0.0.1:${API_PORT}"
# A second instance, pointed at a port nothing listens on: the fail-closed
# case. 127.0.0.1:1 is reserved and refuses instantly, so the limiter's
# error path is reached rather than waited on.
DEAD_REDIS_ADDR="127.0.0.1:1"
DEAD_API_PORT="${POST_G3_SECURITY_DEAD_PORT:-18083}"
DEAD_API_ADDR="127.0.0.1:${DEAD_API_PORT}"

# The budgets this run measures. They are set explicitly rather than taken
# from the defaults so the boundary below is arithmetic and not a guess:
# with CRED=5 and one signup already spent, the sixth credential request —
# the fifth login attempt — is the first refusal.
WINDOW="1m"
WINDOW_SECONDS=60
ANON_PER_IP="240"
CRED_PER_IP=5
# authn has its own budgets inside the service (per email, per IP). They are
# raised here so that the limit this script measures is the EDGE's, and a
# 429 below cannot be some other layer's.
AUTHN_LOGIN_PER_EMAIL=100
AUTHN_LOGIN_PER_IP=100

# The values internal/security stamps, spelled out so this script compares
# the response to a literal rather than to the code that produced it.
# internal/security/headers_test.go is what keeps these strings and the
# constants in agreement; if they ever drift, that test fails first.
HDR_NOSNIFF="nosniff"
HDR_FRAME="DENY"
HDR_REFERRER="no-referrer"
HDR_SERVER="post"
HDR_HSTS="max-age=31536000; includeSubDomains"
HDR_PERMISSIONS="accelerometer=(), autoplay=(), camera=(), display-capture=(), encrypted-media=(), fullscreen=(), geolocation=(), gyroscope=(), magnetometer=(), microphone=(), midi=(), payment=(), picture-in-picture=(), publickey-credentials-get=(), screen-wake-lock=(), usb=(), xr-spatial-tracking=()"
CSP_LOCKDOWN="default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; sandbox allow-downloads"
CSP_DOCUMENT="default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; script-src 'none'; style-src 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:"

WORK="$(mktemp -d)"
API_PID=""
DEAD_API_PID=""
SCRATCH_DB=""
cleanup() {
  [[ -n "$API_PID" ]] && kill "$API_PID" 2>/dev/null
  [[ -n "$DEAD_API_PID" ]] && kill "$DEAD_API_PID" 2>/dev/null
  # The one database this run may destroy is the one it created itself.
  [[ -n "$SCRATCH_DB" ]] && psql "$PG_URL" -q -c \
    "DROP DATABASE IF EXISTS \"$SCRATCH_DB\" WITH (FORCE)" >/dev/null 2>&1
  rm -rf "$WORK" "$ROOT/bin/t1106migrate"
}
trap cleanup EXIT

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

# --- header helpers ----------------------------------------------------------
#
# Values are read out of the header block curl wrote to a file, never out of
# the body: a CSP in a body is a string, and a CSP in a header block is a
# policy the browser obeyed.
hdr() { # hdr <header-file> <name> -> first value, or nothing
  local line
  line="$(tr -d '\r' <"$1" | grep -i -m1 "^$2:")" || return 1
  printf '%s' "${line#*:}" | sed 's/^[[:space:]]*//'
}
has_hdr() { tr -d '\r' <"$1" | grep -qi "^$2:"; }

expect_hdr() { # expect_hdr <file> <name> <want> <label>
  local got
  got="$(hdr "$1" "$2")"
  if [[ "$got" == "$3" ]]; then
    ok "$4: $2: $3"
  else
    fail "$4: $2 is '$got', want '$3'"
  fi
}
expect_absent() { # expect_absent <file> <name> <label>
  if has_hdr "$1" "$2"; then
    fail "$2: $3: header is present ('$(hdr "$1" "$2")'), want it absent"
  else
    ok "$3: no $2"
  fi
}
expect_status() { # expect_status <got> <want> <label>
  if [[ "$1" == "$2" ]]; then ok "$3: $1"; else fail "$3: status $1, want $2"; fi
}
body_field() { # body_field <file> <key>
  python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get(sys.argv[2],""))' "$1" "$2" 2>/dev/null
}

# --- preconditions: real services, or refuse --------------------------------
if ! python3 scripts/pg-ready.py "$PG_URL" >/dev/null 2>&1; then
  echo "G3 security-smoke: FAILED — no PostgreSQL accepting connections at $PG_URL." >&2
  echo "G3 requires real services and does NOT skip. Start them: make infra-up && make infra-init" >&2
  exit 1
fi
if ! (exec 3<>"/dev/tcp/${REDIS_ADDR%:*}/${REDIS_ADDR#*:}") 2>/dev/null; then
  echo "G3 security-smoke: FAILED — no Redis at $REDIS_ADDR (start it: make infra-up)" >&2
  exit 1
fi
{ exec 3<&-; } 2>/dev/null || true
command -v psql >/dev/null 2>&1 || {
  echo "G3 security-smoke: FAILED — psql is required to give this run its own database" >&2
  exit 1
}
command -v curl >/dev/null 2>&1 || {
  echo "G3 security-smoke: FAILED — curl is required to make the requests" >&2
  exit 1
}
ok "preconditions: PostgreSQL at $PG_URL, Redis at $REDIS_ADDR"

# --- this run's own database --------------------------------------------------
#
# Created here and dropped on the way out. A gate that grades a tree must not
# be reading another tree's leftovers: the shared dev database is whatever the
# last tree to run left behind, and Goose refuses a tree whose migrations are
# not a superset of that database's version.
SCRATCH_DB="post_g3sec_$(date +%s)_$$"
if ! psql "$PG_URL" -q -v ON_ERROR_STOP=1 -c "CREATE DATABASE \"$SCRATCH_DB\"" >/dev/null 2>&1; then
  echo "G3 security-smoke: FAILED — could not create this run's own database ($SCRATCH_DB) on the admin endpoint (make infra-up && make infra-init)" >&2
  exit 1
fi
mapfile -t DB < <(SCRATCH_DB="$SCRATCH_DB" PG_URL="$PG_URL" python3 -c '
import os, urllib.parse as u
p = u.urlparse(os.environ["PG_URL"])
q = u.parse_qs(p.query)
print(u.urlunparse(p._replace(path="/" + os.environ["SCRATCH_DB"])))
print(p.hostname or "127.0.0.1")
print(p.port or 5432)
print(u.unquote(p.username or "postgres"))
print(u.unquote(p.password or ""))
print((q.get("sslmode") or ["disable"])[0])
')
if [[ "${#DB[@]}" -ne 6 ]]; then
  echo "G3 security-smoke: FAILED — could not read the connection parts out of the admin URL" >&2
  exit 1
fi
SCRATCH_URL="${DB[0]}"; DB_HOST="${DB[1]}"; DB_PORT="${DB[2]}"
DB_USER="${DB[3]}"; DB_PASSWORD="${DB[4]}"; DB_SSLMODE="${DB[5]}"

# The migrations are applied by THIS tree's own code. The helper lives under
# bin/, which the repository root .gitignore excludes, so it cannot disturb
# the review's fingerprint; its name is this task's so it cannot collide with
# another gate script that writes its own helper there.
mkdir -p "$ROOT/bin/t1106migrate"
cat >"$ROOT/bin/t1106migrate/main.go" <<'GOMIGRATE'
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lichman0405/post/internal/persistence"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: t1106migrate postgres://…")
		os.Exit(2)
	}
	n, err := persistence.Migrate(context.Background(), os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%d migration(s) applied\n", n)
}
GOMIGRATE
if ! go run ./bin/t1106migrate "$SCRATCH_URL" >"$WORK/migrate.log" 2>&1; then
  fail "migrating $SCRATCH_DB with $ROOT's own migrations: $(tail -3 "$WORK/migrate.log")"
  printf '\nG3 security-smoke: %d failure(s)\n' "$FAILS"
  exit 1
fi
ok "migrated $SCRATCH_DB: $(tail -1 "$WORK/migrate.log")"

# --- the two API processes ----------------------------------------------------
if ! go build -o "$WORK/api" ./cmd/api >"$WORK/build.log" 2>&1; then
  fail "building cmd/api: $(tail -5 "$WORK/build.log")"
  printf '\nG3 security-smoke: %d failure(s)\n' "$FAILS"
  exit 1
fi
ok "built cmd/api from $ROOT"

API_LOG="$WORK/api.log"
DEAD_LOG="$WORK/api-dead-redis.log"

start_api() { # start_api <addr> <redis-addr> <logfile> -> pid on stdout
  local addr="$1" redis="$2" log="$3"
  env POST_ENV=test \
      POST_API_ADDR="$addr" \
      POST_REDIS_ADDR="$redis" \
      POST_DB_HOST="$DB_HOST" POST_DB_PORT="$DB_PORT" POST_DB_USER="$DB_USER" \
      POST_DB_PASSWORD="$DB_PASSWORD" POST_DB_SSLMODE="$DB_SSLMODE" POST_DB_NAME="$SCRATCH_DB" \
      POST_BLOB_ACCESS_KEY=g3-ak POST_BLOB_SECRET_KEY=g3-sk \
      POST_GITEA_TOKEN=g3-tok \
      POST_WEB_ORIGIN="$WEB_ORIGIN" \
      POST_SECURITY_RATE_LIMIT_WINDOW="$WINDOW" \
      POST_SECURITY_RATE_LIMIT_ANONYMOUS_PER_IP="$ANON_PER_IP" \
      POST_SECURITY_RATE_LIMIT_CREDENTIAL_PER_IP="$CRED_PER_IP" \
      POST_AUTH_LOGIN_PER_EMAIL="$AUTHN_LOGIN_PER_EMAIL" \
      POST_AUTH_LOGIN_PER_IP="$AUTHN_LOGIN_PER_IP" \
      "$WORK/api" >>"$log" 2>&1 &
  local pid=$!
  for _ in $(seq 1 60); do
    # --max-time: curl has no default one, so against a socket that accepts
    # the connection and then says nothing this waits forever and the
    # kill -0 that notices the process died never gets a turn — the gate
    # would hang instead of failing, and a hang reports nothing.
    curl -fsS --max-time 2 "http://$addr/healthz" >/dev/null 2>&1 && { printf '%s' "$pid"; return 0; }
    kill -0 "$pid" 2>/dev/null || return 1
    sleep 0.5
  done
  return 1
}

if ! API_PID="$(start_api "$API_ADDR" "$REDIS_ADDR" "$API_LOG")"; then
  API_PID=""
  fail "the API did not become healthy on $API_ADDR: $(tail -5 "$API_LOG")"
  printf '\nG3 security-smoke: %d failure(s)\n' "$FAILS"
  exit 1
fi
ok "real API process healthy on $API_ADDR (PostgreSQL + Redis from the infra stack)"
grep -q "post-api: edge rate limiting active" "$API_LOG" \
  && ok "the process logged its own policy: $(grep -m1 'edge rate limiting active' "$API_LOG" | sed 's/.*policy=//')" \
  || fail "the API never logged that the edge rate limiter is active"

# --- S0: reset this run's own edge counters -----------------------------------
#
# The counters live in the shared Redis under the edge's "edge:" namespace,
# keyed by client IP — and every G3 job on this box talks to the API from
# 127.0.0.1. Another gate run's traffic therefore lands in the same buckets,
# which would turn the boundary assertion below into a measurement of
# someone else's requests. The namespace is the edge's alone (authn counts
# under "login:" and "signup:"), the keys expire within the window anyway,
# and this script is the only writer of the ones it deletes.
#
# The stored key is the bucket with the adapter's own prefix on it —
# internal/persistence/auth_redis.go:83 limiterKey() = "post:ratelimit:" +
# bucket. DEL of a name that does not exist returns 0 and exits 0, so the
# reset is READ BACK rather than assumed: a reset that silently did nothing
# is how the boundary below would become a measurement of the previous run.
RCLI_PREFIX="post:ratelimit:"
CRED_KEY="${RCLI_PREFIX}edge:cred:ip:127.0.0.1"
ANON_KEY="${RCLI_PREFIX}edge:anon:ip:127.0.0.1"
if command -v redis-cli >/dev/null 2>&1; then
  RCLI=(redis-cli -h "${REDIS_ADDR%:*}" -p "${REDIS_ADDR#*:}")
  if ! "${RCLI[@]}" DEL "$CRED_KEY" "$ANON_KEY" >/dev/null 2>&1; then
    fail "S0 could not reach Redis to reset the edge counters, so the boundary below measures another run's traffic"
  elif [[ "$("${RCLI[@]}" EXISTS "$CRED_KEY" "$ANON_KEY" 2>/dev/null)" != "0" ]]; then
    fail "S0 the reset did not take effect: $CRED_KEY or $ANON_KEY still exists, so the boundary below measures another run's traffic"
  else
    ok "S0 reset this run's edge counters, verified by read-back ($CRED_KEY, $ANON_KEY)"
  fi
else
  fail "S0 redis-cli is required: without it the boundary below measures whatever another gate run left in the shared buckets"
fi

# --- S1: the header set on a real response, value for value -------------------
H="$WORK/s1.hdr"
code="$(curl -sS -D "$H" -o "$WORK/s1.json" -w '%{http_code}' "http://$API_ADDR/healthz")"
expect_status "$code" "200" "S1 GET /healthz"
expect_hdr "$H" "X-Content-Type-Options" "$HDR_NOSNIFF" "S1"
expect_hdr "$H" "X-Frame-Options" "$HDR_FRAME" "S1"
expect_hdr "$H" "Referrer-Policy" "$HDR_REFERRER" "S1"
expect_hdr "$H" "Permissions-Policy" "$HDR_PERMISSIONS" "S1"
expect_hdr "$H" "Strict-Transport-Security" "$HDR_HSTS" "S1"
expect_hdr "$H" "Server" "$HDR_SERVER" "S1"
# The JSON API is not a document: it gets the lockdown policy, whose sandbox
# directive is what stops a browser that navigated straight at a byte
# response from running anything. allow-downloads and not a bare sandbox is
# deliberate — a bare one blocks the attachment download the files routes
# exist to serve.
expect_hdr "$H" "Content-Security-Policy" "$CSP_LOCKDOWN" "S1"
expect_absent "$H" "X-Powered-By" "S1"
if grep -qE '^[Ss]erver:.*[0-9/]' <(tr -d '\r' <"$H"); then
  fail "S1 Server names a version: $(hdr "$H" Server)"
else
  ok "S1 Server carries no version and no product detail beyond the token"
fi

# --- S2: CORS answers one origin and refuses every other ----------------------
H="$WORK/s2.hdr"
code="$(curl -sS -D "$H" -o "$WORK/s2.json" -w '%{http_code}' \
  -H 'Origin: https://evil.example' "http://$API_ADDR/api/v1/projects")"
expect_absent "$H" "Access-Control-Allow-Origin" "S2 hostile-origin GET"
expect_absent "$H" "Access-Control-Allow-Credentials" "S2 hostile-origin GET"
[[ "$code" -lt 500 ]] && ok "S2 hostile-origin GET answered $code (no cross-origin grant)" \
  || fail "S2 hostile-origin GET answered $code: $(cat "$WORK/s2.json")"

H="$WORK/s2b.hdr"
code="$(curl -sS -D "$H" -o /dev/null -w '%{http_code}' -X OPTIONS \
  -H 'Origin: https://evil.example' -H 'Access-Control-Request-Method: POST' \
  "http://$API_ADDR/api/v1/projects")"
expect_status "$code" "204" "S2 hostile-origin preflight"
expect_absent "$H" "Access-Control-Allow-Origin" "S2 hostile-origin preflight"

H="$WORK/s2c.hdr"
code="$(curl -sS -D "$H" -o "$WORK/s2c.json" -w '%{http_code}' \
  -H "Origin: $WEB_ORIGIN" "http://$API_ADDR/api/v1/projects")"
expect_hdr "$H" "Access-Control-Allow-Origin" "$WEB_ORIGIN" "S2 configured web origin"
expect_hdr "$H" "Access-Control-Allow-Credentials" "true" "S2 configured web origin"
if [[ "$(hdr "$H" Access-Control-Allow-Origin)" == "*" ]]; then
  fail "S2 the credentialed route answered a wildcard ACAO"
else
  ok "S2 the credentialed route echoes one origin, never *"
fi
[[ "$code" -lt 500 ]] && ok "S2 configured-origin GET answered $code" \
  || fail "S2 configured-origin GET answered $code: $(cat "$WORK/s2c.json")"

# --- S3: an anonymous write is 401 with the canonical envelope ----------------
H="$WORK/s3.hdr"
code="$(curl -sS -D "$H" -o "$WORK/s3.json" -w '%{http_code}' -X POST \
  -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' -d '{}' \
  "http://$API_ADDR/api/v1/projects")"
expect_status "$code" "401" "S3 anonymous write"
if [[ "$(hdr "$H" Content-Type)" == "application/json" ]]; then
  ok "S3 the refusal is JSON"
else
  fail "S3 the refusal has Content-Type '$(hdr "$H" Content-Type)', want application/json"
fi
if [[ "$(body_field "$WORK/s3.json" code)" == "AUTH_UNAUTHENTICATED" ]]; then
  ok "S3 the refusal carries code=AUTH_UNAUTHENTICATED"
else
  fail "S3 the refusal's code is '$(body_field "$WORK/s3.json" code)': $(cat "$WORK/s3.json")"
fi
expect_hdr "$H" "X-Content-Type-Options" "$HDR_NOSNIFF" "S3"

# --- S4: a rendered page is a document with the document CSP ------------------
#
# An unknown project id is the deterministic page: the read surface hides
# existence, so it renders the neutral not-found DOCUMENT. No fixture, and
# the assertion is still about a real response from the real mux.
H="$WORK/s4.hdr"
code="$(curl -sS -D "$H" -o "$WORK/s4.html" -w '%{http_code}' \
  -H 'Accept: text/html' \
  "http://$API_ADDR/api/v1/projects/11111111-2222-4333-8444-555555555555/overview")"
expect_status "$code" "404" "S4 unknown project, text/html"
expect_hdr "$H" "Content-Type" "text/html; charset=utf-8" "S4"
expect_hdr "$H" "Content-Security-Policy" "$CSP_DOCUMENT" "S4"
expect_hdr "$H" "X-Content-Type-Options" "$HDR_NOSNIFF" "S4"
grep -qi "<html" "$WORK/s4.html" && ok "S4 the body really is a document" \
  || fail "S4 the body is not HTML: $(head -c 120 "$WORK/s4.html")"

# The same route without the Accept header is the JSON contract: the policy
# follows the response's own Content-Type, not the route.
H="$WORK/s4b.hdr"
curl -sS -D "$H" -o /dev/null "http://$API_ADDR/api/v1/projects/11111111-2222-4333-8444-555555555555/overview"
expect_hdr "$H" "Content-Security-Policy" "$CSP_LOCKDOWN" "S4 same route, JSON"

# --- S5: a real account, a real session, a real project, a real page ----------
EMAIL="g3sec-$(date +%s)-$$@example.test"
PASSWORD="g3sec-pass-$$-correct-horse"
JAR="$WORK/cookies.txt"
H="$WORK/s5.hdr"
code="$(curl -sS -c "$JAR" -b "$JAR" -D "$H" -o "$WORK/s5-signup.json" -w '%{http_code}' -X POST \
  -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\",\"handle\":\"g3sec$$\",\"display_name\":\"G3 Security Smoke\"}" \
  "http://$API_ADDR/api/v1/auth/signup")"
if [[ "$code" == "201" || "$code" == "200" ]]; then
  ok "S5 signup from the web origin -> $code"
else
  fail "S5 signup returned $code: $(cat "$WORK/s5-signup.json")"
fi
CSRF="$(body_field "$WORK/s5-signup.json" csrf_token)"
[[ -n "$CSRF" ]] && ok "S5 the signup returned a CSRF token" \
  || fail "S5 the signup returned no CSRF token: $(cat "$WORK/s5-signup.json")"
grep -q "post_session" "$JAR" && ok "S5 the session cookie was set" \
  || fail "S5 no session cookie in the jar"

code="$(curl -sS -c "$JAR" -b "$JAR" -o "$WORK/s5-session.json" -w '%{http_code}' \
  "http://$API_ADDR/api/v1/auth/session")"
expect_status "$code" "200" "S5 GET /auth/session with the cookie"

H="$WORK/s5-project.hdr"
code="$(curl -sS -c "$JAR" -b "$JAR" -D "$H" -o "$WORK/s5-project.json" -w '%{http_code}' -X POST \
  -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF" -H 'Idempotency-Key: t1106-smoke-$$' \
  -d "{\"slug\":\"g3sec-$$\",\"name\":\"G3 security smoke $$\",\"purpose\":\"T1106 edge assertions\",\"visibility\":\"private\"}" \
  "http://$API_ADDR/api/v1/projects")"
PROJECT_ID="$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("project",{}).get("id",""))' "$WORK/s5-project.json" 2>/dev/null)"
if [[ "$code" == "201" && -n "$PROJECT_ID" ]]; then
  ok "S5 created a real project ($PROJECT_ID) with a session + CSRF token"
else
  fail "S5 project create returned $code: $(cat "$WORK/s5-project.json")"
fi
# A credentialed route carries the same header set: the middleware is not
# something only the unauthenticated surface passes through.
expect_hdr "$H" "X-Content-Type-Options" "$HDR_NOSNIFF" "S5 credentialed write"
expect_hdr "$H" "Server" "$HDR_SERVER" "S5 credentialed write"

if [[ -n "$PROJECT_ID" ]]; then
  H="$WORK/s5-page.hdr"
  code="$(curl -sS -c "$JAR" -b "$JAR" -D "$H" -o "$WORK/s5-page.html" -w '%{http_code}' \
    -H 'Accept: text/html' "http://$API_ADDR/api/v1/projects/$PROJECT_ID/overview")"
  expect_status "$code" "200" "S5 the created project's overview page"
  expect_hdr "$H" "Content-Type" "text/html; charset=utf-8" "S5 overview page"
  expect_hdr "$H" "Content-Security-Policy" "$CSP_DOCUMENT" "S5 overview page"
  expect_hdr "$H" "X-Content-Type-Options" "$HDR_NOSNIFF" "S5 overview page"
  expect_hdr "$H" "Server" "$HDR_SERVER" "S5 overview page"
fi

# --- S6: the credential boundary, counted in real Redis -----------------------
#
# One signup has been spent above, so the fifth login attempt is the sixth
# credential request and the first refusal. The count is arithmetic only
# because S0 cleared the bucket; that is what S0 is for.
FIRST_429=0
# Counted in the loop rather than sliced out of the array afterwards: an
# empty slice makes printf print one empty line, which every count below
# would then misread as a status.
ADMITTED_BAD=0   # a non-429 attempt that answered neither 2xx nor 4xx
LATER_ADMITTED=0 # an attempt after the first refusal that was not refused
STATUSES=()
for i in $(seq 1 $((CRED_PER_IP + 3))); do
  code="$(curl -sS -D "$WORK/s6-$i.hdr" -o "$WORK/s6-$i.json" -w '%{http_code}' -X POST \
    -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$EMAIL\",\"password\":\"wrong-password-$i\"}" \
    "http://$API_ADDR/api/v1/auth/login")"
  STATUSES+=("$code")
  if [[ "$code" == "429" ]]; then
    [[ "$FIRST_429" -eq 0 ]] && FIRST_429="$i"
  else
    [[ "$FIRST_429" -ne 0 ]] && LATER_ADMITTED=$((LATER_ADMITTED + 1))
    [[ "$code" =~ ^[24] ]] || ADMITTED_BAD=$((ADMITTED_BAD + 1))
  fi
done

if [[ "$FIRST_429" -eq 0 ]]; then
  fail "S6 no attempt was refused within $((CRED_PER_IP + 3)): the edge budget of $CRED_PER_IP/min did not fire (statuses: ${STATUSES[*]})"
else
  ok "S6 the credential budget fired on attempt $FIRST_429 (statuses: ${STATUSES[*]})"
  # The documented boundary: $CRED_PER_IP requests per window, and this run
  # spent exactly one of them on the signup above.
  EXPECTED_FIRST=$((CRED_PER_IP))
  if [[ "$FIRST_429" -eq "$EXPECTED_FIRST" ]]; then
    ok "S6 exactly $((CRED_PER_IP - 1)) logins were admitted after the signup, then the refusal (N+1 at N=$CRED_PER_IP)"
  else
    fail "S6 the first refusal was attempt $FIRST_429, want $EXPECTED_FIRST after the signup spent one of $CRED_PER_IP"
  fi
  # Once refused, still refused: a limiter that lets the next request through
  # is not a limiter.
  [[ "$LATER_ADMITTED" -eq 0 ]] && ok "S6 every attempt after the first refusal was also refused" \
    || fail "S6 $LATER_ADMITTED attempt(s) after the first 429 were admitted again"

  H429="$WORK/s6-$FIRST_429.hdr"
  B429="$WORK/s6-$FIRST_429.json"
  RA="$(hdr "$H429" Retry-After)"
  if [[ "$RA" =~ ^[1-9][0-9]*$ ]] && (( RA >= 1 && RA <= WINDOW_SECONDS )); then
    ok "S6 the refusal carries Retry-After: $RA (1..$WINDOW_SECONDS for a $WINDOW window)"
  else
    fail "S6 Retry-After is '$RA', want an integer in 1..$WINDOW_SECONDS"
  fi
  expect_hdr "$H429" "X-Content-Type-Options" "$HDR_NOSNIFF" "S6 the refusal"
  if [[ "$(body_field "$B429" code)" == "RATE_LIMITED" ]]; then
    ok "S6 the refusal's envelope carries code=RATE_LIMITED"
  else
    fail "S6 the refusal's code is '$(body_field "$B429" code)': $(cat "$B429")"
  fi
  if [[ "$(body_field "$B429" retryable)" == "True" ]]; then
    ok "S6 the refusal is marked retryable"
  else
    fail "S6 the refusal's retryable is '$(body_field "$B429" retryable)': $(cat "$B429")"
  fi
  # A refusal is a message to a client, not a diagnostic: the limiter's own
  # error text names the address it could not reach and must never travel.
  if grep -qiE 'redis|6379|127\.0\.0\.1:1' "$B429"; then
    fail "S6 the refusal body names the limiter's backing store: $(cat "$B429")"
  else
    ok "S6 the refusal body names no backing store"
  fi
  # The admitted attempts really ran product logic: the wrong password is a
  # 401 from the auth service, not a 500 from a broken process. This is what
  # separates "the limiter refused" from "everything is broken".
  if [[ "$ADMITTED_BAD" -eq 0 ]]; then
    ok "S6 every admitted attempt answered from the auth service (401 wrong password)"
  else
    fail "S6 $ADMITTED_BAD admitted login attempt(s) answered neither 2xx nor 4xx: ${STATUSES[*]}"
  fi
fi

# --- S7: fail closed, and the probes stay up ----------------------------------
#
# A second API instance whose Redis does not exist. The limiter's counters are
# unknowable there, so every limited route must be REFUSED — never waved
# through — while /healthz keeps answering, because an orchestrator that reads
# a rate limiter as health will restart a healthy fleet.
if ! DEAD_API_PID="$(start_api "$DEAD_API_ADDR" "$DEAD_REDIS_ADDR" "$DEAD_LOG")"; then
  DEAD_API_PID=""
  fail "S7 the API pointed at a dead Redis ($DEAD_REDIS_ADDR) did not start: $(tail -5 "$DEAD_LOG")"
else
  ok "S7 a second API process is up on $DEAD_API_ADDR with POST_REDIS_ADDR=$DEAD_REDIS_ADDR"
  code="$(curl -sS -o "$WORK/s7-probe.json" -w '%{http_code}' "http://$DEAD_API_ADDR/healthz")"
  expect_status "$code" "200" "S7 /healthz is exempt and does not touch the limiter"

  H="$WORK/s7.hdr"
  code="$(curl -sS -D "$H" -o "$WORK/s7.json" -w '%{http_code}' "http://$DEAD_API_ADDR/api/v1/projects")"
  expect_status "$code" "503" "S7 a limited route with a dead limiter fails CLOSED"
  if [[ "$(body_field "$WORK/s7.json" code)" == "SERVICE_UNAVAILABLE" ]]; then
    ok "S7 fail-closed envelope carries code=SERVICE_UNAVAILABLE"
  else
    fail "S7 fail-closed envelope's code is '$(body_field "$WORK/s7.json" code)': $(cat "$WORK/s7.json")"
  fi
  expect_hdr "$H" "Server" "$HDR_SERVER" "S7 the fail-closed response"
  if grep -qiE 'redis|6379|127\.0\.0\.1:1' "$WORK/s7.json"; then
    fail "S7 the fail-closed body names the limiter's backing store: $(cat "$WORK/s7.json")"
  else
    ok "S7 the fail-closed body names no backing store"
  fi
  # And the refusal reached nobody's handler: a write on that instance is 503
  # too, not a 401 — the limiter runs before the auth guard on purpose.
  code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST \
    -H 'Content-Type: application/json' -d '{}' "http://$DEAD_API_ADDR/api/v1/projects")"
  expect_status "$code" "503" "S7 an anonymous write on the dead-limiter instance"
fi

# --- S8: the guards, in the tree under test -----------------------------------
if go test ./internal/security/... ./tests/security/... >"$WORK/gotest.log" 2>&1; then
  ok "S8 go test ./internal/security/... ./tests/security/... ($(grep -c '^ok' "$WORK/gotest.log") package(s))"
else
  fail "S8 the security guard tests failed: $(tail -20 "$WORK/gotest.log")"
fi

# The web half: the CSP and the header set in the Next config, and the
# raw-HTML guard over apps/web. Node runs the TypeScript config directly
# through type stripping (its only import of `next` is a type import).
if command -v node >/dev/null 2>&1; then
  if node --disable-warning=MODULE_TYPELESS_PACKAGE_JSON --test \
      apps/web/lib/security-headers.test.mjs >"$WORK/nodetest.log" 2>&1; then
    ok "S8 node --test apps/web/lib/security-headers.test.mjs ($(grep -c '^✔' "$WORK/nodetest.log") case(s))"
  else
    fail "S8 the web security-header test failed: $(tail -20 "$WORK/nodetest.log")"
  fi
else
  fail "S8 node is required: the web CSP and header set are asserted by a Node test"
fi

# --- verdict ------------------------------------------------------------------
if [[ "$FAILS" -eq 0 ]]; then
  printf '\nG3 security-smoke: OK — edge headers, CORS, the credential boundary and fail-closed behaviour, against real services\n'
  exit 0
fi
printf '\nG3 security-smoke: %d failure(s)\n' "$FAILS"
exit 1
