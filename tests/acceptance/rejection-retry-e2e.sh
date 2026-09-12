#!/usr/bin/env bash
# T0012-TEST-02 — rejection/retry e2e (tests/acceptance/rejection-retry-e2e.sh).
#
# Proves, mechanically:
#   - a self-contradictory RESULT (status completed on a document whose first
#     line says "INTERIM SNAPSHOT" and whose required test is not_run) is
#     REFUSED at collect — the shipped defect can never pass again;
#   - a failed Worker never reaches verification, let alone accepted;
#   - reject -> worker rework re-dispatches the SAME claude session (resume,
#     same session id, rejection reasons carried in the prompt) and the second
#     attempt is collected and accepted;
#   - reject -> worker respawn starts a NEW session and resets the rejected
#     worktree (the rejected attempt's leftover file is gone).
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
    {"id": "T0001", "phase": "P0", "phase_name": "e2e", "title": "retry task one",
     "v1_required": false, "dependencies": [],
     "requirements": ["rework after rejection"],
     "deliverables": ["internal/config/deliverable.txt"],
     "acceptance_criteria": ["second attempt delivers inside the scope"],
     "tests": ["e2e-check.sh"],
     "allowed_scope": ["internal/config/**"]},
    {"id": "T0002", "phase": "P0", "phase_name": "e2e", "title": "retry task two",
     "v1_required": false, "dependencies": [],
     "requirements": ["respawn after rejection"],
     "deliverables": ["internal/config/deliverable.txt"],
     "acceptance_criteria": ["respawn starts from a clean worktree"],
     "tests": ["e2e-check.sh"],
     "allowed_scope": ["internal/config/**"]}
  ]
}'
GATES='{
  "version": 1,
  "required_jobs": ["job-a"],
  "gates": {
    "G1": {"name": "worker", "description": "collect", "runs_jobs": [], "asserts_jobs": []},
    "G2": {"name": "accept", "description": "CI steps", "runs_jobs": ["job-a"], "asserts_jobs": []},
    "G3": {"name": "e2e", "description": "per task", "runs_jobs": [], "asserts_jobs": []},
    "G4": {"name": "merge", "description": "assert", "runs_jobs": [], "asserts_jobs": ["job-a"]}
  },
  "jobs": {"job-a": {"steps": [{"run": "echo a-ok"}]}},
  "review": {"required_for_merge": false},
  "task_overrides": {}
}'

REPO="$(fg_setup_repo "$FG_SCRATCH" "$TASKS" "$GATES")" || exit 1

# The retry fake claude: attempt number = lines already in session-ids.txt
# (the wrapper appends before exec'ing this body). Attempt 1 produces the
# failure shape of the task; attempt >= 2 is the good worker — and for a
# rework it also verifies the SAME session was resumed with the rejection
# reasons in the prompt; for a respawn it verifies the rejected diff is gone.
fg_good_result_py
cat > "$FG_SCRATCH/retry-body.sh" <<'EOF'
#!/usr/bin/env bash
set -u
ATTEMPT=$(wc -l < "$POST_WORKER_RESULT_DIR/session-ids.txt")
TASK="$POST_WORKER_TASK_ID"
case "$TASK" in
  T0001)
    if [ "$ATTEMPT" -eq 1 ]; then
      # The shipped defect, planted verbatim: completed on an INTERIM-marked
      # document with the required test not_run.
      python3 - <<'PY'
import json, os
doc = {"task_id": "T0001", "status": "completed",
       "summary": "INTERIM SNAPSHOT — work in progress, not a deliverable",
       "files_changed": [], "tests": [{"command": "e2e-check.sh", "status": "not_run", "evidence": "none"}],
       "acceptance": [], "risks": [], "follow_up_issues": [],
       "notes_for_supervisor": "planted by the e2e"}
json.dump(doc, open(os.path.join(os.environ["POST_WORKER_RESULT_DIR"], "RESULT.json"), "w"))
PY
      exit 0
    fi
    # Attempt 2 = rework: the SAME session must have been resumed with the
    # recorded rejection reasons in the prompt.
    if [ -z "$FG_RESUME_ID" ]; then echo "rework ran without --resume" >&2; exit 9; fi
    if [ "$FG_RESUME_ID" != "$FG_SESSION_ID" ]; then echo "resume id != session id" >&2; exit 9; fi
    if [ "$FG_PROMPT_HAS_REJECTION" != "1" ]; then echo "rework prompt lacks the rejection reasons" >&2; exit 9; fi
    ;;
  T0002)
    if [ "$ATTEMPT" -eq 1 ]; then
      # Honest non-completion + a leftover file: the rejected attempt's diff
      # that a respawn must discard.
      echo "rejected leftover" > "$POST_WORKER_WORKTREE/internal/config/leftover.txt"
      python3 - <<'PY'
import json, os
doc = {"task_id": "T0002", "status": "failed",
       "summary": "blocked on the fake e2e environment",
       "files_changed": ["internal/config/leftover.txt"],
       "tests": [{"command": "e2e-check.sh", "status": "passed", "evidence": "n/a"}],
       "acceptance": [], "risks": [], "follow_up_issues": [],
       "notes_for_supervisor": "honest failure"}
