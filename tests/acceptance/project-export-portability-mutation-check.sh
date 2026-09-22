#!/usr/bin/env bash
#
# Is project-export-portability-e2e.sh's refusal load-bearing, or does it
# refuse whatever happens?
#
# The e2e proves three tamper classes are refused. That is worth nothing on
# its own: an instrument that answers NO to everything refuses tampering too,
# which is why the acceptance criterion asks for this second half — turn the
# refusal OFF and watch what the tamper does.
#
# The mutation is in the SUBJECT, never in an assertion: a patched copy of the
# driver's own Check (tests/acceptance/portability/bundle.go) is built with
# `go build -overlay`, so the mutant lives in a temp directory and the tree it
# grades is never written to. The patch is anchored on Check's signature, and
# a missing anchor is a loud failure (exit 2) rather than a mutation that
# quietly proves nothing — the same shape tests/acceptance/
# mof-canonical-mutation-check.sh uses.
#
# For each kind the script reports TWO things:
#
#   - did the mutant accept the tampered bundle? (expected: yes — that is what
#     makes the real refusal load-bearing; a mutant that still refuses means
#     the mutation did not land)
#   - once accepted, does anything ELSE catch it? The import re-derives every
#     identifier from the target's own rebuilt stores, so a tampered blob
#     should still be caught there. That is defence in depth, and a class
#     where nothing catches it is a class where the refusal is the ONLY
#     guard — which is exactly what the criterion's "用例转红" is about.
#
# Usage: bash tests/acceptance/project-export-portability-mutation-check.sh \
#          --bundle <dir> --work <dir> --mut-db <name> [--target-org <org>]
# Exit:  0 when every mutation landed and was reported; 1 otherwise; 2 when the
#        mutation could not be built at all.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

BUNDLE=""
WORK=""
MUT_DB="post_seed_demo_t1208_mut"
TARGET_ORG=""
while [ $# -gt 0 ]; do
  case "$1" in
    --bundle) BUNDLE="$2"; shift 2;;
    --work) WORK="$2"; shift 2;;
    --mut-db) MUT_DB="$2"; shift 2;;
    --target-org) TARGET_ORG="$2"; shift 2;;
    *) echo "unknown argument: $1" >&2; exit 2;;
  esac
done
[ -n "$BUNDLE" ] || { echo "--bundle is required" >&2; exit 2; }
[ -d "$BUNDLE" ] || { echo "no bundle at $BUNDLE" >&2; exit 2; }
[ -n "$WORK" ] || WORK="$(mktemp -d /tmp/t1208-mut-XXXXXX)"
mkdir -p "$WORK"

PG_HOST="${POST_DB_HOST:-127.0.0.1}"
PG_PORT="${POST_DB_PORT:-5432}"
PG_USER="${POST_DB_USER:-postgres}"
PG_PW="${POST_DB_PASSWORD:-postgres_dev_pw}"
ADMIN_URL="postgres://$PG_USER:$PG_PW@$PG_HOST:$PG_PORT/postgres?sslmode=disable"
db_url() { printf 'postgres://%s:%s@%s:%s/%s?sslmode=disable' "$PG_USER" "$PG_PW" "$PG_HOST" "$PG_PORT" "$1"; }

VERDICT=0
note() { printf '%s\n' "$*"; }
bad()  { printf 'MUTATION CHECK FAILED — %s\n' "$*" >&2; VERDICT=1; }

# The mutant repositories this run creates, removed on the way out.
MUTATED_REPOS=""
cleanup() {
  local rc=$?
  if [ -n "${POST_GITEA_TOKEN:-}" ]; then
    for r in $MUTATED_REPOS; do
      curl -fsS -X DELETE -H "Authorization: token $POST_GITEA_TOKEN" "${POST_GITEA_BASE_URL:-http://127.0.0.1:3000}/api/v1/repos/$r" >/dev/null 2>&1 || true
    done
  fi
  psql "$ADMIN_URL" -tAc "DROP DATABASE IF EXISTS $MUT_DB WITH (FORCE)" >/dev/null 2>&1 || true
  exit $rc
}
trap cleanup EXIT

# --- 1. build the mutant ---------------------------------------------------

MUT_SRC="$WORK/mutant-src"
mkdir -p "$MUT_SRC"
OVERLAY="$WORK/overlay.json"
python3 - "$ROOT/tests/acceptance/portability/bundle.go" "$MUT_SRC/bundle.go" "$OVERLAY" <<'PY' || exit 2
import json, sys
src, dst, overlay_path = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(src).read()
anchor = "func Check(bundleDir string) (*Index, error) {\n"
if anchor not in s:
    print("the mutation anchor is gone: Check's signature in %s is not the shape this"
          " mutation knows how to break" % src, file=sys.stderr)
    sys.exit(2)
# The kill switch is IN THE MUTANT, never in the tree: this file only ever
# exists under $WORK, and go build -overlay is what puts it in front of the
# real one for the length of one build.
s = s.replace(anchor, anchor + '\tif os.Getenv("T1208_MUTANT") == "1" {\n\t\treturn readIndex(bundleDir)\n\t}\n', 1)
open(dst, "w").write(s)
json.dump({"Replace": {src: dst}}, open(overlay_path, "w"))
PY

