#!/usr/bin/env bash
#
# G3 — the authentication journey against REAL services (T0101, docs/67).
#
# docs/67 G3: "跨系统链路使用真实容器服务 … auth/visibility". The auth task
# shipped with everything mocked at some layer: tests/e2e substitutes
# miniredis, the OIDC provider is an in-process fake, and — the defect that
# got the first attempt rejected — every test drove the API directly, so the
# browser's Origin always happened to match the request Host. The shipped web
# login could not authenticate against the shipped API and no test could see
# it.
#
# This script therefore uses, with nothing substituted:
#
#   * the real API binary (cmd/api), built and started as a process;
#   * the real PostgreSQL from the infra stack, migrated to head;
#   * the real Redis from the infra stack;
#   * real HTTP over a real socket, with the WEB's Origin and the API's Host
#     deliberately different — the cross-origin shape the default topology has.
#
# It fails loudly when a service is missing. It never skips: a G3 that quietly
# becomes a no-op is worse than no G3, because the gate then reports green.
set -uo pipefail

# ROOT is normally the tree this script lives in, which is what a gate wants:
# `rddev gate run G3` executes steps in the task's worktree, so the script
# builds and starts THAT tree's code. G3_REPO_ROOT exists for the one case the
# gate cannot express — auditing an already-collected worktree whose baseline
# predates this file — and is explicit precisely so it cannot be used by
# accident.
ROOT="${G3_REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
cd "$ROOT"

PG_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"
REDIS_ADDR="${POST_G3_REDIS_ADDR:-127.0.0.1:6379}"
WEB_ORIGIN="${POST_G3_WEB_ORIGIN:-http://127.0.0.1:3000}"
API_PORT="${POST_G3_API_PORT:-18081}"
API_ADDR="127.0.0.1:${API_PORT}"

WORK="$(mktemp -d)"
API_PID=""
cleanup() {
  [[ -n "$API_PID" ]] && kill "$API_PID" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

# --- preconditions: real services, or refuse ---------------------------------
if ! python3 scripts/pg-ready.py "$PG_URL" >/dev/null 2>&1; then
  echo "G3 auth-real-services: FAILED — no PostgreSQL accepting connections at $PG_URL." >&2
  echo "G3 requires real services and does NOT skip. Start them: make infra-up && make infra-init" >&2
  exit 1
fi
if ! (exec 3<>"/dev/tcp/${REDIS_ADDR%:*}/${REDIS_ADDR#*:}") 2>/dev/null; then
  echo "G3 auth-real-services: FAILED — no Redis at $REDIS_ADDR (start it: make infra-up)" >&2
  exit 1
fi
# Close the probe fd; the suppression is scoped to a group because a bare
# `exec 3<&- 2>/dev/null` redirects *this shell's* stderr for the rest of the
# script — every `FAILED … >&2` below would be written to /dev/null, and the
# gate log would show a step that failed with no output at all.
{ exec 3<&-; } 2>/dev/null || true

# --- the schema the API writes to --------------------------------------------
#
# The migrations are applied by THIS tree's own code, not by the Supervisor's
# copy elsewhere: a task's diff may add a migration (T0101 adds
# 00016_auth_password.sql), and applying someone else's migrations would leave
# the schema under test missing exactly the column the task is about.
#
# The helper lives under bin/, which the repository root .gitignore excludes,
# so it is invisible to `git status` and cannot disturb the planned review's
# fingerprint. `go run` from this directory resolves the import against this
# tree's module.
mkdir -p "$ROOT/bin/g3migrate"
cat >"$ROOT/bin/g3migrate/main.go" <<'GOMIGRATE'
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lichman0405/post/internal/persistence"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: g3migrate postgres://…")
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
if ! (cd "$ROOT" && go run ./bin/g3migrate "$PG_URL") >"$WORK/migrate.log" 2>&1; then
  rm -rf "$ROOT/bin/g3migrate"
  fail "migration to head against $PG_URL using $ROOT's own migrations: $(tail -3 "$WORK/migrate.log")"
  printf '\nG3 auth-real-services: %d failure(s)\n' "$FAILS"
  exit 1
