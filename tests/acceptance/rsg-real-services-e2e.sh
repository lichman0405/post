#!/usr/bin/env bash
#
# G3 — the RSG core path against real services (docs/67, P2/T0202+).
#
# The RSG is the scientific state model: objects have immutable versions,
# versions relate to each other, and a branch advances through state commits.
# Every one of those words is a durability and ordering claim, which is exactly
# what an all-mock test cannot check — a fake store will happily let a version
# mutate, or a relation point at a version from another branch.
#
# So this drives the real HTTP surface against real PostgreSQL (and Redis for
# the session), along the paths specs/api/openapi.yaml already fixes — the
# contract is openapi-first, so the paths are not a guess and this doubles as a
# check that the implementation honours them.
#
# It is assigned to the P2 tasks from T0204 onward, where the whole chain
# (object, version, relation, state transition) exists. Until then it fails
# loudly, and that is the point: a G3 that passes vacuously certifies nothing.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

PG_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"
REDIS_ADDR="${POST_G3_REDIS_ADDR:-127.0.0.1:6379}"
API_PORT="${POST_G3_RSG_API_PORT:-18082}"
API_ADDR="127.0.0.1:${API_PORT}"
WEB_ORIGIN="${POST_G3_WEB_ORIGIN:-http://127.0.0.1:3000}"

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
MISSING=()
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

# A Go ServeMux answers 307 for an unregistered path under a registered
# subtree, so "not built yet" arrives looking like a redirect. Say what it
# actually is: the list below is the P2 work list, and it is far more useful
# than six unexplained statuses.
served() { # served STATUS METHOD PATH
  case "$1" in
    200|201|202|204) return 0 ;;
    307|404|405|501) MISSING+=("$2 $3 (unserved: $1)"); return 1 ;;
    *) return 1 ;;
  esac
}

if ! python3 scripts/pg-ready.py "$PG_URL" >/dev/null 2>&1; then
  echo "G3 rsg-real-services: FAILED — no PostgreSQL accepting connections at $PG_URL (make infra-up)" >&2
  exit 1
fi
if ! (exec 3<>"/dev/tcp/${REDIS_ADDR%:*}/${REDIS_ADDR#*:}") 2>/dev/null; then
  echo "G3 rsg-real-services: FAILED — no Redis at $REDIS_ADDR (make infra-up)" >&2
  exit 1
fi
# Close the probe fd; the suppression is scoped to a group because a bare
# `exec 3<&- 2>/dev/null` redirects *this shell's* stderr for the rest of the
# script — every `FAILED … >&2` below would be written to /dev/null, and the
# gate log would show a step that failed with no output at all.
{ exec 3<&-; } 2>/dev/null || true

# --- the schema the API writes to --------------------------------------------
#
# This run gets a database of its own, created here and dropped on the way out.
#
# It used to migrate the shared dev database in place, which made every gate run
# a writer of state it does not own: whatever the last tree to run left behind
# is what the next tree inherits. Goose refuses to apply a migration numbered
# below the database's own version, so a tree whose migrations are not a
# superset of someone else's is refused before a single request is made — on
# 2026-09-14 the shared database sat at version 00040 (applied by some other
# tree) with 00034/000035 never applied, and every tree on the migration chain
# failed here in half a second for reasons that had nothing to do with the tree
# under test. A gate that grades a tree must not be reading another tree's
# leftovers, and a disposable database is the only form of "run this tree's
# migrations" that is actually about this tree.
command -v psql >/dev/null 2>&1 || {
  echo "G3 rsg-real-services: FAILED — psql is required to give this run its own database" >&2
  exit 1
}
SCRATCH_DB="post_g3rsg_$(date +%s)_$$"
if ! psql "$PG_URL" -q -v ON_ERROR_STOP=1 -c "CREATE DATABASE \"$SCRATCH_DB\"" >/dev/null 2>&1; then
  echo "G3 rsg-real-services: FAILED — could not create this run's own database ($SCRATCH_DB) on the admin endpoint (make infra-up && make infra-init)" >&2
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
  echo "G3 rsg-real-services: FAILED — could not read the connection parts out of the admin URL" >&2
  exit 1
