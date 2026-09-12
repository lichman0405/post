#!/usr/bin/env bash
# T0012-TEST-01 — the four-gate acceptance e2e (tests/acceptance/four-gate-e2e.sh).
#
# Exercises the full Supervisor loop on a scratch repo with a fake claude:
#   ready -> spawn -> collect (G1) -> G2 (CI steps) -> G3 (task override) ->
#   review worker -> accept -> git commit/push -> pr open/merge (G4).
# Proves, mechanically:
#   - a red/missing gate REFUSES git commit/push and pr open/merge BEFORE
#     git/gh is ever invoked (fake recorder stays empty);
#   - the merge gate demands the latest G2 cover every required job;
#   - a Worker editing its own gate inputs (task-package.json scope,
#     registry baseline) is detected as tamper AND collect still judges the
#     diff by the AUTHORITATIVE scope (the out-of-scope file is refused).
#
# Runnable on a host with bash + coreutils + git + go + python3.
set -u
export LC_ALL=C

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=four-gate-helpers.sh
. "$HERE/four-gate-helpers.sh"

fg_build "$FG_SCRATCH" || exit 1

TASKS='{
  "version": 1,
  "task_count": 2,
  "phases": [{"id": "P0", "name": "e2e"}],
  "tasks": [
    {"id": "T0001", "phase": "P0", "phase_name": "e2e", "title": "e2e task one",
     "v1_required": false, "dependencies": [],
     "requirements": ["write a deliverable inside the allowed scope"],
     "deliverables": ["internal/config/deliverable.txt"],
     "acceptance_criteria": ["deliverable exists inside the allowed scope"],
     "tests": ["e2e-check.sh"],
     "allowed_scope": ["internal/config/**"]},
    {"id": "T0002", "phase": "P0", "phase_name": "e2e", "title": "e2e task two",
     "v1_required": false, "dependencies": [],
     "requirements": ["attempt to widen its own scope"],
     "deliverables": [],
     "acceptance_criteria": ["tamper is detected and refused"],
     "tests": ["e2e-check.sh"],
     "allowed_scope": ["internal/config/**"]}
  ]
}'
# G2 = two fast echo jobs (the real CI's exact steps are pinned by the
# gates-sync unit test against .github/workflows/ci.yml in the real repo);
# G3 is per-task via task_overrides; review is required for merge.
GATES='{
  "version": 1,
  "required_jobs": ["job-a", "job-b"],
  "gates": {
    "G1": {"name": "worker", "description": "collect", "runs_jobs": [], "asserts_jobs": []},
    "G2": {"name": "accept", "description": "CI steps", "runs_jobs": ["job-a", "job-b"], "asserts_jobs": []},
    "G3": {"name": "e2e", "description": "per task", "runs_jobs": [], "asserts_jobs": []},
    "G4": {"name": "merge", "description": "assert", "runs_jobs": [], "asserts_jobs": ["job-a", "job-b"]}
  },
  "jobs": {
    "job-a": {"steps": [{"run": "echo a-ok"}]},
    "job-b": {"steps": [{"run": "echo b-ok"}]},
    "job-g3": {"steps": [{"run": "echo e2e-ok"}]}
  },
  "review": {"required_for_merge": true},
  "task_overrides": {"T0001": {"g3_jobs": ["job-g3"]}}
}'

REPO="$(fg_setup_repo "$FG_SCRATCH" "$TASKS" "$GATES")" || exit 1

# --- fakes ---------------------------------------------------------------
# git+gh recorder for the refusal phase: any invocation appends to the marker.
mkdir -p "$FG_SCRATCH/refuse-bin"
for n in git gh; do
  printf '#!/bin/sh\necho "$0 $*" >> "%s"\nexit 0\n' "$FG_SCRATCH/fake-invocations.log" > "$FG_SCRATCH/refuse-bin/$n"
  chmod +x "$FG_SCRATCH/refuse-bin/$n"
done
: > "$FG_SCRATCH/fake-invocations.log"

# gh recorder for the green phase (real git stays on PATH): pr view -> no
# existing PR (exit 1), pr create -> 42, pr merge -> a fake merge sha.
mkdir -p "$FG_SCRATCH/gh-bin"
cat > "$FG_SCRATCH/gh-bin/gh" <<EOF
#!/bin/bash
echo "gh \$*" >> "$FG_SCRATCH/gh-invocations.log"
case "\$1 \$2" in
  "pr view") exit 1;;
  "pr create") echo "42";;
  "pr checks") printf '%s' '[{"name":"job-a","state":"SUCCESS"},{"name":"job-b","state":"SUCCESS"}]';;
  "pr merge") echo "0000000000000000000000000000000000000000";;
  *) echo "unexpected gh call: \$*" >&2; exit 9;;
esac
EOF
chmod +x "$FG_SCRATCH/gh-bin/gh"
: > "$FG_SCRATCH/gh-invocations.log"

