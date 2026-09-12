#!/usr/bin/env bash
# Shared scaffolding for the T0012 four-gate e2e scripts
# (tests/acceptance/four-gate-e2e.sh, rejection-retry-e2e.sh,
# supervisor-git-e2e.sh). This file is not a test on its own.
#
# Runnable on a host with bash + coreutils + git + go + python3.
set -u
export LC_ALL=C

FG_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
FG_SCRATCH="$(mktemp -d)"
trap 'rm -rf "$FG_SCRATCH"' EXIT
export FG_SCRATCH # the fake claude workers read $FG_SCRATCH/good-result.py

FG_FAILS=0
fg_fail() { printf 'FAIL %s\n' "$*"; FG_FAILS=$((FG_FAILS+1)); }
fg_ok()   { printf 'ok   %s\n' "$*"; }

# fg_assert_eq WANT GOT LABEL — plain string equality with ok/fail output.
fg_assert_eq() {
  if [ "$1" = "$2" ]; then fg_ok "$3"; else fg_fail "$3: want [$1], got [$2]"; fi
}

# fg_assert_contains NEEDLE HAYSTACK LABEL
fg_assert_contains() {
  case "$2" in
    *"$1"*) fg_ok "$3" ;;
    *) fg_fail "$3: output does not contain [$1]: $2" ;;
  esac
}

# fg_assert_not_contains NEEDLE HAYSTACK LABEL
fg_assert_not_contains() {
  case "$2" in
    *"$1"*) fg_fail "$3: output contains forbidden [$1]: $2" ;;
    *) fg_ok "$3" ;;
  esac
}

fg_build() { # fg_build WORK — build rddev into $WORK/bin/rddev
  mkdir -p "$1/bin"
  if ! (cd "$FG_ROOT" && go build -o "$1/bin/rddev" ./cmd/rddev); then
    fg_fail "building rddev"
    return 1
  fi
  fg_ok "rddev built"
}

# fg_fake_claude PATH BODY_FILE — write a fake claude script. The wrapper
# handles --version, records the --session-id it is dispatched with
# (session-ids.txt in the result dir, one line per dispatch — rework must
# append the SAME id, respawn a DIFFERENT one), and then runs BODY_FILE with
# env POST_WORKER_TASK_ID / POST_WORKER_WORKTREE / POST_WORKER_RESULT_DIR set.
fg_fake_claude() {
  local path="$1" body="$2"
  cat > "$path" <<'EOF'
#!/usr/bin/env bash
set -u
if [ "${1:-}" = "--version" ]; then echo "2.1.0 (fake e2e)"; exit 0; fi
# The full command line must be captured BEFORE the parsing loop consumes the
# positional parameters — `case "$*"` after the loop always sees an empty
# string, so the prompt check would never match (caught by the rework e2e).
ALL_ARGS="$*"
SESSION_ID=""
RESUME_ID=""
while [ $# -gt 0 ]; do
  case "$1" in
    --session-id) shift; SESSION_ID="${1:-}";;
    --resume) shift; RESUME_ID="${1:-}";;
  esac
  shift
done
printf '%s\n' "$SESSION_ID" >> "$POST_WORKER_RESULT_DIR/session-ids.txt"
HAS_REJ=0
case "$ALL_ARGS" in *REJECTED*) HAS_REJ=1;; esac
export FG_SESSION_ID="$SESSION_ID" FG_RESUME_ID="$RESUME_ID" FG_PROMPT_HAS_REJECTION="$HAS_REJ"
EOF
  cat >> "$path" <<EOF
exec bash "$body"
EOF
  chmod +x "$path"
}

# fg_setup_repo WORK TASKIDS_JSON GATES_JSON — create a scratch git repo at
# $WORK/repo with the orchestrator spec files copied in, a task DAG and state
# file for the given tasks, and the given gates spec. Prints the repo path.
fg_setup_repo() {
  local work="$1" tasks_json="$2" gates_json="$3"
  local repo="$work/repo"
  mkdir -p "$repo/specs/orchestrator" "$repo/tasks" "$repo/internal/config"
  cp "$FG_ROOT/specs/orchestrator/task-package.schema.json" "$repo/specs/orchestrator/"
  cp "$FG_ROOT/specs/orchestrator/worker-result.schema.json" "$repo/specs/orchestrator/"
  cp "$FG_ROOT/specs/orchestrator/derived-artifacts.json" "$repo/specs/orchestrator/"
  cp "$FG_ROOT/specs/orchestrator/review-verdict.schema.json" "$repo/specs/orchestrator/"
  printf '%s\n' "$tasks_json" > "$repo/tasks/tasks.json"
  # State file: todo for every task, in the real file's shape.
  TASKS_JSON="$tasks_json" python3 - "$repo/tasks/task_status.json" <<'PY'
import json, os, sys
tasks = json.loads(os.environ["TASKS_JSON"])
entries = {t["id"]: {"status": "todo", "started_at": None, "completed_at": None,
                     "notes": "", "worker_run_id": None,
                     "accepted_by_supervisor_at": None, "merged_at": None}
           for t in tasks["tasks"]}
json.dump({"version": 1, "overall": "P0_in_progress", "tasks": entries},
          open(sys.argv[1], "w"), indent=1)
PY
  printf '%s\n' "$gates_json" > "$repo/specs/orchestrator/gates.json"
  echo "package config" > "$repo/internal/config/.keep"
  (
    cd "$repo" &&
    git init -q -b main &&
    git -c user.name=e2e -c user.email=e2e@test add -A &&
    git -c user.name=e2e -c user.email=e2e@test commit -q -m "scratch base"
  ) || { fg_fail "initializing the scratch repo"; return 1; }
  echo "$repo"
}