fi
SCRATCH_URL="${DB[0]}"; DB_HOST="${DB[1]}"; DB_PORT="${DB[2]}"
DB_USER="${DB[3]}"; DB_PASSWORD="${DB[4]}"; DB_SSLMODE="${DB[5]}"

# The migrations are applied by THIS tree's own code, not by the Supervisor's
# copy elsewhere: a task's diff may add a migration, and applying someone else's
# migrations would leave the schema under test missing exactly the thing the
# task delivers. The helper lives under bin/, which the repository root
# .gitignore excludes, so it is invisible to `git status` and cannot disturb the
# planned review's fingerprint. `go run` from this directory resolves the import
# against this tree's module.
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
	if _, err := persistence.Migrate(context.Background(), os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
GOMIGRATE
(cd "$ROOT" && go run ./bin/g3migrate "$SCRATCH_URL") >"$WORK/migrate.log" 2>&1 || {
  rm -rf "$ROOT/bin/g3migrate"
  # The tail is the whole value of this branch: `go run` folds the program's
  # own error into "exit status 1", and goose's reason (an unapplied migration
  # below the database's version, say) is only in the log it wrote.
  echo "G3 rsg-real-services: FAILED — could not migrate the database with this tree's migrations: $(tail -3 "$WORK/migrate.log" | tr '\n' ' ')" >&2
  exit 1
}
rm -rf "$ROOT/bin/g3migrate"

go build -o "$WORK/api" ./cmd/api >"$WORK/build.log" 2>&1 || {
  fail "building cmd/api: $(tail -3 "$WORK/build.log")"
  printf '\nG3 rsg-real-services: %d failure(s)\n' "$FAILS"; exit 1
}

start_api() {
  env POST_ENV=test POST_API_ADDR="$API_ADDR" POST_REDIS_ADDR="$REDIS_ADDR" \
      POST_DB_HOST="$DB_HOST" POST_DB_PORT="$DB_PORT" POST_DB_USER="$DB_USER" \
      POST_DB_PASSWORD="$DB_PASSWORD" POST_DB_SSLMODE="$DB_SSLMODE" POST_DB_NAME="$SCRATCH_DB" \
      POST_BLOB_ACCESS_KEY=g3-ak POST_BLOB_SECRET_KEY=g3-sk POST_GITEA_TOKEN=g3-tok \
      POST_WEB_ORIGIN="$WEB_ORIGIN" "$WORK/api" >>"$WORK/api.log" 2>&1 &
  API_PID=$!
  for _ in $(seq 1 60); do
    curl -fsS "http://$API_ADDR/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$API_PID" 2>/dev/null || return 1
    sleep 0.5
  done
  return 1
}
stop_api() { [[ -n "$API_PID" ]] && kill "$API_PID" 2>/dev/null; wait "$API_PID" 2>/dev/null; API_PID=""; }

start_api || { fail "the API did not become healthy: $(tail -5 "$WORK/api.log")"; printf '\nG3 rsg-real-services: %d failure(s)\n' "$FAILS"; exit 1; }
ok "real API against real PostgreSQL and Redis"

JAR="$WORK/cookies.txt"
EMAIL="g3rsg-$(date +%s)-$$@example.test"
curl -sS -c "$JAR" -b "$JAR" -o "$WORK/signup.json" -X POST \
  -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
  -d "{\"email\":\"$EMAIL\",\"password\":\"g3-rsg-password\",\"handle\":\"g3rsg$$\",\"display_name\":\"G3 RSG\"}" \
  "http://$API_ADDR/api/v1/auth/signup" >/dev/null
CSRF="$(python3 -c "import json;print(json.load(open('$WORK/signup.json')).get('csrf_token',''))" 2>/dev/null)"
[[ -n "$CSRF" ]] && ok "authenticated (session + CSRF)" || fail "could not sign up: $(cat "$WORK/signup.json")"

# req METHOD PATH [BODY] -> body on stdout, status in $STATUS
req() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -c "$JAR" -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}'
              -X "$method" -H "Origin: $WEB_ORIGIN" -H "X-CSRF-Token: $CSRF" -H "Idempotency-Key: g3-$RANDOM$RANDOM")
  [[ -n "$body" ]] && args+=(-H 'Content-Type: application/json' -d "$body")
  STATUS="$(curl "${args[@]}" "http://$API_ADDR/api/v1$path")"
}
jq_get() { python3 -c "import json,sys;d=json.load(open('$WORK/resp.json'));print(d$1)" 2>/dev/null; }

