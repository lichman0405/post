#!/usr/bin/env bash
#
# staticcheck gate (T0008): run the pinned staticcheck over the whole Go
# module and fail on any finding that is not grandfathered in
# ops/ci/staticcheck-baseline.txt (one "path/file.go:line:CHECK" entry per
# finding).
#
# Baseline semantics mirror ops/ci/gofmt-baseline.txt: findings that
# predate the gate stay listed with a follow-up to fix them; the gate scans
# every module package and only drops the listed findings, so new drift in
# the same packages still fails it.
#
# Test injection: with SC_REPORT_ON_STDIN=1 the report is read from stdin
# instead of running staticcheck, so the filter logic is fixture-testable
# (scripts/tests/staticcheck-unit-test.sh) without changing real code.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BASELINE=ops/ci/staticcheck-baseline.txt
VER="${STATICCHECK_VER:-2026.2.1}"

if [[ ! -f "$BASELINE" ]]; then
  echo "staticcheck: baseline file $BASELINE missing" >&2
  exit 2
fi

if [[ "${SC_REPORT_ON_STDIN:-0}" = "1" ]]; then
  report="$(cat)"
else
  # staticcheck prints findings on stdout and exits 1 when it found any;
  # the baseline may cover every finding, so its exit code is not the gate.
  # stderr is kept separate: "go run" appends "exit status 1" there, which
  # must not leak into the parsed report (real stderr is shown).
  errfile="$(mktemp)"
  trap 'rm -f "$errfile"' EXIT
  report="$(go run honnef.co/go/tools/cmd/staticcheck@"$VER" ./... 2>"$errfile" || true)"
  if grep -qv '^exit status [0-9][0-9]*$' "$errfile"; then
    cat "$errfile" >&2
  fi
fi

baseline_keys="$(grep -v '^#' "$BASELINE" | grep -v '^[[:space:]]*$' || true)"
bad=0
grandfathered=0
while IFS= read -r line; do
  [[ -z "$line" ]] && continue
  # "path/file.go:123:45: message (CHECK)" -> key "path/file.go:123:CHECK"
  key="$(sed -nE 's#^([^ :]+):([0-9]+):[0-9]+:.*\(([A-Za-z0-9]+)\)$#\1:\2:\3#p' <<<"$line")"
  if [[ -z "$key" ]]; then
    echo "staticcheck: unparseable report line: $line" >&2
    bad=1
    continue
  fi
  if grep -qxF "$key" <<<"$baseline_keys"; then
    grandfathered=$((grandfathered + 1))
    continue
  fi
  echo "$line"
  bad=1
done <<<"$report"

if (( bad )); then
  echo "" >&2
  echo "staticcheck: NEW findings above are not in $BASELINE." >&2
  echo "Fix them; only pre-existing findings may be baselined, with a" >&2
  echo "follow-up issue to fix them — never baseline new code." >&2
  exit 1
fi
if (( grandfathered )); then
  echo "staticcheck: clean ($grandfathered grandfathered baseline finding(s))"
else
  echo "staticcheck: clean"
fi
