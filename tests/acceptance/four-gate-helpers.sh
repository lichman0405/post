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

# fg_assert_eq WANT GOT LABEL — plain string equality with ok/fail output. A
# failure also prints the rddev run it is about ($FG_OUT, set by fg_run): a bare
# "want [0], got [1]" hides the command's own message, and that message is the
# only thing that says *why* a pipeline step returned non-zero. It is what made
# issue #133 unanswerable for a week — the acceptance log recorded the code and
# not one word of the refusal behind it.
fg_assert_eq() {
  if [ "$1" = "$2" ]; then fg_ok "$3"; else
    if [ "${FG_OUT+x}" = x ] && [ -n "$FG_OUT" ]; then
      printf '\n==== rddev output (exit %s) ====\n%s\n' "${FG_RC:-unset}" "$FG_OUT"
      unset FG_OUT
    fi
    fg_fail "$3: want [$1], got [$2]"
  fi
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
# append the SAME id, respawn a DIFFERENT one), waits until the spawn has
# finished recording THIS run's process identity (see the wait below — a fake
# that exits first is refused as an unverifiable Worker), and then runs
# BODY_FILE with env POST_WORKER_TASK_ID / POST_WORKER_WORKTREE /
# POST_WORKER_RESULT_DIR set.
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
printf '%s\n' "$RESUME_ID" >> "$POST_WORKER_RESULT_DIR/resumed-ids.txt"
HAS_REJ=0
case "$ALL_ARGS" in *REJECTED*) HAS_REJ=1;; esac
export FG_SESSION_ID="$SESSION_ID" FG_RESUME_ID="$RESUME_ID" FG_PROMPT_HAS_REJECTION="$HAS_REJ"
# A Worker is a live process to rddev: spawn reads its /proc entry (environment,
# start time) and records its pid the moment it has started it. A fake that
# finishes and exits before those reads lands loses that race, and rddev then
# refuses the spawn ("reading /proc/<pid>/environ: no such file or directory:
# spawn aborted") — a red gate that says nothing about the behaviour under
# test. Observed exactly that in CI (PR #196's acceptance job): the review
# spawn at attempt 4 lost a race a ~2s stall had opened. Real claude is a
# seconds-long process; the fake has to be at least as observable as the thing
# it stands in for. So it waits for spawn's own last step to appear — the
# authoritative gate inputs gain this run's non-zero pid only after the
# post-spawn assertions have passed. (The previous attempt's pid is gone from
# that file by now: spawn rewrites it before it starts anything.)
gi="$POST_REPO_ROOT/.rddev/runtime/tasks/$POST_WORKER_TASK_ID/gate-inputs.json"
tries=0
while [ "$tries" -lt 600 ]; do
  if [ -f "$gi" ] && grep -qE '"pid": [1-9][0-9]*' "$gi" 2>/dev/null; then break; fi
  sleep 0.05
  tries=$((tries+1))
done
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
    # The identity belongs to the REPO, not to these commands. The code under
    # test shells out a plain `git commit` (CommitTask), which reads the repo
    # config — a per-command -c override never reaches it, so on a runner with
    # no global identity the commit died with "empty ident name ... not
    # allowed" while passing on a developer machine that happens to have one.
    # Same defect, and the same fix, as T0012's TestGitControlCommitOnGreenGate.
    git config user.name e2e &&
    git config user.email e2e@test &&
    git -c user.name=e2e -c user.email=e2e@test add -A &&
    git -c user.name=e2e -c user.email=e2e@test commit -q -m "scratch base"
  ) || { fg_fail "initializing the scratch repo"; return 1; }
  echo "$repo"
}