# --- project -----------------------------------------------------------------
req POST /projects "{\"name\":\"G3 RSG\",\"slug\":\"g3-rsg-$$\",\"visibility\":\"private\",\"purpose\":\"integration\"}"
[[ "$STATUS" == "201" || "$STATUS" == "200" ]] || fail "create project -> $STATUS: $(head -c 200 "$WORK/resp.json")"
# The project surface answers {project: {...}, membership: {...}}.
PROJECT="$(jq_get "['project']['id']")"
[[ -n "$PROJECT" ]] && ok "created a project" || fail "no project id in the response"

# --- branch ------------------------------------------------------------------
req POST "/projects/$PROJECT/branches" '{"name":"main","base_ref":"","visibility":"private"}'
if [[ "$STATUS" == "201" || "$STATUS" == "200" ]]; then ok "created a research branch"; else fail "create branch -> $STATUS: $(head -c 200 "$WORK/resp.json")"; fi
BRANCH="$(jq_get "['id']")"

# --- object + immutable version ---------------------------------------------
req POST "/projects/$PROJECT/branches/$BRANCH/objects" '{"object_type":"material","payload":{"name":"MOF-5"}}'
if served "$STATUS" POST "/projects/{id}/branches/{id}/objects"; then ok "created a scientific object"; else fail "create object -> $STATUS: $(head -c 200 "$WORK/resp.json")"; fi
OBJECT="$(jq_get "['id']")"
V1="$(jq_get "['version_id']")"
V1_STATE="$(jq_get "['state_id']")"

req POST "/projects/$PROJECT/branches/$BRANCH/objects/$OBJECT:version" '{"expected_version":1,"patch":{"name":"MOF-5","formula":"Zn4O(BDC)3"}}'
if served "$STATUS" POST "/projects/{id}/branches/{id}/objects/{id}:version"; then ok "created a second version (a state transition, not a mutation)"; else fail "create version -> $STATUS: $(head -c 200 "$WORK/resp.json")"; fi
V2="$(jq_get "['version_id']")"
V2_STATE="$(jq_get "['state_id']")"
[[ -n "$V1" && -n "$V2" && "$V1" != "$V2" ]] \
  && ok "the two versions have distinct identities" \
  || fail "the second version did not produce a new identity (v1=$V1 v2=$V2) — versions are being mutated in place"

# --- relation ----------------------------------------------------------------
req POST "/projects/$PROJECT/branches/$BRANCH/relations" "{\"relation_type\":\"derived_from\",\"source_object_version_id\":\"$V2\",\"target_object_version_id\":\"$V1\"}"
if served "$STATUS" POST "/projects/{id}/branches/{id}/relations"; then ok "created a typed relation between two versions"; else fail "create relation -> $STATUS: $(head -c 200 "$WORK/resp.json")"; fi

# --- validation gate ---------------------------------------------------------
req POST "/projects/$PROJECT/branches/$BRANCH:validate" '{"gate":"pr"}'
case "$STATUS" in
  200|201|202) ok "the branch validates (:validate) -> $STATUS" ;;
  *) fail ":validate -> $STATUS: $(head -c 200 "$WORK/resp.json")" ;;
esac

