#!/bin/bash
# tests-ledger-unit-test.sh — the refusal rules of the tests.json ledger tools.
#
# scripts/reconcile_tests_ledger.py was written on 2026-09-21 to stop
# tasks/tests.json being an empty ledger (22 passed of 173 blocking entries)
# while T1206's acceptance criterion reads "tests.json blocking all passed".
# Its whole safety argument is one sentence: **it never writes `passed` for an
# entry whose task is not merged, and never for one it cannot point at**.
# A claim like that in a commit message is worth nothing without a fixture that
# tries to break it, so this is that fixture.
#
# Every case below is a way the rule could be wrong, not a happy path:
#   1. merged + matching passed label   -> MUST flip
#   2. NOT merged (running)             -> MUST NOT flip, even though the label matches
#   3. merged, label not found          -> MUST NOT flip
#   4. merged, label found but failed   -> MUST NOT flip
#   5. record_test_run --exit-code 1    -> MUST write `failed`, never `passed`
#   6. record_test_run unknown id       -> MUST refuse (exit 2) and write nothing
# Each rule is tested BOTH ways where it matters: case 1 proves the tool is not
# simply inert (a tool that never writes anything would pass cases 2-4).
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

failures=0
ok()   { echo "ok   $1"; }
bad()  { echo "FAIL $1"; failures=$((failures + 1)); }

# --- the fixture -----------------------------------------------------------
# A minimal repository: the two state files the tools read, and one Worker
# record per task. Nothing else is needed — neither tool looks at the DAG.
mkdir -p "$WORK/tasks"
for t in T1001 T1002 T1003 T1004 T1005; do mkdir -p "$WORK/.rddev/workers/$t"; done

cat >"$WORK/tasks/task_status.json" <<'JSON'
{"tasks": {
  "T1001": {"status": "merged",  "merged_at": "2026-09-20T10:00:00Z"},
  "T1002": {"status": "running"},
  "T1003": {"status": "merged",  "merged_at": "2026-09-20T11:00:00Z"},
  "T1004": {"status": "merged",  "merged_at": "2026-09-20T12:00:00Z"},
  "T1005": {"status": "merged",  "merged_at": "2026-09-20T13:00:00Z"}
}}
JSON

cat >"$WORK/tasks/tests.json" <<'JSON'
{"version": 1, "tests": [
  {"id": "T1001-TEST-01", "task_id": "T1001", "name": "case one merged",      "gate": "G1/G2", "blocking": true, "status": "not_run", "last_run": "", "evidence": ""},
  {"id": "T1002-TEST-01", "task_id": "T1002", "name": "case two running",     "gate": "G1/G2", "blocking": true, "status": "not_run", "last_run": "", "evidence": ""},
  {"id": "T1003-TEST-01", "task_id": "T1003", "name": "case three no label",  "gate": "G1/G2", "blocking": true, "status": "not_run", "last_run": "", "evidence": ""},
  {"id": "T1004-TEST-01", "task_id": "T1004", "name": "case four failed run", "gate": "G1/G2", "blocking": true, "status": "not_run", "last_run": "", "evidence": ""},
  {"id": "T1005-TEST-01", "task_id": "T1005", "name": "case five near miss", "gate": "G1/G2", "blocking": true, "status": "not_run", "last_run": "", "evidence": ""}
]}
JSON

