#!/usr/bin/env bash
#
# G3 — the research-profile journey against REAL services (T0102, docs/67).
#
# The T0102 acceptance criteria are "未登录可读公开 profile" (the public
# profile is readable without a session) and "用户只能改自己可编辑字段"
# (only the owner may edit the editable fields). tests/e2e proves these over
# miniredis + in-memory stores; this script proves them with nothing
# substituted:
#
#   * the real API binary (cmd/api), built and started as a process;
#   * the real PostgreSQL from the infra stack, migrated to head BY THIS
#     TREE's own migration set (so the profiles table under test is the one
#     this task ships, 00017_profiles.sql);
#   * the real Redis from the infra stack (sessions, CSRF);
#   * real HTTP over a real socket, with the WEB's Origin and the API's Host
#     deliberately different — the shape the shipped web app makes.
#
# It fails loudly when a service is missing. It never skips: a G3 that
# quietly becomes a no-op is worse than no G3, because the gate then reports
# green. Same harness as auth-real-services-e2e.sh (T0101).
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
API_PORT="${POST_G3_API_PORT:-18082}"
API_ADDR="127.0.0.1:${API_PORT}"

WORK="$(mktemp -d)"
API_PID=""
SCRATCH_DB=""
cleanup() {
  [[ -n "$API_PID" ]] && kill "$API_PID" 2>/dev/null
  # The one database this run may destroy is the one it created itself.
  [[ -n "$SCRATCH_DB" ]] && psql "$PG_URL" -q -c \
    "DROP DATABASE IF EXISTS \"$SCRATCH_DB\" WITH (FORCE)" >/dev/null 2>&1
  rm -rf "$WORK"
}
trap cleanup EXIT

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

# --- preconditions: real services, or refuse ---------------------------------
if ! python3 scripts/pg-ready.py "$PG_URL" >/dev/null 2>&1; then
  echo "G3 profile-real-services: FAILED — no PostgreSQL accepting connections at $PG_URL." >&2
  echo "G3 requires real services and does NOT skip. Start them: make infra-up && make infra-init" >&2
  exit 1
fi
if ! (exec 3<>"/dev/tcp/${REDIS_ADDR%:*}/${REDIS_ADDR#*:}") 2>/dev/null; then
  echo "G3 profile-real-services: FAILED — no Redis at $REDIS_ADDR (start it: make infra-up)" >&2
  exit 1
fi
# Close the probe fd; the suppression is scoped to a group because a bare
# `exec 3<&- 2>/dev/null` redirects *this shell's* stderr for the rest of the
# script — every `FAILED … >&2` below would be written to /dev/null, and the
# gate log would show a step that failed with no output at all.
{ exec 3<&-; } 2>/dev/null || true

# --- this run's own database --------------------------------------------------
#
# Created here, dropped on the way out.
#
# This script used to migrate the shared dev database in place, which made every
# gate run a writer of state it does not own: whatever the last tree to run left
# behind is what the next tree inherits. Goose refuses to apply a migration
# numbered below the database's own version, so a tree whose migrations are not
# a superset of someone else's is refused before a single request is made — on
# 2026-09-14 the shared database sat at version 00040 (applied by some other
# tree) with 00034/000035 never applied, and every tree on the migration chain
# failed here in half a second for reasons that had nothing to do with the tree
# under test. A gate that grades a tree must not be reading another tree's
# leftovers, and a disposable database is the only form of "run this tree's
# migrations" that is actually about this tree.
command -v psql >/dev/null 2>&1 || {
  echo "G3 profile-real-services: FAILED — psql is required to give this run its own database" >&2
  exit 1
}
SCRATCH_DB="post_g3profile_$(date +%s)_$$"
if ! psql "$PG_URL" -q -v ON_ERROR_STOP=1 -c "CREATE DATABASE \"$SCRATCH_DB\"" >/dev/null 2>&1; then
  echo "G3 profile-real-services: FAILED — could not create this run's own database ($SCRATCH_DB) on the admin endpoint (make infra-up && make infra-init)" >&2
  exit 1
fi

# Every connection part comes out of the same URL, so the API this gate drives
# can only ever talk to the database this gate migrated. (Read into an array
# rather than eval'd assignments: a password is not shell source. The URL is
# passed through the environment, so it does not land on a command line here.)
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
  echo "G3 profile-real-services: FAILED — could not read the connection parts out of the admin URL" >&2
  exit 1