# --- durability of history ---------------------------------------------------
stop_api
start_api || fail "the API did not come back up after a restart"
req GET "/projects/$PROJECT/branches/$BRANCH/objects/$OBJECT"
if [[ "$STATUS" == "200" ]]; then
  CUR="$(jq_get "['version_id']")"
  [[ "$CUR" == "$V2" ]] \
    && ok "the object's current version survived an API restart" \
    || fail "the current version changed across a restart ($CUR != $V2)"
else
  fail "re-reading the object after a restart -> $STATUS"
fi

# --- query: the RSG graph slice (T0209) ---------------------------------------
# The project-wide slice renders the material at its as-of v2 AND at the
# v1 the derived_from edge pins, with the edge's version-pinned endpoints.
req GET "/projects/$PROJECT/query"
if [[ "$STATUS" != "200" ]]; then
  fail "query the project-wide slice -> $STATUS: $(head -c 200 "$WORK/resp.json")"
else
  python3 - "$WORK/resp.json" "$OBJECT" "$V1" "$V2" <<'PY' && ok "query returns the project-wide graph slice" \
    || fail "query slice content wrong: $(head -c 300 "$WORK/resp.json")"
import json, sys
d = json.load(open(sys.argv[1]))
obj_id, v1, v2 = sys.argv[2], sys.argv[3], sys.argv[4]
objs = {(o["id"], o["version_id"]): o["version_no"] for o in d["objects"]}
rels = [r for r in d["relations"] if r["relation_type"] == "derived_from"]
assert d["project_id"], "project_id missing"
assert (obj_id, v2) in objs, f"as-of v2 missing: {sorted(objs)}"
assert objs.get((obj_id, v1)) == 1, f"pinned endpoint v1 missing: {sorted(objs)}"
assert any(r["source_object_version_id"] == v2 and r["target_object_version_id"] == v1 for r in rels), \
    "derived_from v2->v1 missing"
PY
fi

# The object-type filter selects the material and its induced edge (both
# endpoints are the same material).
req GET "/projects/$PROJECT/query?object_type=material&depth=1"
if [[ "$STATUS" == "200" ]]; then
  python3 - "$WORK/resp.json" "$OBJECT" "$V1" "$V2" <<'PY' && ok "query filters by object type with the induced edge" \
    || fail "object-type query content wrong: $(head -c 300 "$WORK/resp.json")"
import json, sys
d = json.load(open(sys.argv[1]))
obj_id, v1, v2 = sys.argv[2], sys.argv[3], sys.argv[4]
objs = {(o["id"], o["version_id"]) for o in d["objects"]}
assert {(obj_id, v1), (obj_id, v2)} <= objs, f"material versions missing: {sorted(objs)}"
assert all(o["object_type"] == "material" for o in d["objects"]), "non-material node leaked in"
assert any(r["relation_type"] == "derived_from" for r in d["relations"]), "induced edge missing"
PY
else
  fail "object-type query -> $STATUS: $(head -c 200 "$WORK/resp.json")"
fi

# State pins return state-specific slices: at v1's state the material is
# still at v1 and the (later) edge is absent; at v2's state the material is
# at v2 and the edge is still absent.
req GET "/projects/$PROJECT/query?state_id=$V1_STATE"
if [[ "$STATUS" == "200" ]]; then
  python3 - "$WORK/resp.json" "$OBJECT" "$V1" "$V2" <<'PY' && ok "state pin renders the v1 slice" \
    || fail "v1 state slice wrong: $(head -c 300 "$WORK/resp.json")"
import json, sys
d = json.load(open(sys.argv[1]))
obj_id, v1, v2 = sys.argv[2], sys.argv[3], sys.argv[4]
objs = {(o["id"], o["version_id"]) for o in d["objects"]}
assert objs == {(obj_id, v1)}, f"want only v1: {sorted(objs)}"
assert d["relations"] == [], "the later edge must not exist at v1"
PY
else
  fail "v1 state query -> $STATUS: $(head -c 200 "$WORK/resp.json")"
