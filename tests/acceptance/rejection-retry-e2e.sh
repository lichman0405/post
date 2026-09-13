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
#     worktree (the rejected attempt's leftover file is gone);
#   - a re-dispatched Review Worker never inherits the previous attempt's
#     verdict: an earlier RESULT.json is archived at spawn and cannot be
#     collected as the new run's judgement, and a verdict that predates the run
#     it is collected for is refused even when it is put back by hand.
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
  "task_count": 3,
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
     "allowed_scope": ["internal/config/**"]},
    {"id": "T0003", "phase": "P0", "phase_name": "e2e", "title": "re-reviewed task",
     "v1_required": false, "dependencies": [],
     "requirements": ["be reviewed again after the code moves"],
     "deliverables": ["internal/config/deliverable.txt"],
     "acceptance_criteria": ["every verdict collected belongs to the run that produced it"],
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
  "jobs": {"job-a": {"steps": [{"run": "echo a-ok"}]},
           "job-g3": {"steps": [{"run": "echo g3-ok"}]}},
  "review": {"required_for_merge": false},
  "task_overrides": {"T0001": {"g3_jobs": ["job-g3"]}}
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
    # A rework must NOT also name a session id: real claude (2.1.269) refuses
    # the whole invocation with "--session-id can only be used with --continue
    # or --resume if --fork-session is also specified", and every rework died
    # at startup until that was fixed. This assertion used to demand the
    # opposite — it encoded the bug — which went unnoticed because nothing ran
    # tests/acceptance/*.sh.
    if [ -n "$FG_SESSION_ID" ]; then echo "rework also passed --session-id" >&2; exit 9; fi
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
# Attempt 1 CREATES the session (--session-id), the rework RESUMES it
# (--resume) and must not name one — see the fake's rework branch above.
SESSIONS="$(cat "$REPO/.rddev/workers/T0001/session-ids.txt")"
FIRST="$(printf '%s\n' "$SESSIONS" | sed -n 1p)"
SECOND="$(printf '%s\n' "$SESSIONS" | sed -n 2p)"
[ -n "$FIRST" ] && fg_ok "attempt 1 named its session: $FIRST" || fg_fail "attempt 1 passed no --session-id"
[ -z "$SECOND" ] && fg_ok "the rework passed no --session-id (resume carries the identity)" \
  || fg_fail "the rework also passed --session-id ($SECOND)"
RESUMED="$(cat "$REPO/.rddev/workers/T0001/resumed-ids.txt" 2>/dev/null | sed -n 2p)"
fg_assert_eq "$FIRST" "$RESUMED" "rework resumed the SAME claude session"
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

# --- T0003: a re-dispatched review never inherits the old verdict ---------
# A verdict describes the code of ONE attempt. The Reviewer may leave
# RESULT.json unwritten (its verdict is also carried by the session log), and
# collect prefers the file — so a RESULT.json surviving from an earlier attempt
# would be recorded as the NEW attempt's judgement: an approve outliving the
# rework that invalidated it, with nobody having looked at the current code.
# Both ways such a file can be collected are pinned here.
cat > "$FG_SCRATCH/review-approve.sh" <<'EOF'
#!/usr/bin/env bash
set -u
python3 - <<'PY'
import json, os
rdir = os.environ["POST_WORKER_RESULT_DIR"]
task = os.environ["POST_WORKER_TASK_ID"]
task = task[:-len("-review")] if task.endswith("-review") else task
json.dump({"task_id": task, "verdict": "approve",
           "summary": "reads correctly", "findings": [], "risks": []},
          open(os.path.join(rdir, "RESULT.json"), "w"))
PY
EOF
chmod +x "$FG_SCRATCH/review-approve.sh"
fg_fake_claude "$FG_SCRATCH/bin/claude-approve" "$FG_SCRATCH/review-approve.sh"

# A Reviewer that completes the session and writes no verdict at all.
printf '#!/usr/bin/env bash\nset -u\nprintf "no verdict written\\n"\n' > "$FG_SCRATCH/review-silent.sh"
chmod +x "$FG_SCRATCH/review-silent.sh"
fg_fake_claude "$FG_SCRATCH/bin/claude-silent" "$FG_SCRATCH/review-silent.sh"

# The earlier attempt's verdict, put back exactly as it was — mtime included.
cat > "$FG_SCRATCH/review-replay.sh" <<'EOF'
#!/usr/bin/env bash
set -u
rdir="$POST_WORKER_RESULT_DIR"
for f in "$rdir"/RESULT.superseded-*.json; do
  [ -e "$f" ] || continue
  cp -p "$f" "$rdir/RESULT.json"
  break
done
EOF
chmod +x "$FG_SCRATCH/review-replay.sh"
fg_fake_claude "$FG_SCRATCH/bin/claude-replay" "$FG_SCRATCH/review-replay.sh"

REVIEW_DIR="$REPO/.rddev/workers/T0003-review"

fg_run "$REPO" task ready T0003
fg_assert_eq 0 "$FG_RC" "task ready T0003"
fg_run "$REPO" worker spawn T0003 --claude-bin "$FG_SCRATCH/bin/claude"
fg_assert_eq 0 "$FG_RC" "worker spawn T0003"
fg_wait_exit "$REPO" T0003 30 || fg_fail "worker T0003 did not exit"
fg_run "$REPO" worker collect T0003
fg_assert_eq 0 "$FG_RC" "worker collect T0003"
fg_run "$REPO" task inspect T0003 --json
fg_assert_contains '"verification"' "$FG_OUT" "T0003 is in verification"

fg_run "$REPO" review spawn T0003 --claude-bin "$FG_SCRATCH/bin/claude-approve"
fg_assert_eq 0 "$FG_RC" "review spawn T0003 (attempt 1)"
fg_wait_exit "$REPO" T0003-review 30 || fg_fail "review T0003 attempt 1 did not exit"
fg_run "$REPO" review collect T0003
fg_assert_eq 0 "$FG_RC" "review collect T0003 (attempt 1)"
fg_assert_contains "approve" "$FG_OUT" "attempt 1's verdict is approve"

# The code moves: exactly the rework a second review exists for.
echo "more" >> "$REPO/.rddev/worktrees/T0003/internal/config/deliverable.txt"

fg_run "$REPO" review spawn T0003 --claude-bin "$FG_SCRATCH/bin/claude-silent"
fg_assert_eq 0 "$FG_RC" "review spawn T0003 (attempt 2, writing no verdict)"
fg_wait_exit "$REPO" T0003-review 30 || fg_fail "review T0003 attempt 2 did not exit"
if ls "$REVIEW_DIR"/RESULT.superseded-*.json >/dev/null 2>&1; then
  fg_ok "the previous verdict was archived under the attempt it belongs to"
else
  fg_fail "the previous attempt's verdict was not archived"
fi
if [ -e "$REVIEW_DIR/RESULT.json" ]; then
  fg_fail "attempt 1's verdict is still in place when attempt 2 is collected"
else
  fg_ok "the new attempt starts with no verdict of its own"
fi
fg_run "$REPO" review collect T0003
fg_assert_eq 1 "$FG_RC" "collect refuses a run that produced no verdict"
fg_assert_not_contains "review ok — approve" "$FG_OUT" "an earlier attempt's approve is NOT recorded as this run's verdict"

# ... and a verdict that survives anyway (here: put back with its original
# mtime) is refused for the reason that makes the archive a belt rather than
# the only brace: it belongs to a run that has ended.
fg_run "$REPO" review spawn T0003 --claude-bin "$FG_SCRATCH/bin/claude-replay"
fg_assert_eq 0 "$FG_RC" "review spawn T0003 (attempt 3, the old verdict put back)"
fg_wait_exit "$REPO" T0003-review 30 || fg_fail "review T0003 attempt 3 did not exit"
fg_run "$REPO" review collect T0003
fg_assert_eq 1 "$FG_RC" "collect refuses a verdict that predates its own run"
fg_assert_contains "review-verdict-run" "$FG_OUT" "the refusal names the run the verdict does not belong to"

# The refusals are not a dead end: a review of the current code is collected.
fg_run "$REPO" review spawn T0003 --claude-bin "$FG_SCRATCH/bin/claude-approve"
fg_assert_eq 0 "$FG_RC" "review spawn T0003 (attempt 4)"
fg_wait_exit "$REPO" T0003-review 30 || fg_fail "review T0003 attempt 4 did not exit"
fg_run "$REPO" review collect T0003
fg_assert_eq 0 "$FG_RC" "review collect T0003 (attempt 4)"
fg_assert_contains "approve" "$FG_OUT" "the re-dispatched review approved the current code"

fg_finish
