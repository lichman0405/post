#!/usr/bin/env bash
#
# Unit tests for scripts/web-unit-tests.sh (fixture-driven).
#
# The gate has exactly one branch worth proving: the zero-match guard. A
# glob that matches nothing must FAIL, because `node --test` on an empty
# match set exits 0 reporting "tests 0" — a green stage that ran nothing.
# That is the whole reason the guard exists, so it is the case tested here.
#
# Never touches the real apps/web tests: WEB_TEST_GLOB is pointed at
# generated fixtures in a temp tree.
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
GATE="$ROOT/scripts/web-unit-tests.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

if [[ ! -x "$GATE" ]]; then
  fail "scripts/web-unit-tests.sh is missing or not executable"
  printf '\nweb-tests-unit-test: %d failure(s)\n' "$FAILS"
  exit 1
fi

if [[ -n "${WEB_TEST_GLOB:-}" ]]; then
  fail "WEB_TEST_GLOB leaked in from the environment; this test needs it unset"
fi

# --- case 1: a glob matching nothing must fail loudly -----------------------
# The gate cd's to the repo root, so the override is an absolute pattern.
mkdir -p "$WORK/empty"
OUT="$(WEB_TEST_GLOB="$WORK/empty/*.test.mjs" "$GATE" 2>&1 </dev/null)"
RC=$?
if (( RC == 0 )); then
  fail "empty glob exited 0 — a stage that ran no test would report success"
elif [[ "$OUT" != *"matched no test file"* ]]; then
  fail "empty glob failed without naming the cause: $OUT"
else
  ok "empty glob: rc=$RC and names the cause"
fi

# --- case 2: the real glob finds the real tests -----------------------------
OUT="$("$GATE" 2>&1 </dev/null)"
RC=$?
if (( RC != 0 )); then
  fail "the real glob failed (rc=$RC): $OUT"
elif [[ "$OUT" != *"file(s) matched"* ]]; then
  fail "the real glob did not report how many files it matched: $OUT"
elif ! grep -qE '[1-9][0-9]* file\(s\) matched' <<<"$OUT"; then
  fail "the real glob matched zero files: $OUT"
else
  ok "real glob: $(grep -oE '[0-9]+ file\(s\) matched' <<<"$OUT")"
fi

# --- case 3: the guard does not hide a real failure -------------------------
mkdir -p "$WORK/failing"
cat >"$WORK/failing/broken.test.mjs" <<'JS'
import { test } from "node:test";
import assert from "node:assert";
test("deliberately failing", () => { assert.strictEqual(1, 2); });
JS
OUT="$(WEB_TEST_GLOB="$WORK/failing/*.test.mjs" "$GATE" 2>&1 </dev/null)"
RC=$?
if (( RC == 0 )); then
  fail "a failing test exited 0 — the gate would not catch a real regression"
else
  ok "a failing test still fails the gate (rc=$RC)"
fi

printf '\n'
if (( FAILS )); then
  printf 'web-tests-unit-test: %d failure(s)\n' "$FAILS"
  exit 1
fi
printf 'web-tests-unit-test: all cases passed\n'