# T1001: the label matches and passed -> the one case that may flip.
# T1002: same, but the task is still running -> flipping it would record a pass
#        for work that no gate has seen. This is the dangerous one.
cat >"$WORK/.rddev/workers/T1002/RESULT.json" <<'JSON'
{"task_id": "T1002", "status": "completed", "tests": [
  {"label": "case two running", "command": "go test ./x/", "status": "passed"}
]}
JSON
cat >"$WORK/.rddev/workers/T1001/RESULT.json" <<'JSON'
{"task_id": "T1001", "status": "completed", "tests": [
  {"label": "case one merged", "command": "go test ./y/", "status": "passed"}
]}
JSON
# T1003: a record exists, but under a different label -> must not be matched.
cat >"$WORK/.rddev/workers/T1003/RESULT.json" <<'JSON'
{"task_id": "T1003", "status": "completed", "tests": [
  {"label": "something else entirely", "command": "go test ./z/", "status": "passed"}
]}
JSON
# T1004: the label matches but the recorded run FAILED.
cat >"$WORK/.rddev/workers/T1004/RESULT.json" <<'JSON'
{"task_id": "T1004", "status": "completed", "tests": [
  {"label": "case four failed run", "command": "go test ./w/", "status": "failed"}
]}
JSON
# T1005: a NEAR miss, not a miss. The label is the entry's name plus a suffix,
# which is the shape real records actually have -- T0205's reads
# "branch-domain-real-services (G3)", T0207's "(rework-requested re-run)". A
# matcher that asks "does the label contain the name" flips this one; asking for
# equality does not. This case exists because mutation 1 of this very test
# survived an earlier, weaker version of case 3.
cat >"$WORK/.rddev/workers/T1005/RESULT.json" <<'JSON'
{"task_id": "T1005", "status": "completed", "tests": [
  {"label": "case five near miss (rework)", "command": "go test ./v/", "status": "passed"}
]}
JSON

# --- the fixture is what this test thinks it is ---------------------------
# Learned the hard way: the first version of this file wrote the T1003/T1004
# records into directories it had not created, so those two cases "passed"
# because there was no RESULT.json at all -- i.e. for the opposite reason to the
# one they claim to test (unmatched label vs. no record). A fixture that failed
# to materialise makes the assertions below vacuous, so assert it materialised.
for t in T1001 T1002 T1003 T1004 T1005; do
  if ! python3 -c "
import json,sys
r=json.load(open('$WORK/.rddev/workers/$t/RESULT.json'))
assert r['tests'], 'no tests in the record'
" 2>/dev/null; then
    echo "FIXTURE BROKEN: $t has no readable RESULT.json — every case below would be vacuous"
    exit 2
  fi
done
# Case 3 must be testing "the label does not match", not "there is no record".
if grep -q "case three no label" "$WORK/.rddev/workers/T1003/RESULT.json"; then
  echo "FIXTURE BROKEN: T1003's record contains the label it must not match"
  exit 2
fi