fg_good_worker_bin "$FG_SCRATCH/bin/claude"
fg_review_worker_bin "$FG_SCRATCH/bin/claude-review"
# The tamper worker: writes OUTSIDE the authoritative scope, then edits its
# own gate inputs (package scope -> "**", registry baseline -> deadbeef) and
# still claims completion.
fg_good_result_py
cat > "$FG_SCRATCH/tamper-body.sh" <<'EOF'
#!/usr/bin/env bash
set -u
mkdir -p "$POST_WORKER_WORKTREE/apps"
echo "evil" > "$POST_WORKER_WORKTREE/apps/evil.txt"
# The registry is written by spawn right after dispatch — wait for it.
n=0
while [ ! -f "$POST_WORKER_RESULT_DIR/registry.json" ] && [ $n -lt 20 ]; do sleep 0.25; n=$((n+1)); done
python3 - <<'PY'
import json, os
rdir = os.environ["POST_WORKER_RESULT_DIR"]
pkg = json.load(open(os.path.join(rdir, "task-package.json")))
pkg["allowed_scope"] = ["**"]
json.dump(pkg, open(os.path.join(rdir, "task-package.json"), "w"))
reg = json.load(open(os.path.join(rdir, "registry.json")))
reg["baseline_sha"] = "deadbeef"
json.dump(reg, open(os.path.join(rdir, "registry.json"), "w"))
PY
python3 "$FG_SCRATCH/good-result.py" '["apps/evil.txt"]'
EOF
chmod +x "$FG_SCRATCH/tamper-body.sh"
fg_fake_claude "$FG_SCRATCH/bin/claude-tamper" "$FG_SCRATCH/tamper-body.sh"

# A push target so the green-phase push is a real local push.
git init -q --bare "$FG_SCRATCH/origin.git"
(cd "$REPO" && git remote add origin "$FG_SCRATCH/origin.git")

# --- 1. dispatch ---------------------------------------------------------
fg_run "$REPO" task ready T0001
fg_assert_eq 0 "$FG_RC" "task ready T0001"
fg_run "$REPO" task inspect T0001 --json
fg_assert_contains '"ready"' "$FG_OUT" "T0001 is ready"

# --- 2. red gate refuses git/pr BEFORE any invocation (T0012 req 4) ------
for action in "git commit" "git push" "pr open" "pr merge"; do
  fg_dump
  FG_OUT="$(cd "$REPO" && PATH="$FG_SCRATCH/refuse-bin:$PATH" "$FG_SCRATCH/bin/rddev" $action T0001 2>&1)"
  FG_RC=$?
  fg_assert_eq 1 "$FG_RC" "$action refused (exit 1) on a red gate"
  fg_assert_contains "REFUSED" "$FG_OUT" "$action prints REFUSED"
  fg_assert_contains "was never invoked" "$FG_OUT" "$action proves no git/gh invocation"
done
fg_dump
fg_assert_eq "" "$(cat "$FG_SCRATCH/fake-invocations.log")" "git/gh recorder stayed EMPTY across all refusals"
fg_assert_eq 0 "$(wc -c < "$FG_SCRATCH/fake-invocations.log")" "no control-plane binary was ever invoked"

# --- 3. worker -> collect = G1 ------------------------------------------
fg_run "$REPO" worker spawn T0001 --claude-bin "$FG_SCRATCH/bin/claude"
fg_assert_eq 0 "$FG_RC" "worker spawn T0001"
fg_wait_exit "$REPO" T0001 30 || fg_fail "worker T0001 did not exit"
fg_run "$REPO" worker collect T0001
fg_assert_eq 0 "$FG_RC" "worker collect T0001 (G1 green)"
fg_assert_contains "collect ok" "$FG_OUT" "collect report is ok"
fg_run "$REPO" task inspect T0001 --json
fg_assert_contains '"verification"' "$FG_OUT" "T0001 is in verification"

# G1 green but G2 missing: the merge gate must STILL refuse.
fg_run "$REPO" gate status T0001
fg_assert_contains "REFUSED" "$FG_OUT" "merge gate refuses with G2 missing"
FG_OUT="$(cd "$REPO" && PATH="$FG_SCRATCH/refuse-bin:$PATH" "$FG_SCRATCH/bin/rddev" git commit T0001 2>&1)"
FG_RC=$?
fg_assert_eq 1 "$FG_RC" "git commit refused after collect with no G2"
fg_assert_contains "REFUSED" "$FG_OUT" "refusal names the four-gate assertion"
fg_dump
fg_assert_eq 0 "$(wc -c < "$FG_SCRATCH/fake-invocations.log")" "recorder still empty after the post-collect refusal"

