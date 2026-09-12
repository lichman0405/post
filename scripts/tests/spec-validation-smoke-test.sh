#!/usr/bin/env bash
#
# Real-repo smoke test for the POST spec validation suite.
#
# Runs the REAL validators against this checkout:
#   1. validate_specs.py on the live tree (CI gate)
#   2. spec_version.py --check (derived marker vs. live inputs)
#   3. source_repo_preflight.py with the real git remote:
#      - when the environment allows `git remote -v` (CI, Supervisor), the
#        live capture is parsed and cross-checked by an independent
#        normalization implemented inline below;
#      - when the environment denies it (Worker guard), the preflight must
#        report the remote as unverifiable and refuse to bless — the
#        refusal is asserted either way (visibility is unknown without an
#        operator input, and a preflight must never silently pass).
#   4. The recorded fixture of this worktree's real remote output
#      (scripts/tests/fixtures/origin-real.remote-v.txt) proves the parse
#      of the real remote bytes when live capture is unavailable.
#
# The verdict itself is not required to be "bless" — the repo state is
# judged by the preflight; this test judges the preflight.
#
# Runnable on a host with bash + coreutils + python3 (stdlib only).
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
VALIDATOR="$ROOT/scripts/validate_specs.py"
PREFLIGHT="$ROOT/scripts/source_repo_preflight.py"
SPEC_VERSION="$ROOT/scripts/spec_version.py"
FIXTURE_DIR="$ROOT/scripts/tests/fixtures"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

jget() { python3 - "$@" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for k in sys.argv[2:]:
    d = d[int(k)] if isinstance(d, list) else d[k]
print(d)
PY
}

find_check() { python3 - "$@" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
c = [x for x in d["checks"] if x["id"] == sys.argv[2]][0]
v = c.get(sys.argv[3], "")
print("" if v is None else v)
PY
}

cd "$ROOT" || exit 1

# ---------------------------------------------------------------- 0. syntax
for f in "$ROOT/scripts/validate_specs.py" "$ROOT/scripts/spec_version.py" \
         "$ROOT/scripts/source_repo_preflight.py"; do
  if python3 -m py_compile "$f"; then ok "py_compile $(basename "$f")"; else fail "py_compile $(basename "$f")"; fi
done
if bash -n "$0"; then ok "bash -n spec-validation-smoke-test.sh"; else fail "bash -n spec-validation-smoke-test.sh"; fi

# ------------------------------------------------- 1. validate_specs (live)
"$VALIDATOR" >"$WORK/val.out" 2>"$WORK/val.err"; VAL_RC=$?
if [[ "$VAL_RC" == 0 ]]; then ok "validate_specs live tree: exit 0"; else fail "validate_specs live tree: exit $VAL_RC"; cat "$WORK/val.out"; fi
"$VALIDATOR" --json >"$WORK/val.json" 2>/dev/null
assert_field() { # desc expected actual
  if [[ "$2" == "$3" ]]; then ok "$1"; else fail "$1: expected [$2] got [$3]"; fi
}
assert_field "validate_specs --json: exit_code field" "0" "$(jget "$WORK/val.json" exit_code)"
assert_field "validate_specs --json: ok field" "True" "$(jget "$WORK/val.json" ok)"

# --------------------------------------------- 2. spec_version --check (live)
"$SPEC_VERSION" --check >"$WORK/ver.out" 2>"$WORK/ver.err"; VER_RC=$?
if [[ "$VER_RC" == 0 ]]; then ok "spec_version --check: marker current"; else fail "spec_version --check: exit $VER_RC"; cat "$WORK/ver.out"; fi

# ---------------------------------- 3. preflight real mode (live git remote)
"$PREFLIGHT" --json >"$WORK/pf1.json" 2>"$WORK/pf1.err"; PF_RC=$?
"$PREFLIGHT" --json >"$WORK/pf2.json" 2>"$WORK/pf2.err"
if python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$WORK/pf1.json" 2>/dev/null; then
  ok "preflight real mode: stdout is valid JSON"
else
  fail "preflight real mode: stdout is not valid JSON (rc=$PF_RC)"
fi

# the preflight must never silently pass: without a visibility observation
# the verdict must be refused, in every environment
assert_field "preflight real mode: exit code is refused (1)" "1" "$PF_RC"
assert_field "preflight real mode: exit_code field" "1" "$(jget "$WORK/pf1.json" exit_code)"
assert_field "preflight real mode: push_blessing" "refused" "$(jget "$WORK/pf1.json" verdict push_blessing)"
if [[ "$(jget "$WORK/pf1.json" verdict reasons)" == *"visibility_unverifiable"* ]]; then
  ok "preflight real mode: refuses on unverified visibility (no silent pass)"
else
  fail "preflight real mode: expected visibility_unverifiable in reasons: $(jget "$WORK/pf1.json" verdict reasons)"
fi

# spec-side checks must pass on the live tree
for chk in SPEC-PARSE SPEC-CANONICAL SPEC-EXPECTATION SPEC-VERSION; do
  assert_field "preflight real mode: $chk passed" "passed" "$(find_check "$WORK/pf1.json" "$chk" status)"
done

# live remote capture: parse the real `git remote -v` output independently
# (regex, not speclib) and cross-check what the preflight reported
python3 - "$WORK/live-remote.txt" <<'PY'
import re, subprocess, sys
outfile = sys.argv[1]
proc = subprocess.run(["git", "remote", "-v"], capture_output=True, text=True)
if proc.returncode == 0 and proc.stdout.strip():
    m = re.match(r"origin\s+(\S+://\S+|git@\S+:\S+)\s+\(fetch\)", proc.stdout)
    lines = ["LIVE_CAPTURED"]
    lines.append(m.group(1) if m else "NO_MATCH")
    open(outfile, "w").write("\n".join(lines) + "\n")
