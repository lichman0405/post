#!/usr/bin/env bash
#
# Unit tests for scripts/validate_openapi.py (fixture-driven).
#
# Exercises the OpenAPI contract checks against injected fixture trees
# (--root); never touches the real specs/api/openapi.yaml except for one
# read-only pass on the canonical contract. Runnable on a host with
# bash + coreutils + python3 (PyYAML required, like the CI spec stage).
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
VALIDATOR="$ROOT/scripts/validate_openapi.py"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILS=0
RC=0; OUT=""; ERR=""

fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

# mkdoc DIR YAML — writes DIR/specs/api/openapi.yaml
mkdoc() {
  mkdir -p "$1/specs/api"
  printf '%s\n' "$2" > "$1/specs/api/openapi.yaml"
}

run_validator() { # dir [extra args...] -> sets RC/OUT/ERR
  OUT="$(python3 "$VALIDATOR" --root "$1" "${@:2}" 2>"$WORK/err")"
  RC=$?
  ERR="$(cat "$WORK/err")"
}

BASE='
openapi: 3.1.0
info:
  title: Fixture API
  version: 0.1.0
paths:
  /things:
    get:
      summary: list
      parameters:
        - $ref: "#/components/parameters/ThingId"
      responses:
        "200": {description: OK}
components:
  securitySchemes:
    bearerAuth: {type: http, scheme: bearer}
  parameters:
    ThingId: {name: thingId, in: query, schema: {type: string}}
security:
  - bearerAuth: []
'

# --- 1. valid fixture passes ------------------------------------------------
mkdoc "$WORK/clean" "$BASE"
run_validator "$WORK/clean"
if [[ $RC -eq 0 ]]; then ok "clean fixture: rc=0"; else fail "clean fixture: rc=$RC out=$OUT err=$ERR"; fi

# --- 2. dangling internal ref ------------------------------------------------
mkdoc "$WORK/dangling-ref" "${BASE/\#\/components\/parameters\/ThingId/\#\/components\/parameters\/Nope}"
run_validator "$WORK/dangling-ref"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "OPENAPI-REFS"; then
  ok "dangling ref: rc=1, OPENAPI-REFS"
else
  fail "dangling ref: rc=$RC out=$OUT"
fi

# --- 3. external ref rejected (offline determinism) -------------------------
mkdoc "$WORK/external-ref" "${BASE/\#\/components\/parameters\/ThingId/https:\/\/example.com\/thing.yaml}"
run_validator "$WORK/external-ref"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "OPENAPI-REFS"; then
  ok "external ref: rc=1, OPENAPI-REFS"
else
  fail "external ref: rc=$RC out=$OUT"
fi

# --- 4. non-3.1 version -------------------------------------------------------
mkdoc "$WORK/bad-version" "${BASE/openapi: 3.1.0/openapi: 3.0.0}"
run_validator "$WORK/bad-version"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "OPENAPI-VERSION"; then
  ok "non-3.1 version: rc=1, OPENAPI-VERSION"
else
  fail "non-3.1 version: rc=$RC out=$OUT"
fi

# --- 5. missing info ---------------------------------------------------------
mkdoc "$WORK/no-info" "$(printf '%s\n' "$BASE" | grep -v -e 'info:' -e 'title:' -e 'version:')"
run_validator "$WORK/no-info"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "OPENAPI-INFO"; then
  ok "missing info: rc=1, OPENAPI-INFO"
else
  fail "missing info: rc=$RC out=$OUT"
fi

# --- 6. empty paths -----------------------------------------------------------
mkdoc "$WORK/no-paths" "$(printf '%s\n' "$BASE" | sed 's|^paths:$|paths: {}|; s|^  /things:$||; /^    get:$/d; /summary: list/d; /parameters:/d; /- \$ref:/d; /responses:/d; /"200":/d')"
run_validator "$WORK/no-paths"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "OPENAPI-PATHS"; then
  ok "empty paths: rc=1, OPENAPI-PATHS"
else
  fail "empty paths: rc=$RC out=$OUT"
fi

# --- 7. operation without responses ------------------------------------------
mkdoc "$WORK/no-responses" "$(printf '%s\n' "$BASE" | sed '/responses:/d; /"200": {description: OK}/d')"
run_validator "$WORK/no-responses"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "OPENAPI-OPS"; then
  ok "operation without responses: rc=1, OPENAPI-OPS"
else
  fail "operation without responses: rc=$RC out=$OUT"
fi

# --- 8. parameter without name/in --------------------------------------------
mkdoc "$WORK/bad-param" "${BASE/name: thingId, in: query, /}"
run_validator "$WORK/bad-param"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "OPENAPI-OPS"; then
  ok "parameter without name/in: rc=1, OPENAPI-OPS"
else
  fail "parameter without name/in: rc=$RC out=$OUT"
fi

# --- 9. duplicate operationId -------------------------------------------------
mkdoc "$WORK/dup-opid" 'openapi: 3.1.0
info:
  title: Fixture API
  version: 0.1.0
paths:
  /things:
    get:
      summary: list
      operationId: listThings
      responses:
        "200": {description: OK}
  /other:
    get:
      operationId: listThings
      responses:
        "200": {description: OK}
'
run_validator "$WORK/dup-opid"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "OPENAPI-OPIDS"; then
  ok "duplicate operationId: rc=1, OPENAPI-OPIDS"
else
  fail "duplicate operationId: rc=$RC out=$OUT"
fi

# --- 10. security requirement for an undefined scheme ------------------------
mkdoc "$WORK/bad-security" "${BASE/bearerAuth: \[\]/nonexistent: \[\]}"
run_validator "$WORK/bad-security"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "OPENAPI-SECURITY"; then
  ok "undefined security scheme: rc=1, OPENAPI-SECURITY"
else
  fail "undefined security scheme: rc=$RC out=$OUT"
fi

# --- 11. invalid YAML ----------------------------------------------------------
mkdoc "$WORK/bad-yaml" 'openapi: [unclosed'
run_validator "$WORK/bad-yaml"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "OPENAPI-PARSE"; then
  ok "invalid YAML: rc=1, OPENAPI-PARSE"
else
  fail "invalid YAML: rc=$RC out=$OUT"
fi

# --- 12. missing contract file --------------------------------------------------
mkdir -p "$WORK/missing/specs/api"
run_validator "$WORK/missing"
if [[ $RC -eq 1 ]] && echo "$OUT" | grep -q "OPENAPI-PARSE"; then
  ok "missing contract: rc=1, OPENAPI-PARSE"
else
  fail "missing contract: rc=$RC out=$OUT"
fi

# --- 13. ref'd parameter with valid target passes (regression: $ref params) ---
run_validator "$WORK/clean"
if [[ $RC -eq 0 ]]; then ok "ref'd parameter fixture: rc=0"; else fail "ref'd parameter fixture: rc=$RC out=$OUT"; fi

# --- 14. the canonical contract passes -----------------------------------------
run_validator "$ROOT"
if [[ $RC -eq 0 ]]; then ok "canonical specs/api/openapi.yaml: rc=0"; else fail "canonical contract: rc=$RC out=$OUT"; fi

# --- summary --------------------------------------------------------------------
echo ""
if [[ $FAILS -eq 0 ]]; then
  echo "openapi-unit-test: all cases passed"
  exit 0
fi
echo "openapi-unit-test: $FAILS case(s) failed"
exit 1