fi
rm -rf "$ROOT/bin/g3migrate"
ok "migrated ($ROOT's own migration set): $(tail -1 "$WORK/migrate.log")"

# --- a real API process ------------------------------------------------------
if ! go build -o "$WORK/api" ./cmd/api >"$WORK/build.log" 2>&1; then
  fail "building cmd/api: $(tail -3 "$WORK/build.log")"
  printf '\nG3 auth-real-services: %d failure(s)\n' "$FAILS"
  exit 1
fi

start_api() {
  env POST_ENV=test \
      POST_API_ADDR="$API_ADDR" \
      POST_REDIS_ADDR="$REDIS_ADDR" \
      POST_DB_HOST=127.0.0.1 POST_DB_PORT=5432 \
      POST_DB_PASSWORD=postgres_dev_pw POST_DB_SSLMODE=disable \
      POST_BLOB_ACCESS_KEY=g3-ak POST_BLOB_SECRET_KEY=g3-sk \
      POST_GITEA_TOKEN=g3-tok \
      POST_WEB_ORIGIN="$WEB_ORIGIN" \
      "$WORK/api" >>"$WORK/api.log" 2>&1 &
  API_PID=$!
  for _ in $(seq 1 60); do
    curl -fsS "http://$API_ADDR/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$API_PID" 2>/dev/null || return 1
    sleep 0.5
  done
  return 1
}

stop_api() {
  [[ -n "$API_PID" ]] && kill "$API_PID" 2>/dev/null
  wait "$API_PID" 2>/dev/null
  API_PID=""
}

if ! start_api; then
  fail "the API did not become healthy on $API_ADDR: $(tail -5 "$WORK/api.log")"
  printf '\nG3 auth-real-services: %d failure(s)\n' "$FAILS"
  exit 1
fi
ok "real API process healthy on $API_ADDR (PostgreSQL + Redis from the infra stack)"

EMAIL="g3-$(date +%s)-$$@example.test"
PASSWORD="g3-pass-$$-correct-horse"
JAR="$WORK/cookies.txt"

# --- A1: an unauthenticated write is 401, not 404 and not 200 ----------------
code="$(curl -sS -o "$WORK/a1.json" -w '%{http_code}' -X POST \
  -H 'Content-Type: application/json' -d '{}' \
  "http://$API_ADDR/api/v1/projects")"
if [[ "$code" == "401" ]]; then
  ok "A1 unauthenticated write -> 401"
else
  fail "A1 POST /api/v1/projects without a session returned $code, want 401: $(cat "$WORK/a1.json")"
fi

# --- A2: the cross-origin journey the shipped web app actually makes ---------
# The Origin is the WEB origin while the request goes to the API host. This is
# the shape that broke the first attempt: a guard comparing Origin against
# r.Host rejects it. Nothing here uses a matching Origin.
code="$(curl -sS -c "$JAR" -b "$JAR" -o "$WORK/a2-signup.json" -w '%{http_code}' -X POST \
  -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\",\"handle\":\"g3user$$\",\"display_name\":\"G3 User\"}" \
  "http://$API_ADDR/api/v1/auth/signup")"
if [[ "$code" == "201" || "$code" == "200" ]]; then
  ok "A2 signup from the web origin ($WEB_ORIGIN -> $API_ADDR) -> $code"
else
  fail "A2 cross-origin signup returned $code, want 201/200 — the shipped web app could not sign up: $(cat "$WORK/a2-signup.json")"
fi

CSRF="$(python3 -c "import json,sys;print(json.load(open('$WORK/a2-signup.json')).get('csrf_token',''))" 2>/dev/null)"
code="$(curl -sS -c "$JAR" -b "$JAR" -o "$WORK/a2-session.json" -w '%{http_code}' "http://$API_ADDR/api/v1/auth/session")"
if [[ "$code" == "200" ]]; then
  ok "A2 GET /auth/session with the cookie -> 200"
else
  fail "A2 GET /auth/session returned $code: $(cat "$WORK/a2-session.json")"
fi
[[ -n "$CSRF" ]] && ok "A2 signup returned a CSRF token" || fail "A2 signup returned no csrf_token"