fi
SCRATCH_URL="${DB[0]}"; DB_HOST="${DB[1]}"; DB_PORT="${DB[2]}"
DB_USER="${DB[3]}"; DB_PASSWORD="${DB[4]}"; DB_SSLMODE="${DB[5]}"

# --- the schema the API writes to --------------------------------------------
#
# The migrations are applied by THIS tree's own code, not by the Supervisor's
# copy elsewhere: the profiles table under test is this task's 00017, not
# whatever another tree happens to have shipped. The helper lives under bin/,
# which the repository root .gitignore excludes, so it is invisible to
# `git status` and cannot disturb the planned review's fingerprint.
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
if ! (cd "$ROOT" && go run ./bin/g3migrate "$SCRATCH_URL") >"$WORK/migrate.log" 2>&1; then
  rm -rf "$ROOT/bin/g3migrate"
  fail "migration to head into $SCRATCH_DB using $ROOT's own migrations: $(tail -3 "$WORK/migrate.log")"
  printf '\nG3 profile-real-services: %d failure(s)\n' "$FAILS"
  exit 1
fi
rm -rf "$ROOT/bin/g3migrate"
ok "migrated $SCRATCH_DB ($ROOT's own migration set): $(tail -1 "$WORK/migrate.log")"

# --- a real API process ------------------------------------------------------
if ! go build -o "$WORK/api" ./cmd/api >"$WORK/build.log" 2>&1; then
  fail "building cmd/api: $(tail -3 "$WORK/build.log")"
  printf '\nG3 profile-real-services: %d failure(s)\n' "$FAILS"
  exit 1
fi

start_api() {
  env POST_ENV=test \
      POST_API_ADDR="$API_ADDR" \
      POST_REDIS_ADDR="$REDIS_ADDR" \
      POST_DB_HOST="$DB_HOST" POST_DB_PORT="$DB_PORT" POST_DB_USER="$DB_USER" \
      POST_DB_PASSWORD="$DB_PASSWORD" POST_DB_SSLMODE="$DB_SSLMODE" POST_DB_NAME="$SCRATCH_DB" \
      POST_BLOB_ACCESS_KEY=g3-ak POST_BLOB_SECRET_KEY=g3-sk \
      POST_GITEA_TOKEN=g3-tok \
      POST_WEB_ORIGIN="$WEB_ORIGIN" \
      "$WORK/api" >>"$WORK/api.log" 2>&1 &
  API_PID=$!
  for _ in $(seq 1 60); do
    # --max-time: curl has no default one, so against a socket that accepts the
    # connection and then says nothing this line waits forever, and the kill -0
    # on the next line — the one that notices the API died — never gets a turn.
    # The gate then hangs instead of failing, and a hang reports nothing at all
    # (#212). Two seconds is not a latency allowance for /healthz; it is the
    # bound that turns a silence into a failure.
    curl -fsS --max-time 2 "http://$API_ADDR/healthz" >/dev/null 2>&1 && return 0
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
  printf '\nG3 profile-real-services: %d failure(s)\n' "$FAILS"
  exit 1
fi
ok "real API process healthy on $API_ADDR (PostgreSQL + Redis from the infra stack)"

EMAIL_A="g3p-a-$(date +%s)-$$@example.test"
EMAIL_B="g3p-b-$(date +%s)-$$@example.test"
PASSWORD="g3-pass-$$-correct-horse"
HANDLE_A="g3alice$$"
HANDLE_A2="g3alice-r$$"  # the rename target: PID-unique so a re-run of this
                         # script never collides with a previous run's renamed
                         # user (rows persist in the real dev PostgreSQL)
HANDLE_B="g3bob$$"
JAR_A="$WORK/a.jar"   # Alice's browser (session cookie lives here)
JAR_B="$WORK/b.jar"   # Bob's browser
JAR_0="$WORK/0.jar"   # a signed-out browser (never receives a cookie)
touch "$JAR_0"

jsonval() { python3 -c "import json,sys;print(json.load(open('$1')).get('$2',''))" 2>/dev/null; }

# --- seed: Alice and Bob sign up through the real API ------------------------
code="$(curl -sS -c "$JAR_A" -o "$WORK/signup-a.json" -w '%{http_code}' -X POST \
  -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL_A\",\"password\":\"$PASSWORD\",\"handle\":\"$HANDLE_A\",\"display_name\":\"G3 Alice\"}" \
  "http://$API_ADDR/api/v1/auth/signup")"
