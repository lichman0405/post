#!/usr/bin/env bash
#
# Unit tests for scripts/staticcheck.sh (fixture-driven).
#
# Exercises the baseline filter against injected staticcheck-style reports
# (SC_REPORT_ON_STDIN=1) plus one case with the baseline file removed —
# never runs staticcheck itself and never touches real code. Runnable on a
# host with bash + coreutils.
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
GATE="$ROOT/scripts/staticcheck.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILS=0
RC=0; OUT=""; ERR=""

fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

run_gate() { # report-string -> sets RC/OUT/ERR
  OUT="$(SC_REPORT_ON_STDIN=1 bash "$GATE" <<<"$1" 2>"$WORK/err")"
  RC=$?
  ERR="$(cat "$WORK/err")"
}

# Baseline findings (exact lines from the real staticcheck run).
BASELINE_REPORT='
cmd/rddev/main_test.go:178:4: this value of msg is never used (SA4006)
internal/devorchestrator/doctor/catalog.go:11:5: var envIDs is unused (U1000)
internal/devorchestrator/doctor/catalog.go:13:5: var resIDs is unused (U1000)
internal/devorchestrator/doctor/eval.go:28:16: func Check.ok is unused (U1000)
internal/worker/queue.go:63:14: (*Client).BRPopLPush is deprecated (SA1019)
internal/worker/queue.go:111:16: (*Client).RPopLPush is deprecated (SA1019)
internal/worker/worker_test.go:144:12: (*Client).BRPopLPush is deprecated (SA1019)
'

# --- 1. report containing only baseline findings passes ----------------------
run_gate "$BASELINE_REPORT"
if [[ $RC -eq 0 ]]; then ok "only baseline findings: rc=0"; else fail "only baseline findings: rc=$RC out=$OUT err=$ERR"; fi

# --- 2. a new finding not in the baseline fails and is named -----------------
run_gate "$BASELINE_REPORT
internal/devorchestrator/doctor/new.go:5:2: var unusedThing is unused (U1000)"
if [[ $RC -eq 1 && "$OUT" == *"internal/devorchestrator/doctor/new.go:5:2"* ]]; then
  ok "new finding: rc=1, finding named"
else
  fail "new finding: rc=$RC out=$OUT"
fi

# --- 3. new finding in an already-baselined package still fails ---------------
run_gate "internal/worker/queue.go:200:1: this value of x is never used (SA4006)"
if [[ $RC -eq 1 ]]; then ok "new finding in baselined package: rc=1"; else fail "new finding in baselined package: rc=$RC out=$OUT"; fi

# --- 4. same finding at a different line is NOT covered by the baseline ------
run_gate "internal/worker/queue.go:64:14: BRPopLPush is deprecated (SA1019)"
if [[ $RC -eq 1 ]]; then ok "baseline is per line, not per file: rc=1"; else fail "baseline per-line: rc=$RC out=$OUT"; fi

# --- 5. unparseable report line fails ----------------------------------------
run_gate "this is not a staticcheck report line"
if [[ $RC -eq 1 && "$ERR" == *"unparseable"* ]]; then ok "unparseable line: rc=1, named"; else fail "unparseable line: rc=$RC err=$ERR"; fi

# --- 6. empty report passes ---------------------------------------------------
run_gate ""
if [[ $RC -eq 0 ]]; then ok "empty report: rc=0"; else fail "empty report: rc=$RC out=$OUT"; fi

# --- 7. missing baseline file is exit 2 ---------------------------------------
OUT="$(SC_REPORT_ON_STDIN=1 bash -c 'cd "$1" && mv ops/ci/staticcheck-baseline.txt ops/ci/staticcheck-baseline.txt.bak && bash scripts/staticcheck.sh </dev/null; rc=$?; mv ops/ci/staticcheck-baseline.txt.bak ops/ci/staticcheck-baseline.txt; exit $rc' _ "$ROOT" 2>"$WORK/err7")"
RC=$?
if [[ $RC -eq 2 ]]; then ok "missing baseline file: rc=2"; else fail "missing baseline file: rc=$RC out=$OUT"; fi

# --- 8. comment/blank lines in the baseline are ignored ----------------------
# (implicitly covered by case 1: the baseline file starts with comment lines)

# --- summary ------------------------------------------------------------------
if [[ $FAILS -eq 0 ]]; then
  echo "staticcheck-unit-test: all cases passed"
  exit 0
fi
echo "staticcheck-unit-test: $FAILS case(s) FAILED" >&2
exit 1