else:
    first = proc.stderr.strip().splitlines()[0] if proc.stderr.strip() else ""
    open(outfile, "w").write("LIVE_DENIED\n%s\n" % first)
PY
LIVE_STATUS="$(sed -n 1p "$WORK/live-remote.txt" 2>/dev/null)"
REPO_STATUS="$(find_check "$WORK/pf1.json" REPO-CANONICAL status)"
PF_MEASURED="$(find_check "$WORK/pf1.json" REPO-CANONICAL measured)"
if [[ "$LIVE_STATUS" == "LIVE_CAPTURED" ]]; then
  RAW_URL="$(sed -n 2p "$WORK/live-remote.txt")"
  if [[ "$RAW_URL" == "NO_MATCH" ]]; then
    fail "live remote capture: test-side regex did not match real git remote -v output"
  fi
  # independent normalization: strip scheme/host/.git -> owner/name
  NORM="$(printf '%s' "$RAW_URL" | python3 -c 'import sys; u=sys.stdin.read().strip(); u=u.replace("https://github.com/","").replace("git@github.com:",""); print(u[:-4] if u.endswith(".git") else u)')"
  if [[ "$PF_MEASURED" == "lichman0405/post" ]]; then
    assert_field "live remote: preflight normalized real output" "passed" "$REPO_STATUS"
    assert_field "live remote: independent normalization agrees" "$PF_MEASURED" "$NORM"
    assert_field "live remote: canonical lichman0405/post observed" "lichman0405/post" "$PF_MEASURED"
  else
    assert_field "live remote: non-canonical origin rejected" "failed" "$REPO_STATUS"
    if [[ "$(jget "$WORK/pf1.json" verdict reasons)" == *"remote_not_canonical"* ]]; then
      ok "live remote: non-canonical origin carries remote_not_canonical"
    else
      fail "live remote: expected remote_not_canonical in reasons: $(jget "$WORK/pf1.json" verdict reasons)"
    fi
  fi
elif [[ "$LIVE_STATUS" == "LIVE_DENIED" ]]; then
  # The environment denies `git remote -v` (e.g. Worker guard): the
  # preflight must report the remote honestly as unverifiable and refuse —
  # never guess
  assert_field "denied live remote: REPO-CANONICAL unknown" "unknown" "$REPO_STATUS"
  if [[ "$(jget "$WORK/pf1.json" verdict reasons)" == *"remote_unverifiable"* ]]; then
    ok "denied live remote: verdict refuses with remote_unverifiable"
  else
    fail "denied live remote: expected remote_unverifiable in reasons: $(jget "$WORK/pf1.json" verdict reasons)"
  fi
  if [[ "$(jget "$WORK/pf1.json" verdict reasons)" == *"git remote -v rc="* ]]; then
    ok "denied live remote: detail records the capture failure"
  else
    fail "denied live remote: detail does not record the capture failure"
  fi
else
  fail "live remote capture: unexpected test-side state [$LIVE_STATUS]"
fi

# determinism of real-mode output
if cmp -s "$WORK/pf1.json" "$WORK/pf2.json"; then
  ok "preflight real mode: two --json runs byte-identical"
else
  fail "preflight real mode: two --json runs differ"
fi

# ---------------------------------- 4. recorded real-remote fixture fallback
"$PREFLIGHT" --fixture "$FIXTURE_DIR" --json \
  >"$WORK/pf-fix.json" 2>"$WORK/pf-fix.err"; FIX_RC=$?
assert_field "recorded fixture: exit code" "1" "$FIX_RC"
assert_field "recorded fixture: REPO-CANONICAL passed" "passed" "$(find_check "$WORK/pf-fix.json" REPO-CANONICAL status)"
assert_field "recorded fixture: normalized real origin" "lichman0405/post" "$(find_check "$WORK/pf-fix.json" REPO-CANONICAL measured)"
assert_field "recorded fixture: source" "fixture" "$(find_check "$WORK/pf-fix.json" REPO-CANONICAL source)"

# --------------------------------------- 5. output schema (when jsonschema)
if python3 -c "import jsonschema" 2>/dev/null; then
  SCHEMA_OUT="$(python3 - "$WORK/pf1.json" "$ROOT/specs/orchestrator/source-repo-preflight.schema.json" <<'PY'
import json, sys
import jsonschema
data = json.load(open(sys.argv[1]))
schema = json.load(open(sys.argv[2]))
try:
    jsonschema.validate(data, schema)
    print("SCHEMA_OK")
except Exception as exc:
    print("SCHEMA_FAIL " + str(exc))
PY
)"
  if [[ "$SCHEMA_OUT" == "SCHEMA_OK" ]]; then
    ok "preflight --json validates against source-repo-preflight.schema.json"
  else
    fail "preflight --json schema validation: $SCHEMA_OUT"
  fi
else
  ok "jsonschema not installed — schema validation skipped (optional)"
fi

# ------------------------------------------------------------------ result
echo
if [[ "$FAILS" == 0 ]]; then
  echo "ALL SMOKE TESTS PASSED"
  exit 0
else
  echo "$FAILS SMOKE TEST(S) FAILED"
  exit 1
fi