# --- A5 (before logout): the session is SERVER-side, really in Redis ---------
# Restarting the API is the assertion miniredis cannot make: an in-process
# store would lose the session, a real Redis keeps it.
stop_api
if ! start_api; then
  fail "the API did not come back up after a restart: $(tail -5 "$WORK/api.log")"
else
  code="$(curl -sS -c "$JAR" -b "$JAR" -o "$WORK/a5.json" -w '%{http_code}' "http://$API_ADDR/api/v1/auth/session")"
  if [[ "$code" == "200" ]]; then
    ok "A5 session survives an API restart -> server-side state in real Redis"
  else
    fail "A5 the session did not survive an API restart ($code) — sessions are not in Redis: $(cat "$WORK/a5.json")"
  fi
fi

# --- A2 cont: logout is CSRF-protected and really revokes --------------------
code="$(curl -sS -c "$JAR" -b "$JAR" -o "$WORK/a2-logout-nocsrf.json" -w '%{http_code}' -X POST \
  -H "Origin: $WEB_ORIGIN" "http://$API_ADDR/api/v1/auth/logout")"
if [[ "$code" == "403" ]]; then
  ok "A2 logout without the CSRF token -> 403"
else
  fail "A2 logout without a CSRF token returned $code, want 403"
fi

code="$(curl -sS -c "$JAR" -b "$JAR" -o /dev/null -w '%{http_code}' -X POST \
  -H "Origin: $WEB_ORIGIN" -H "X-CSRF-Token: $CSRF" "http://$API_ADDR/api/v1/auth/logout")"
if [[ "$code" == "204" || "$code" == "200" ]]; then
  ok "A2 logout with the CSRF token -> $code"
else
  fail "A2 logout with a valid CSRF token returned $code, want 204/200"
fi

code="$(curl -sS -c "$JAR" -b "$JAR" -o /dev/null -w '%{http_code}' "http://$API_ADDR/api/v1/auth/session")"
if [[ "$code" == "401" ]]; then
  ok "A2 the session is gone after logout -> 401 (revoked server-side)"
else
  fail "A2 the session still works after logout ($code) — logout did not revoke it"
fi

# --- A3: enumeration — unknown email and wrong password are indistinguishable -
bodies=()
for payload in \
  "{\"email\":\"nobody-$EMAIL\",\"password\":\"$PASSWORD\"}" \
  "{\"email\":\"$EMAIL\",\"password\":\"definitely-not-the-password\"}" ; do
  code="$(curl -sS -o "$WORK/a3-$RANDOM.json" -w '%{http_code}' -X POST \
    -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
    -d "$payload" "http://$API_ADDR/api/v1/auth/login")"
  f="$(ls -t "$WORK"/a3-*.json | head -1)"
  bodies+=("$code:$(python3 -c "
import json;d=json.load(open('$f'));d.pop('request_id',None);print(json.dumps(d,sort_keys=True))
" 2>/dev/null)")
done
if [[ "${bodies[0]}" == "${bodies[1]}" ]]; then
  ok "A3 unknown email and wrong password returned identical responses (code and body)"
else
  fail "A3 enumeration: the two login failures differ — ${bodies[0]} vs ${bodies[1]}"
fi

# --- A4: the rate limiter is real (backed by Redis) --------------------------
limited=0
for _ in $(seq 1 12); do
  code="$(curl -sS -o /dev/null -w '%{http_code}' -X POST \
    -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$EMAIL\",\"password\":\"wrong-again\"}" \
    "http://$API_ADDR/api/v1/auth/login")"
  [[ "$code" == "429" ]] && { limited=1; break; }
done
if (( limited )); then
  ok "A4 repeated failed logins were rate limited (429)"
else
  fail "A4 12 failed logins were never rate limited — rate limiting is not effective against a real Redis"
fi

printf '\n'
if (( FAILS )); then
  printf 'G3 auth-real-services: %d failure(s)\n' "$FAILS"
  exit 1
fi
printf 'G3 auth-real-services: all checks passed against real PostgreSQL and real Redis\n'