# fg_record_query REPO TASK [RUNID] — one field of a task's registry record, as
# `rddev worker list --json` reports it (status, run_id, exit_status). With
# RUNID it prints that run's exit_status, "-" when the run is not recorded yet;
# without it, the current run_id. Prints nothing and returns 1 when the task has
# no record at all.
fg_record_query() {
  local repo="$1" task="$2" run="${3:-}" raw rc
  raw="$(cd "$repo" && "$FG_SCRATCH/bin/rddev" worker list --json 2>/dev/null)"; rc=$?
  if [ $rc -ne 0 ]; then
    fg_record_query_note "rddev worker list --json exited $rc"
    return 1
  fi
  printf '%s' "$raw" | python3 -c '
import json, sys
task, want = sys.argv[1], sys.argv[2]
try:
    doc = json.load(sys.stdin)
except Exception:
    sys.exit(2)
# The CLI wraps the array: {"workers": [...]}. Anything else is a shape this
# instrument does not understand, and an instrument that guesses here is worse
# than one that says it cannot read the answer. (Getting this wrong once cost a
# whole e2e run: a bare-list assumption made every poll exit non-zero and the
# waiter reported "did not exit" about a Worker that had exited.)
views = doc.get("workers") if isinstance(doc, dict) else doc
if not isinstance(views, list):
    sys.exit(2)
for v in views:
    if not isinstance(v, dict):
        continue
    if v.get("task_id") != task:
        continue
    if not want:
        print(v.get("run_id", ""))
        sys.exit(0)
    if v.get("run_id") == want:
        print(v["exit_status"] if v.get("exit_status") is not None else "-")
        sys.exit(0)
sys.exit(1)' "$task" "$run"
  rc=$?
  # 2 = the output was not the shape this helper reads; 1 = no such record yet
  # (the normal answer while polling). Only the first is worth a word.
  if [ $rc -eq 2 ]; then fg_record_query_note "unreadable worker list output: $(printf '%s' "$raw" | head -c 200)"; fi
  return $rc
}

# fg_record_query_note MSG — say why the registry reading is unavailable, at
# most once per run (a poll loop must not repeat it 80 times).
fg_record_query_note() {
  [ -e "$FG_SCRATCH/record-query-warned" ] && return 0
  : > "$FG_SCRATCH/record-query-warned"
  printf 'NOTE fg_record_query: %s\n' "$*" >&2
}

# fg_wait_exit REPO TASKID [TIMEOUT_SECS] — wait for the run that collect will
# judge to be over, judged by the fact collect judges it by: rddev's recorded
# exit (the registry's exit_status for THIS run).
#
# Not exit.status on disk (#207). The reaper writes that file FIRST and only
# then collects the Worker's process group, so a waiter that stops at the file
# returns with the run's session still being torn down — and the file it saw
# need not even be this attempt's (the previous attempt's survives on disk until
# the re-spawn rewrites it). The run_id is pinned before waiting, so the answer
# is about the attempt this call followed and never about the one before it. The
# recorded exit is merged by reconcile once the reaper has stopped running, the
# same condition review/worker collect require before they look at anything
# else: one fact, waited on by both sides.
fg_wait_exit() {
  local repo="$1" task="$2" tmo="${3:-30}" n=0 run="" exits=""
  # pin the run first; the spawn that this wait follows has already written it.
  # Its own counter: the timeout below is the budget for the wait, and spending
  # part of it on pinning would shorten the very wait it is meant to bound.
  while [ $n -lt 40 ] && [ -z "$run" ]; do
    run="$(fg_record_query "$repo" "$task" || true)"
    if [ -z "$run" ]; then sleep 0.25; n=$((n+1)); fi
  done
  [ -n "$run" ] || return 1
  n=0
  while [ $n -lt $((tmo*2)) ]; do
    exits="$(fg_record_query "$repo" "$task" "$run" || true)"
    if [ -n "$exits" ] && [ "$exits" != "-" ]; then return 0; fi
    sleep 0.5
    n=$((n+1))
  done
  return 1
}

# fg_assert_reaper_stopped REPO TASK LABEL — the recorded session leader (the
# reaper) must not be running any more. The run is over once the reaper has
# stopped: it writes exit.status, then collects the Worker's process group, then
# exits, so a waiter that returns with this process alive has returned before
# the run was over (#207). A zombie counts as stopped — it is finished, just
# unreaped. Reads /proc directly: no dependency beyond python3.
fg_assert_reaper_stopped() {
  local repo="$1" task="$2" label="$3" pid state
  pid="$(python3 -c '
import json, sys
try:
    print(json.load(open(sys.argv[1])).get("session_leader_pid") or 0)
except Exception:
    print(0)' "$repo/.rddev/workers/$task/registry.json")" || pid=0
  if [ "$pid" = "0" ]; then
    fg_fail "$label: the registry names no session leader to judge"
    return
  fi
  state="$(python3 -c '
import sys
try:
    stat = open("/proc/%s/stat" % sys.argv[1]).read()
except OSError:
    print("gone"); raise SystemExit
print(stat.rsplit(")", 1)[1].split()[0])' "$pid")" || state=gone
  case "$state" in
    gone|Z) fg_ok "$label" ;;
    *) fg_fail "$label: the reaper (pid $pid) is still running (state $state) while the run reads finished" ;;
  esac
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