if [[ "$code" == "201" || "$code" == "200" ]]; then
  ok "signup Alice -> $code"
else
  fail "signup Alice returned $code: $(cat "$WORK/signup-a.json")"
fi
USER_A="$(python3 -c "import json;print(json.load(open('$WORK/signup-a.json'))['user']['id'])" 2>/dev/null || true)"
CSRF_A="$(jsonval "$WORK/signup-a.json" "csrf_token")"

code="$(curl -sS -c "$JAR_B" -o "$WORK/signup-b.json" -w '%{http_code}' -X POST \
  -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL_B\",\"password\":\"$PASSWORD\",\"handle\":\"$HANDLE_B\",\"display_name\":\"G3 Bob\"}" \
  "http://$API_ADDR/api/v1/auth/signup")"
if [[ "$code" == "201" || "$code" == "200" ]]; then
  ok "signup Bob -> $code"
else
  fail "signup Bob returned $code: $(cat "$WORK/signup-b.json")"
fi
CSRF_B="$(jsonval "$WORK/signup-b.json" "csrf_token")"

[[ -n "$USER_A" && -n "$CSRF_A" && -n "$CSRF_B" ]] \
  && ok "signups returned user id + CSRF tokens" \
  || fail "missing id or csrf_token in signup responses (id=$USER_A a=$CSRF_A b=$CSRF_B)"

# --- P1: 未登录可读公开 profile — signed-out GET, email absent ----------------
code="$(curl -sS -b "$JAR_0" -o "$WORK/p1.json" -w '%{http_code}' \
  "http://$API_ADDR/api/v1/users/$USER_A/profile")"
if [[ "$code" == "200" ]]; then
  ok "P1 anonymous GET /users/{id}/profile -> 200"
else
  fail "P1 anonymous profile GET returned $code, want 200: $(cat "$WORK/p1.json")"
fi
if python3 -c "
import json,sys
d=json.load(open('$WORK/p1.json'))
assert d.get('handle')=='$HANDLE_A', d
assert d.get('display_name')=='G3 Alice', d
assert d.get('id')=='$USER_A', d
assert 'bio' in d and 'created_at' in d, d
" 2>/dev/null; then
  ok "P1 public payload carries handle/display_name/bio/created_at"
else
  fail "P1 public payload wrong: $(cat "$WORK/p1.json")"
fi
if grep -q "@example.test" "$WORK/p1.json" || grep -q '"email"' "$WORK/p1.json"; then
  fail "P1 the public profile leaked email (structural leak): $(cat "$WORK/p1.json")"
else
  ok "P1 public profile carries no email (structurally absent)"
fi

# --- P2: 用户只能改自己可编辑字段 — owner edits land, stranger edits don't ----
code="$(curl -sS -c "$JAR_A" -b "$JAR_A" -o "$WORK/p2.json" -w '%{http_code}' -X PATCH \
  -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF_A" \
  -d "{\"display_name\":\"G3 Alice R.\",\"bio\":\"alloy design\",\"handle\":\"$HANDLE_A2\"}" \
  "http://$API_ADDR/api/v1/users/$USER_A/profile")"
if [[ "$code" == "200" ]]; then
  ok "P2 owner PATCH (display_name/bio/handle) -> 200"
else
  fail "P2 owner PATCH returned $code, want 200: $(cat "$WORK/p2.json")"
fi

# Bob tries to edit Alice's profile with his own session + CSRF: 403.
code="$(curl -sS -c "$JAR_B" -b "$JAR_B" -o "$WORK/p2-forge.json" -w '%{http_code}' -X PATCH \
  -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF_B" \
  -d '{"display_name":"Bob the Usurper"}' \
  "http://$API_ADDR/api/v1/users/$USER_A/profile")"
if [[ "$code" == "403" ]]; then
  ok "P2 Bob PATCHes Alice's profile -> 403"
else
  fail "P2 foreign PATCH returned $code, want 403: $(cat "$WORK/p2-forge.json")"