# --- 4. G2 + G3 (the exact-steps executor) ------------------------------
fg_run "$REPO" gate run G2 T0001
fg_assert_eq 0 "$FG_RC" "gate run G2 T0001"
fg_assert_contains "G2 passed" "$FG_OUT" "G2 is green"
fg_run "$REPO" gate run G3 T0001
fg_assert_eq 0 "$FG_RC" "gate run G3 T0001 (task override)"
fg_assert_contains "G3 passed" "$FG_OUT" "G3 is green"
fg_run "$REPO" gate records T0001
fg_assert_contains "gate-run" "$FG_OUT" "G2/G3 records exist on disk"

# --- 5. independent Review Worker ----------------------------------------
fg_run "$REPO" review spawn T0001 --claude-bin "$FG_SCRATCH/bin/claude-review"
fg_assert_eq 0 "$FG_RC" "review spawn T0001"
fg_wait_exit "$REPO" T0001-review 30 || fg_fail "review worker did not exit"
fg_run "$REPO" review collect T0001
fg_assert_eq 0 "$FG_RC" "review collect T0001"
fg_assert_contains "approve" "$FG_OUT" "the review verdict is approve"

# --- 6. accept -----------------------------------------------------------
fg_run "$REPO" task accept T0001
fg_assert_eq 0 "$FG_RC" "task accept T0001 (G2+G3 green)"
fg_run "$REPO" task inspect T0001 --json
fg_assert_contains '"accepted"' "$FG_OUT" "T0001 is accepted"

# --- 7. G4 green: the Supervisor git/PR actions run and are recorded -----
fg_run "$REPO" gate status T0001
fg_assert_contains "passed" "$FG_OUT" "merge gate is green after accept"
fg_run "$REPO" git commit T0001
fg_assert_eq 0 "$FG_RC" "git commit T0001 on the green gate"
fg_assert_contains "commit ok" "$FG_OUT" "commit succeeded"
fg_run "$REPO" git push T0001
fg_assert_eq 0 "$FG_RC" "git push T0001 on the green gate"
FG_OUT="$(cd "$REPO" && PATH="$FG_SCRATCH/gh-bin:$PATH" "$FG_SCRATCH/bin/rddev" pr open T0001 2>&1)"
FG_RC=$?
fg_assert_eq 0 "$FG_RC" "pr open T0001 on the green gate"
fg_assert_contains "42" "$FG_OUT" "pr open recorded the PR number"
FG_OUT="$(cd "$REPO" && PATH="$FG_SCRATCH/gh-bin:$PATH" "$FG_SCRATCH/bin/rddev" pr merge T0001 2>&1)"
FG_RC=$?
fg_assert_eq 0 "$FG_RC" "pr merge T0001 on the green gate"
fg_assert_contains "pr-merge ok" "$FG_OUT" "pr merge succeeded"
fg_dump
fg_assert_contains "pr create" "$(cat "$FG_SCRATCH/gh-invocations.log")" "gh pr create was invoked"
fg_assert_contains "pr merge" "$(cat "$FG_SCRATCH/gh-invocations.log")" "gh pr merge was invoked"
fg_run "$REPO" task inspect T0001 --json
fg_assert_contains '"merged"' "$FG_OUT" "pr merge folded accepted -> merged"

# --- 8. the resume-from-disk surface ------------------------------------
fg_run "$REPO" workflow T0001
fg_assert_eq 0 "$FG_RC" "rddev workflow T0001 rebuilds the trail from disk"
fg_assert_contains "merged" "$FG_OUT" "workflow shows the terminal state"
fg_assert_contains "G2: passed" "$FG_OUT" "workflow lists the gate evidence"

# --- 9. tamper demo: gate inputs the Worker edits (T0012 security fix) ---
fg_run "$REPO" task ready T0002
fg_assert_eq 0 "$FG_RC" "task ready T0002"
fg_run "$REPO" worker spawn T0002 --claude-bin "$FG_SCRATCH/bin/claude-tamper"
fg_assert_eq 0 "$FG_RC" "worker spawn T0002 (tamper worker)"
fg_wait_exit "$REPO" T0002 30 || fg_fail "worker T0002 did not exit"
fg_run "$REPO" worker collect T0002
fg_assert_eq 1 "$FG_RC" "worker collect T0002 refuses the tamper"
fg_assert_contains "tamper" "$FG_OUT" "collect detects the gate-input tamper"
fg_assert_contains "apps/evil.txt" "$FG_OUT" "collect judged the diff by the AUTHORITATIVE scope (out-of-scope file named)"
fg_assert_contains "gate-inputs" "$FG_OUT" "the gate-inputs check failed"
fg_run "$REPO" task inspect T0002 --json
fg_assert_contains '"rejected"' "$FG_OUT" "T0002 is rejected, never verification"
fg_run "$REPO" task accept T0002
fg_assert_eq 1 "$FG_RC" "task accept T0002 is refused while rejected"
fg_assert_contains "illegal" "$FG_OUT" "the refusal names the illegal transition explicitly"

fg_finish