status_of() { python3 -c "
import json,sys
d=json.load(open('$WORK/tasks/tests.json'))
print(next(t['status'] for t in d['tests'] if t['id']=='$1'))"; }
evidence_of() { python3 -c "
import json
d=json.load(open('$WORK/tasks/tests.json'))
print(next(t['evidence'] for t in d['tests'] if t['id']=='$1'))"; }

# --- the dry run is inert --------------------------------------------------
out=$(python3 "$ROOT/scripts/reconcile_tests_ledger.py" --repo "$WORK" 2>&1)
if grep -q "would flip to passed   : 1" <<<"$out"; then
  ok "dry run counts exactly the one flippable entry"
else
  bad "dry run counted the wrong number of flips: $(grep 'would flip' <<<"$out")"
fi
if [ "$(status_of T1001-TEST-01)" = "not_run" ]; then
  ok "dry run wrote nothing"
else
  bad "dry run modified the ledger"
fi

# --- apply ----------------------------------------------------------------
python3 "$ROOT/scripts/reconcile_tests_ledger.py" --repo "$WORK" --apply >/dev/null 2>&1

if [ "$(status_of T1001-TEST-01)" = "passed" ]; then
  ok "1. merged task with a matching passed label was recorded"
else
  bad "1. a fact already proven was NOT recorded: $(status_of T1001-TEST-01)"
fi
# The evidence must say where the pass came from, and must not claim a re-run.
ev="$(evidence_of T1001-TEST-01)"
if grep -q "not a fresh re-run" <<<"$ev" && grep -q "go test ./y/" <<<"$ev"; then
  ok "1. the evidence names the command and disclaims a re-run"
else
  bad "1. the evidence does not say where the pass came from: $ev"
fi

if [ "$(status_of T1002-TEST-01)" = "not_run" ]; then
  ok "2. a RUNNING task's entry was left alone (the rule that matters)"
else
  bad "2. a running task's entry was flipped to $(status_of T1002-TEST-01)"
fi
if [ "$(status_of T1003-TEST-01)" = "not_run" ]; then
  ok "3. a merged task with no matching label was left alone"
else
  bad "3. an unmatched label was matched anyway -> $(status_of T1003-TEST-01)"
fi
if [ "$(status_of T1004-TEST-01)" = "not_run" ]; then
  ok "4. a merged task whose recorded run FAILED was left alone"
else
  bad "4. a failed run was recorded as $(status_of T1004-TEST-01)"
fi
# 4b. The matcher must be an equality, not a "contains": a label that is the
# name plus a suffix is a different test until someone says otherwise, and
# saying otherwise is a human's call. (An earlier version of this file passed
# while a substring matcher was deliberately installed — this case is why.)
if [ "$(status_of T1005-TEST-01)" = "not_run" ]; then
  ok "4b. a near-miss label was left for a human, not matched"
else
  bad "4b. a near-miss label was matched -> $(status_of T1005-TEST-01)"
fi

# The residue report must name the merged-but-unprovable entries, so a human
# sees them rather than a silent gap: that is what T1206 has to run.
out=$(python3 "$ROOT/scripts/reconcile_tests_ledger.py" --repo "$WORK" --residue 2>&1)
if grep -q "T1003-TEST-01" <<<"$out" && grep -q "T1004-TEST-01" <<<"$out"; then
  ok "5. the residue report names the merged entries that lack evidence"
else
  bad "5. the residue report hid an unproven merged entry"
fi

# --- record_test_run.py ----------------------------------------------------
python3 "$ROOT/scripts/record_test_run.py" --repo "$WORK" --id T1003-TEST-01 \
  --command 'true' --exit-code 1 --apply >/dev/null 2>&1
if [ "$(status_of T1003-TEST-01)" = "failed" ]; then
  ok "6. a non-zero exit code is recorded as failed, not passed"
else
  bad "6. a non-zero exit code became $(status_of T1003-TEST-01)"
fi

before="$(cat "$WORK/tasks/tests.json")"
set +e
python3 "$ROOT/scripts/record_test_run.py" --repo "$WORK" --id T9999-TEST-99 \
  --command 'true' --exit-code 0 --apply >/dev/null 2>&1
rc=$?
set -e
if [ "$rc" -eq 2 ]; then
  ok "7. an unknown entry id is refused (exit 2)"
else
  bad "7. an unknown entry id exited $rc, want 2"
fi
if [ "$before" = "$(cat "$WORK/tasks/tests.json")" ]; then
  ok "7. the refusal wrote nothing"
else
  bad "7. the refusal modified the ledger"
fi

# 8. Output that looks like a credential must not be copied into a TRACKED
#    file. The pattern set is worker_collect.go's; the fixture value is a
#    syntactically valid but non-existent token, never a real one.
before="$(cat "$WORK/tasks/tests.json")"
printf 'PASS\nusing token ghp_%s\n' "0123456789abcdefghijklmnopqrstuvwxyz" >"$WORK/log-with-secret.txt"
set +e
out=$(python3 "$ROOT/scripts/record_test_run.py" --repo "$WORK" --id T1004-TEST-01 \
  --command 'true' --exit-code 0 --output-file "$WORK/log-with-secret.txt" --apply 2>&1)
rc=$?
set -e
if [ "$rc" -eq 3 ]; then
  ok "8. output matching a credential shape is refused (exit 3)"
else
  bad "8. a credential-shaped output was accepted (exit $rc)"
fi
# The refusal itself must not print the secret it found.
if grep -q "ghp_0123" <<<"$out"; then
  bad "8. the refusal echoed the credential it found"
else
  ok "8. the refusal names the class, not the matched text"
fi
if [ "$before" = "$(cat "$WORK/tasks/tests.json")" ]; then
  ok "8. the refusal wrote nothing"
else
  bad "8. the refusal modified the ledger"
fi

echo
if [ "$failures" -eq 0 ]; then
  echo "tests-ledger-unit-test: all cases passed"
  exit 0
fi
echo "tests-ledger-unit-test: $failures case(s) failed"
exit 1