json.dump(doc, open(os.path.join(os.environ["POST_WORKER_RESULT_DIR"], "RESULT.json"), "w"))
PY
      exit 0
    fi
    # Attempt 2 = respawn: fresh session, rejected diff reset away.
    if [ -n "$FG_RESUME_ID" ]; then echo "respawn carried --resume" >&2; exit 9; fi
    if [ -e "$POST_WORKER_WORKTREE/internal/config/leftover.txt" ]; then
      echo "respawn did not reset the rejected worktree" >&2; exit 9
    fi
    ;;
esac
mkdir -p "$POST_WORKER_WORKTREE/internal/config"
echo "deliverable from attempt $ATTEMPT" > "$POST_WORKER_WORKTREE/internal/config/deliverable.txt"
python3 "$FG_SCRATCH/good-result.py" '["internal/config/deliverable.txt"]'
EOF
chmod +x "$FG_SCRATCH/retry-body.sh"
fg_fake_claude "$FG_SCRATCH/bin/claude" "$FG_SCRATCH/retry-body.sh"

# --- T0001: contradictory RESULT -> rejected -> rework (same session) -----
fg_run "$REPO" task ready T0001
fg_assert_eq 0 "$FG_RC" "task ready T0001"
fg_run "$REPO" worker spawn T0001 --claude-bin "$FG_SCRATCH/bin/claude"
fg_assert_eq 0 "$FG_RC" "worker spawn T0001 (attempt 1)"
fg_wait_exit "$REPO" T0001 30 || fg_fail "worker T0001 attempt 1 did not exit"
fg_run "$REPO" worker collect T0001
fg_assert_eq 1 "$FG_RC" "collect refuses the contradictory RESULT (exit 1)"
fg_assert_contains "result-marker" "$FG_OUT" "the INTERIM marker check failed"
fg_assert_contains "not_run" "$FG_OUT" "the not_run test is named in the refusal"
fg_run "$REPO" task inspect T0001 --json
fg_assert_contains '"rejected"' "$FG_OUT" "the failed Worker is rejected, never verification"
fg_run "$REPO" task accept T0001
fg_assert_eq 1 "$FG_RC" "accept is refused for the rejected task"
fg_assert_contains "illegal" "$FG_OUT" "the refusal names the illegal transition"

fg_run "$REPO" worker rework T0001 --claude-bin "$FG_SCRATCH/bin/claude"
fg_assert_eq 0 "$FG_RC" "worker rework T0001 (attempt 2)"
fg_wait_exit "$REPO" T0001 30 || fg_fail "rework attempt did not exit"
fg_run "$REPO" worker collect T0001
fg_assert_eq 0 "$FG_RC" "collect accepts the reworked attempt"
fg_assert_contains "collect ok" "$FG_OUT" "the reworked run is clean"
SESSIONS="$(cat "$REPO/.rddev/workers/T0001/session-ids.txt")"
fg_assert_eq "$(printf '%s\n' "$SESSIONS" | sed -n 1p)" "$(printf '%s\n' "$SESSIONS" | sed -n 2p)" \
  "rework resumed the SAME claude session"
fg_run "$REPO" task accept T0001
fg_assert_eq 0 "$FG_RC" "task accept T0001 after rework"
fg_run "$REPO" task inspect T0001 --json
fg_assert_contains '"accepted"' "$FG_OUT" "the reworked task is accepted"

# --- T0002: honest failure -> rejected -> respawn (new session, clean tree)
fg_run "$REPO" task ready T0002
fg_assert_eq 0 "$FG_RC" "task ready T0002"
fg_run "$REPO" worker spawn T0002 --claude-bin "$FG_SCRATCH/bin/claude"
fg_assert_eq 0 "$FG_RC" "worker spawn T0002 (attempt 1)"
fg_wait_exit "$REPO" T0002 30 || fg_fail "worker T0002 attempt 1 did not exit"
fg_run "$REPO" worker collect T0002
fg_assert_eq 1 "$FG_RC" "collect refuses the honest non-completion"
fg_assert_contains "non-completion" "$FG_OUT" "the refusal names the honest failed status"
fg_run "$REPO" task inspect T0002 --json
fg_assert_contains '"rejected"' "$FG_OUT" "T0002 is rejected"

fg_run "$REPO" worker respawn T0002 --claude-bin "$FG_SCRATCH/bin/claude"
fg_assert_eq 0 "$FG_RC" "worker respawn T0002 (attempt 2)"
fg_wait_exit "$REPO" T0002 30 || fg_fail "respawn attempt did not exit"
fg_run "$REPO" worker collect T0002
fg_assert_eq 0 "$FG_RC" "collect accepts the respawned attempt"
SESSIONS="$(cat "$REPO/.rddev/workers/T0002/session-ids.txt")"
S1="$(printf '%s\n' "$SESSIONS" | sed -n 1p)"
S2="$(printf '%s\n' "$SESSIONS" | sed -n 2p)"
[ "$S1" != "$S2" ] && fg_ok "respawn started a NEW claude session" || fg_fail "respawn reused the rejected session ($S1)"
[ ! -e "$REPO/.rddev/worktrees/T0002/internal/config/leftover.txt" ] && fg_ok "respawn reset the rejected worktree (leftover gone)" || fg_fail "rejected leftover still present after respawn"
fg_run "$REPO" task accept T0002
fg_assert_eq 0 "$FG_RC" "task accept T0002 after respawn"

fg_finish