note "MUTATION CHECK — the mutation: Check() returns the index without verifying anything"
note "                (real:  $ROOT/tests/acceptance/portability/bundle.go)"
note "                (mutant: $MUT_SRC/bundle.go, built with -overlay; the tree is untouched)"
note ""

go build -o "$WORK/rddev" ./cmd/rddev >/dev/null 2>&1 || { echo "go build ./cmd/rddev failed" >&2; exit 2; }
T1208_MUTANT=1 go build -overlay "$OVERLAY" -o "$WORK/portability-mutant" ./tests/acceptance/portability \
  || { echo "could not build the mutant with -overlay" >&2; exit 2; }
go build -o "$WORK/portability-real" ./tests/acceptance/portability \
  || { echo "could not build the real driver" >&2; exit 2; }
note "built the mutant and the real driver"

# --- 2. one case per tamper class ------------------------------------------

run_case() {
  local kind="$1"
  local tampered="$WORK/tampered-$kind"
  local repo="t1208-mut-$kind"

  note ""
  note "--- $kind -------------------------------------------------------------"
  local trc=0
  "$WORK/portability-real" tamper --bundle "$BUNDLE" --out "$tampered" --kind "$kind" >"$WORK/tamper-$kind.log" 2>&1 || trc=$?
  if [ "$trc" != 0 ]; then
    bad "$kind: could not build the tampered bundle: $(cat "$WORK/tamper-$kind.log")"
    return
  fi
  note "  $(grep '^TAMPER' "$WORK/tamper-$kind.log")"

  # (a) the real driver must refuse. This is the same assertion the e2e
  # makes, repeated here so the pair sits side by side in one output.
  local rc=0
  "$WORK/portability-real" check --bundle "$tampered" >"$WORK/real-$kind.log" 2>&1 || rc=$?
  if [ "$rc" = "3" ]; then
    note "  with the refusal IN:   exit 3 — $(grep '^REFUSED' "$WORK/real-$kind.log")"
  else
    bad "$kind: the REAL driver exited $rc on the tampered bundle; it must refuse (exit 3)"
  fi

  # (b) the mutant must accept it. If it still refuses, the mutation did
  # not land and this whole check proves nothing.
  rc=0
  T1208_MUTANT=1 "$WORK/portability-mutant" check --bundle "$tampered" >"$WORK/mutant-$kind.log" 2>&1 || rc=$?
  if [ "$rc" = "0" ]; then
    note "  with the refusal OFF:  exit 0 — $(grep '^CHECK OK' "$WORK/mutant-$kind.log")"
    note "                         the tampered bundle was ACCEPTED, so the refusal is what caught it"
  else
    bad "$kind: the mutant exited $rc instead of accepting the tampered bundle — the mutation did not land, so this case proves nothing"
    return
  fi

  # (c) what happens once it is accepted: is there a second guard?
  psql "$ADMIN_URL" -tAc "DROP DATABASE IF EXISTS $MUT_DB WITH (FORCE)" >/dev/null 2>&1
  psql "$ADMIN_URL" -tAc "CREATE DATABASE $MUT_DB" >/dev/null 2>&1
  "$WORK/rddev" db migrate --url "$(db_url "$MUT_DB")" >/dev/null 2>&1
  rc=0
  T1208_MUTANT=1 POST_TGT_DATABASE_URL="$(db_url "$MUT_DB")" \
    "$WORK/portability-mutant" import --bundle "$tampered" --repo-name "$repo" ${TARGET_ORG:+--target-org "$TARGET_ORG"} \
    >"$WORK/mutant-import-$kind.log" 2>&1 || rc=$?
  if [ "$rc" = "0" ]; then
    note "  the mutant import SUCCEEDED (exit 0) with the tampered bundle in place."
    note "  Nothing downstream reads this file: the importer rebuilds the manifest"
    note "  from the rows it loads, so a tampered COPY of manifest.json changes"
    note "  nothing in the target and the re-derived hash still matches. For the"
    note "  $kind class the bundle check is the ONLY guard — with it disabled the"
    note "  tampered document travels unnoticed."
  else
    note "  the mutant import was caught by a SECOND guard (exit $rc): $(tail -1 "$WORK/mutant-import-$kind.log")"
    note "  (defence in depth: the import re-derives every identifier from the"
    note "   target's own rebuilt stores, so this class is caught twice.)"
  fi
  MUTATED_REPOS="$MUTATED_REPOS ${TARGET_ORG:-${POST_GITEA_OWNER:-postadmin}}/$repo"
}

run_case blob
run_case manifest
run_case commit

note ""
if [ "$VERDICT" = "0" ]; then
  note "MUTATION CHECK PASSED — in all three classes the unmutated refusal is what"
  note "turns the tamper away: with it disabled the mutant accepts the tampered bundle."
else
  note "MUTATION CHECK FAILED — see the FAILED lines above."
fi
exit $VERDICT