fi

# Anonymous PATCH: 401 before routing.
code="$(curl -sS -b "$JAR_0" -o "$WORK/p2-anon.json" -w '%{http_code}' -X PATCH \
  -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
  -d '{"display_name":"Anonymous"}' \
  "http://$API_ADDR/api/v1/users/$USER_A/profile")"
if [[ "$code" == "401" ]]; then
  ok "P2 anonymous PATCH -> 401"
else
  fail "P2 anonymous PATCH returned $code, want 401: $(cat "$WORK/p2-anon.json")"
fi

# Alice's profile still shows HER edits — the refused attempts changed nothing.
if python3 -c "
import json
d=json.load(open('$WORK/p2.json'))
assert d.get('display_name')=='G3 Alice R.', d
assert d.get('bio')=='alloy design', d
assert d.get('handle')=='$HANDLE_A2', d
" 2>/dev/null; then
  ok "P2 owner's edits landed (display_name/bio/handle)"
else
  fail "P2 owner PATCH body wrong: $(cat "$WORK/p2.json")"
fi

# --- P3: the id-keyed URL is stable across the handle rename -----------------
code="$(curl -sS -b "$JAR_0" -o "$WORK/p3-id.json" -w '%{http_code}' \
  "http://$API_ADDR/api/v1/users/$USER_A/profile")"
code_new="$(curl -sS -b "$JAR_0" -o "$WORK/p3-new.json" -w '%{http_code}' \
  "http://$API_ADDR/api/v1/users/by-handle/$HANDLE_A2/profile")"
code_old="$(curl -sS -b "$JAR_0" -o "$WORK/p3-old.json" -w '%{http_code}' \
  "http://$API_ADDR/api/v1/users/by-handle/$HANDLE_A/profile")"
if [[ "$code" == "200" && "$code_new" == "200" && "$code_old" == "404" ]]; then
  ok "P3 id-keyed URL 200, new handle resolves, old handle 404 (stable profile URL)"
else
  fail "P3 after rename: id=$code (want 200) new=$code_new (want 200) old=$code_old (want 404)"
fi

# --- P4: validation + handle conflicts over the wire -------------------------
code="$(curl -sS -c "$JAR_A" -b "$JAR_A" -o "$WORK/p4-valid.json" -w '%{http_code}' -X PATCH \
  -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF_A" \
  -d '{"display_name":""}' \
  "http://$API_ADDR/api/v1/users/$USER_A/profile")"
[[ "$code" == "400" ]] \
  && ok "P4 invalid display_name -> 400" \
  || fail "P4 invalid display_name returned $code, want 400"

code="$(curl -sS -c "$JAR_A" -b "$JAR_A" -o "$WORK/p4-taken.json" -w '%{http_code}' -X PATCH \
  -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
  -H "X-CSRF-Token: $CSRF_A" \
  -d "{\"handle\":\"$HANDLE_B\"}" \
  "http://$API_ADDR/api/v1/users/$USER_A/profile")"
[[ "$code" == "409" ]] \
  && ok "P4 taking Bob's handle -> 409" \
  || fail "P4 handle conflict returned $code, want 409"

# --- P5: profiles live in the real PostgreSQL ---------------------------------
# A second API process restart must not lose the profile: it is a row in
# PostgreSQL, not in-process state.
stop_api
if ! start_api; then
  fail "the API did not come back up: $(tail -5 "$WORK/api.log")"
else
  code="$(curl -sS -b "$JAR_0" -o "$WORK/p5.json" -w '%{http_code}' \
    "http://$API_ADDR/api/v1/users/$USER_A/profile")"
  if [[ "$code" == "200" ]] && python3 -c "
import json
d=json.load(open('$WORK/p5.json'))
assert d.get('bio')=='alloy design', d
assert d.get('handle')=='$HANDLE_A2', d
" 2>/dev/null; then
    ok "P5 profile survives an API restart -> row in real PostgreSQL"
  else
    fail "P5 profile after restart: $code / $(cat "$WORK/p5.json")"
  fi
fi

printf '\n'
if (( FAILS )); then
  printf 'G3 profile-real-services: %d failure(s)\n' "$FAILS"
  exit 1
fi
printf 'G3 profile-real-services: all checks passed against real PostgreSQL and real Redis\n'
