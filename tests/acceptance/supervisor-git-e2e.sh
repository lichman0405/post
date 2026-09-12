#!/usr/bin/env bash
# T0012-TEST-03 — supervisor git control e2e (tests/acceptance/supervisor-git-e2e.sh).
#
# Proves the Supervisor-only Git control plane end to end:
#   - with a red gate, rddev git commit/push and rddev pr open/merge/status
#     refuse BEFORE git/gh is invoked (the fake recorder stays empty and the
#     refusal enumerates the merge-gate reasons);
#   - with all four gates green the same commands really run: the commit lands
#     on the task branch, the push reaches the bare origin, gh is invoked for
#     pr create/merge, and every executed action writes its GitRecord evidence.
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
  "task_count": 1,
  "phases": [{"id": "P0", "name": "e2e"}],
  "tasks": [
    {"id": "T0001", "phase": "P0", "phase_name": "e2e", "title": "git control task",
     "v1_required": false, "dependencies": [],
     "requirements": ["deliver a committable change"],
     "deliverables": ["internal/config/deliverable.txt"],
     "acceptance_criteria": ["the change reaches main through the gates"],
     "tests": ["e2e-check.sh"],
     "allowed_scope": ["internal/config/**"]}
  ]
}'
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
    "job-b": {"steps": [{"run": "echo b-ok"}]}
  },
  "review": {"required_for_merge": false},
  "task_overrides": {}
}'

REPO="$(fg_setup_repo "$FG_SCRATCH" "$TASKS" "$GATES")" || exit 1

mkdir -p "$FG_SCRATCH/refuse-bin"
for n in git gh; do
  printf '#!/bin/sh\necho "$0 $*" >> "%s"\nexit 0\n' "$FG_SCRATCH/fake-invocations.log" > "$FG_SCRATCH/refuse-bin/$n"
  chmod +x "$FG_SCRATCH/refuse-bin/$n"
done
: > "$FG_SCRATCH/fake-invocations.log"

mkdir -p "$FG_SCRATCH/gh-bin"
cat > "$FG_SCRATCH/gh-bin/gh" <<EOF
#!/bin/bash
echo "gh \$*" >> "$FG_SCRATCH/gh-invocations.log"
case "\$1 \$2" in
  "pr view") exit 1;;
  "pr create") echo "7";;
  "pr merge") echo "1111111111111111111111111111111111111111";;
  *) echo "unexpected gh call: \$*" >&2; exit 9;;
esac
EOF
chmod +x "$FG_SCRATCH/gh-bin/gh"
: > "$FG_SCRATCH/gh-invocations.log"

fg_good_worker_bin "$FG_SCRATCH/bin/claude"

git init -q --bare "$FG_SCRATCH/origin.git"
(cd "$REPO" && git remote add origin "$FG_SCRATCH/origin.git")

# --- 1. red gate: every control-plane action refuses, nothing is invoked ---
fg_run "$REPO" task ready T0001
fg_assert_eq 0 "$FG_RC" "task ready T0001"
for action in "git commit" "git push" "pr open" "pr merge"; do
  fg_dump
  FG_OUT="$(cd "$REPO" && PATH="$FG_SCRATCH/refuse-bin:$PATH" "$FG_SCRATCH/bin/rddev" $action T0001 2>&1)"
  FG_RC=$?
  fg_assert_eq 1 "$FG_RC" "$action refused on the red gate"
  fg_assert_contains "REFUSED" "$FG_OUT" "$action prints REFUSED"
done
fg_dump
FG_OUT="$(cd "$REPO" && PATH="$FG_SCRATCH/refuse-bin:$PATH" "$FG_SCRATCH/bin/rddev" pr status T0001 2>&1)"
FG_RC=$?
fg_assert_eq 1 "$FG_RC" "pr status refuses on the red gate"
fg_assert_contains "no G2 gate-run record exists" "$FG_OUT" "the refusal enumerates the missing G2 reason"
fg_dump
fg_assert_eq 0 "$(wc -c < "$FG_SCRATCH/fake-invocations.log")" "git/gh was never invoked while the gate was red"

# --- 2. green gates: the same commands really run -------------------------
fg_run "$REPO" worker spawn T0001 --claude-bin "$FG_SCRATCH/bin/claude"
fg_assert_eq 0 "$FG_RC" "worker spawn T0001"
fg_wait_exit "$REPO" T0001 30 || fg_fail "worker did not exit"
fg_run "$REPO" worker collect T0001
fg_assert_eq 0 "$FG_RC" "worker collect T0001"
fg_run "$REPO" gate run G2 T0001
fg_assert_eq 0 "$FG_RC" "gate run G2 T0001"
fg_run "$REPO" task accept T0001
fg_assert_eq 0 "$FG_RC" "task accept T0001"
FG_OUT="$(cd "$REPO" && PATH="$FG_SCRATCH/gh-bin:$PATH" "$FG_SCRATCH/bin/rddev" pr status T0001 2>&1)"
FG_RC=$?
fg_assert_eq 0 "$FG_RC" "pr status passes on the green gate"
fg_assert_contains "passed" "$FG_OUT" "pr status shows the green G4 assertion"

fg_run "$REPO" git commit T0001
fg_assert_eq 0 "$FG_RC" "git commit T0001"
SHA="$(cd "$REPO/.rddev/worktrees/T0001" && git rev-parse HEAD)"
fg_assert_eq "$SHA" "$(cd "$REPO" && git rev-parse "refs/heads/task/T0001-git-control-task")" \
  "the commit landed on the task branch"
fg_run "$REPO" git push T0001
fg_assert_eq 0 "$FG_RC" "git push T0001"
fg_assert_eq "$SHA" "$(cd "$REPO" && git rev-parse refs/heads/task/T0001-git-control-task)" \
  "the push reached the bare origin (branch still at the commit)"
FG_OUT="$(cd "$REPO" && PATH="$FG_SCRATCH/gh-bin:$PATH" "$FG_SCRATCH/bin/rddev" pr open T0001 2>&1)"
FG_RC=$?
fg_assert_eq 0 "$FG_RC" "pr open T0001"
fg_assert_contains "7" "$FG_OUT" "pr open returned the PR number"
FG_OUT="$(cd "$REPO" && PATH="$FG_SCRATCH/gh-bin:$PATH" "$FG_SCRATCH/bin/rddev" pr merge T0001 2>&1)"
FG_RC=$?
fg_assert_eq 0 "$FG_RC" "pr merge T0001"
fg_dump
for want in "pr create" "pr merge"; do
  fg_assert_contains "$want" "$(cat "$FG_SCRATCH/gh-invocations.log")" "gh $want was invoked on the green gate"
done
fg_run "$REPO" task inspect T0001 --json
fg_assert_contains '"merged"' "$FG_OUT" "pr merge folded accepted -> merged"
fg_run "$REPO" gate records T0001
fg_assert_contains "git-commit" "$FG_OUT" "commit wrote its GitRecord evidence"
fg_assert_contains "git-pr-merge" "$FG_OUT" "merge wrote its GitRecord evidence"

fg_finish