fi
req GET "/projects/$PROJECT/query?state_id=$V2_STATE"
if [[ "$STATUS" == "200" ]]; then
  python3 - "$WORK/resp.json" "$OBJECT" "$V2" <<'PY' && ok "state pin renders the v2 slice" \
    || fail "v2 state slice wrong: $(head -c 300 "$WORK/resp.json")"
import json, sys
d = json.load(open(sys.argv[1]))
obj_id, v2 = sys.argv[2], sys.argv[3]
objs = {(o["id"], o["version_id"]) for o in d["objects"]}
assert objs == {(obj_id, v2)}, f"want only v2: {sorted(objs)}"
assert d["relations"] == [], "the later edge must not exist at v2"
PY
else
  fail "v2 state query -> $STATUS: $(head -c 200 "$WORK/resp.json")"
fi

# A non-member of the private project gets not-found, never forbidden (the
# relation list of the private project never leaks) — the same for an
# anonymous caller.
BOB_JAR="$WORK/bob-cookies.txt"
curl -sS -c "$BOB_JAR" -b "$BOB_JAR" -o "$WORK/bob-signup.json" -X POST \
  -H "Origin: $WEB_ORIGIN" -H 'Content-Type: application/json' \
  -d "{\"email\":\"bob-$EMAIL\",\"password\":\"g3-rsg-password\",\"handle\":\"g3rsgbob$$\",\"display_name\":\"G3 Bob\"}" \
  "http://$API_ADDR/api/v1/auth/signup" >/dev/null
STATUS_BOB="$(curl -sS -c "$BOB_JAR" -b "$BOB_JAR" -o "$WORK/resp.json" -w '%{http_code}' "http://$API_ADDR/api/v1/projects/$PROJECT/query")"
STATUS_ANON="$(curl -sS -o "$WORK/resp-anon.json" -w '%{http_code}' "http://$API_ADDR/api/v1/projects/$PROJECT/query")"
if [[ "$STATUS_BOB" == "404" ]] \
  && python3 -c "import json;assert json.load(open('$WORK/resp.json'))['code']=='PROJECT_NOT_FOUND'" 2>/dev/null; then
  ok "a non-member query answers 404 PROJECT_NOT_FOUND (existence hidden, never forbidden)"
else
  fail "non-member query -> $STATUS_BOB: $(head -c 200 "$WORK/resp.json")"
fi
if [[ "$STATUS_ANON" == "404" ]]; then
  ok "an anonymous query of the private project answers 404"
else
  fail "anonymous query -> $STATUS_ANON: $(head -c 200 "$WORK/resp-anon.json")"
fi

# Shape refusals stay 400 at the transport; the depth cap is the service's.
STATUS_BADDEPTH="$(curl -sS -c "$JAR" -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' "http://$API_ADDR/api/v1/projects/$PROJECT/query?depth=abc")"
[[ "$STATUS_BADDEPTH" == "400" ]] \
  && ok "a non-numeric depth is refused with 400" \
  || fail "bad depth -> $STATUS_BADDEPTH: $(head -c 200 "$WORK/resp.json")"
STATUS_DEEP="$(curl -sS -c "$JAR" -b "$JAR" -o "$WORK/resp.json" -w '%{http_code}' "http://$API_ADDR/api/v1/projects/$PROJECT/query?depth=6")"
[[ "$STATUS_DEEP" == "400" ]] \
  && ok "a depth beyond the cap is refused with 400" \
  || fail "over-cap depth -> $STATUS_DEEP: $(head -c 200 "$WORK/resp.json")"

printf '\n'
if (( FAILS )); then
  if (( ${#MISSING[@]} )); then
    printf 'The following paths from specs/api/openapi.yaml are not served yet —\n'
    printf 'P2 builds them, and this gate is assigned from the task that completes the chain:\n'
    printf '  - %s\n' "${MISSING[@]}"
  fi
  printf 'G3 rsg-real-services: %d failure(s)\n' "$FAILS"
  exit 1
fi
printf 'G3 rsg-real-services: all checks passed against real PostgreSQL and Redis\n'