# fg_wait_exit REPO TASKID [TIMEOUT_SECS] — wait for the worker's exit.status.
fg_wait_exit() {
  local repo="$1" task="$2" tmo="${3:-30}" n=0
  while [ $n -lt $((tmo*2)) ]; do
    [ -f "$repo/.rddev/workers/$task/exit.status" ] && return 0
    sleep 0.5
    n=$((n+1))
  done
  return 1
}

# fg_run REPO ARGS... — run rddev with cwd=REPO; captures combined output into
# FG_OUT and the exit code into FG_RC. A previous unread run is appended to
# the transcript (never lost) before being replaced.
fg_run() {
  local repo="$1"; shift
  if [ "${FG_OUT+x}" = x ]; then
    { printf '\n==== rddev run (previous, exit %s) ====\n' "$FG_RC"; printf '%s\n' "$FG_OUT"; } >> "$FG_SCRATCH/transcript.log"
    unset FG_OUT FG_RC
  fi
  FG_OUT="$(cd "$repo" && "$FG_SCRATCH/bin/rddev" "$@" 2>&1)"
  FG_RC=$?
}

# fg_dump — flush the current run to the transcript and clear FG_OUT/FG_RC.
fg_dump() {
  if [ "${FG_OUT+x}" = x ]; then
    { printf '\n==== rddev run (exit %s) ====\n' "$FG_RC"; printf '%s\n' "$FG_OUT"; } >> "$FG_SCRATCH/transcript.log"
    unset FG_OUT FG_RC
  fi
}

fg_finish() { # fg_finish — print the verdict and exit
  fg_dump
  if [ $FG_FAILS -eq 0 ]; then
    printf '\nall e2e checks passed\n'
    exit 0
  fi
  printf '\n%d e2e check(s) FAILED — transcript: %s\n' "$FG_FAILS" "$FG_SCRATCH/transcript.log"
  exit 1
}

# fg_good_result_py — write the RESULT.json generator. It reads the task
# package from the Worker's result dir and produces a completed RESULT whose
# tests/acceptance entries mirror the package's required_tests and
# acceptance_criteria (so the G1 consistency checks see a fully covered,
# honestly passed document).
fg_good_result_py() {
  cat > "$FG_SCRATCH/good-result.py" <<'PY'
import json, os, sys
rdir = os.environ["POST_WORKER_RESULT_DIR"]
pkg = json.load(open(os.path.join(rdir, "task-package.json")))
files = json.loads(sys.argv[1])
doc = {
    "task_id": os.environ["POST_WORKER_TASK_ID"],
    "status": "completed",
    "summary": "e2e fake worker deliverable",
    "files_changed": files,
    "tests": [{"command": t, "status": "passed",
               "evidence": "e2e fake worker ran it: exit 0"} for t in pkg["required_tests"]],
    "acceptance": [{"criterion": c, "status": "passed",
                    "evidence": "e2e fake worker delivered it"} for c in pkg["acceptance_criteria"]],
    "risks": [], "follow_up_issues": [],
    "notes_for_supervisor": "written by the fake e2e claude",
}
json.dump(doc, open(os.path.join(rdir, "RESULT.json"), "w"))
PY
}

# fg_good_worker_bin — write the fake claude for a plain good worker: it
# creates the deliverable file inside the worktree's allowed scope and writes
# an honestly completed RESULT.json.
fg_good_worker_bin() {
  fg_good_result_py
  cat > "$FG_SCRATCH/worker-body.sh" <<'EOF'
#!/usr/bin/env bash
set -u
mkdir -p "$POST_WORKER_WORKTREE/internal/config"
echo "deliverable from $POST_WORKER_TASK_ID" > "$POST_WORKER_WORKTREE/internal/config/deliverable.txt"
python3 "$FG_SCRATCH/good-result.py" '["internal/config/deliverable.txt"]'
EOF
  chmod +x "$FG_SCRATCH/worker-body.sh"
  fg_fake_claude "$1" "$FG_SCRATCH/worker-body.sh"
}

# fg_review_worker_bin — write the fake claude for a Review Worker: an approve
# verdict against the review schema, addressed to the reviewed task.
fg_review_worker_bin() {
  cat > "$FG_SCRATCH/review-body.sh" <<'EOF'
#!/usr/bin/env bash
set -u
TASK="${POST_WORKER_TASK_ID%-review}"
python3 - "$TASK" <<'PY'
import json, os, sys
task = sys.argv[1]
doc = {"task_id": task, "verdict": "approve",
       "summary": "e2e fake review: diff inspected, tests and scope evidence check out",
       "findings": [], "risks": []}
json.dump(doc, open(os.path.join(os.environ["POST_WORKER_RESULT_DIR"], "RESULT.json"), "w"))
PY
EOF
  chmod +x "$FG_SCRATCH/review-body.sh"
  fg_fake_claude "$1" "$FG_SCRATCH/review-body.sh"
}
