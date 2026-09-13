#!/usr/bin/env bash
#
# The Supervisor's unattended driver.
#
# Why this exists: the mechanical stretch of the loop — dispatch, wait, collect,
# review, accept, commit, push, open, wait for CI, merge — needs no judgement,
# and doing it by hand is what made progress stop between tasks. What DOES need
# judgement is every point where the evidence disagrees with itself: a rejected
# collect, a review that asks for changes, a refused accept, a conflicting PR,
# a red CI job. Those are the driver's stop conditions, and they return control
# to the Supervisor rather than to the user.
#
# It never lowers a gate. Every action goes through `rddev`, which refuses on
# its own terms; this script only decides WHAT to attempt next, never whether a
# gate passed.
#
# Stops (exit 0 with a report) when:
#   - the DAG frontier is empty and nothing is running  -> phase complete
#   - a decision is needed (the list above)             -> hand back to the Supervisor
#   - --max-tasks reached
# Never stops merely because a task finished: that is the whole point.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PARALLEL="${PARALLEL:-2}"
MAX_TASKS="${MAX_TASKS:-0}"   # 0 = no limit
RDDEV="${RDDEV:-go run ./cmd/rddev}"
POLL="${POLL:-20}"

done_count=0
STATUS="complete"
DECISION=""

log()  { printf '%s supervise: %s\n' "$(date +%H:%M:%S)" "$*"; }
rddev() { $RDDEV "$@"; }

running_tasks() {
  rddev worker list 2>/dev/null | awk '$0 ~ /running/ {print $1}' | grep -v -- '-review$'
}
dispatchable() {
  rddev task next 2>/dev/null | awk 'NF {print $1}'
}
exited_uncollected() {
  # Workers that finished and whose task is still `running` in the state file:
  # exactly the ones waiting to be collected.
  local t
  for t in $(rddev worker list 2>/dev/null | awk '$0 ~ /exited/ {print $1}' | grep -v -- '-review$'); do
    local st
    st="$(python3 - "$t" <<'PY'
import json,sys
t=sys.argv[1]
try:
    d=json.load(open("tasks/task_status.json"))
except Exception:
    print(""); raise SystemExit
print(d.get("tasks",{}).get(t,{}).get("status",""))
PY
)"
    [[ "$st" == "running" ]] && echo "$t"
  done
}

log "starting: parallel=$PARALLEL, max_tasks=${MAX_TASKS:-unlimited}"

while :; do
  # 1) Collect anything that finished, then take it as far as the gates allow.
  for task in $(exited_uncollected); do
    log "collecting $task"
    if ! out="$(rddev worker collect "$task" 2>&1)"; then
      STATUS="decision"; DECISION="collect rejected for $task — triage the reasons, then rework or respawn:
$out"
      break 2
    fi
    log "reviewing $task"
    rddev review spawn "$task" --timeout 45m >/dev/null 2>&1
    while rddev worker list 2>/dev/null | grep -q "^$task-review.*running"; do sleep "$POLL"; done
    if ! out="$(rddev review collect "$task" 2>&1)" || echo "$out" | grep -q request_changes; then
      STATUS="decision"; DECISION="review needs a decision for $task:
$out"
      break 2
    fi
    log "accepting $task"
    if ! out="$(rddev task accept "$task" 2>&1)"; then
      # A parallel task merging advances main, which invalidates THIS task's
      # verdict by design: it was written for a code state that no longer
      # exists. That is the binding working, not a failure - and the repair is
      # mechanical, so the loop performs it instead of stopping. A merge
      # CONFLICT is a different thing and does stop: resolving one is a
      # judgement call.
      if echo "$out" | grep -q 'DIFFERENT code state'; then
        log "$task's verdict is stale (main advanced); advancing its baseline"
        wt=".rddev/worktrees/$task"
        if git -C "$wt" -c user.name=supervisor -c user.email=supervisor@post.local \
             merge main --no-edit >"$wt/.supervise-merge.log" 2>&1; then
          printf '%s\n' "Baseline advanced: main moved under this task while it was reviewed, so the \
review verdict no longer describes the composed code. The branch was merged with the \
current main and your work reapplied unchanged. Re-verify on the merged baseline: run \
the required tests and make test-integration again, confirm nothing broke, and re-submit \
RESULT.json (keep the label fields). Do not weaken or delete any test." \
            > ".rddev/worktrees/$task/.supervise-reason"
          rddev task reject "$task" --reason-file "$wt/.supervise-reason" >/dev/null 2>&1
          rm -f "$wt/.supervise-reason"
          if rddev worker rework "$task" --timeout 60m >/dev/null 2>&1; then
            log "$task reworking on the advanced baseline"
            continue
          fi
        fi
        STATUS="decision"
        DECISION="advancing $task's baseline needs the Supervisor (a merge conflict, or the rework was refused):
$out"
        break 2
      fi
      STATUS="decision"; DECISION="accept refused for $task:
$out"
      break 2
    fi
    rddev git commit "$task" >/dev/null 2>&1 || true   # refuses when the tree is unchanged
    if ! out="$(rddev git push "$task" 2>&1)"; then
      STATUS="decision"; DECISION="push failed for $task:
$out"
      break 2
    fi
    rddev pr open "$task" >/dev/null 2>&1 || true      # an existing PR is fine
    branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null)"
    task_branch="$(python3 - "$task" <<'PY'
import json,sys
t=sys.argv[1]
d=json.load(open("tasks/tasks.json"))
for x in d["tasks"]:
    if x["id"]==t:
        import re
        print("task/%s-%s" % (t, re.sub(r'[^a-z0-9]+','-',x["title"].lower()).strip('-')))
PY
)"
    log "waiting for CI on $task_branch"
    for _ in $(seq 1 90); do
      out="$(gh pr checks "$task_branch" 2>&1)"
      echo "$out" | grep -q 'no checks reported' && { sleep "$POLL"; continue; }
      echo "$out" | grep -qE 'pending|in_progress' && { sleep "$POLL"; continue; }
      break
    done
    if echo "$out" | grep -qE 'fail'; then
      STATUS="decision"; DECISION="CI is red on $task's PR:
$out"
      break 2
    fi
    log "merging $task"
    if ! out="$(rddev pr merge "$task" 2>&1)"; then
      STATUS="decision"; DECISION="merge refused for $task:
$out"
      break 2
    fi
    git checkout -q main && git pull -q --ff-only
    done_count=$((done_count+1))
    log "merged $task ($done_count this run)"
    if (( MAX_TASKS > 0 && done_count >= MAX_TASKS )); then
      STATUS="max-tasks"; break 2
    fi
  done

  # 2) Fill the pipeline.
  while (( $(running_tasks | wc -l) < PARALLEL )); do
    next="$(dispatchable | head -1)"
    [[ -z "$next" ]] && break
    log "dispatch $next"
    if ! out="$(rddev task ready "$next" 2>&1)"; then
      STATUS="decision"; DECISION="ready refused for $next:
$out"
      break 2
    fi
    if ! out="$(rddev worker spawn "$next" --timeout 90m 2>&1)"; then
      STATUS="decision"; DECISION="spawn refused for $next (a phase with no G3 refuses here):
$out"
      break 2
    fi
  done

  if [[ -z "$(running_tasks)" && -z "$(exited_uncollected)" && -z "$(dispatchable)" ]]; then
    break
  fi
  sleep "$POLL"
done

printf '\n'
case "$STATUS" in
  complete)   printf 'supervise: no dispatchable work left and nothing running — %d task(s) merged this run.\n' "$done_count" ;;
  max-tasks)  printf 'supervise: reached %d task(s) this run.\n' "$done_count" ;;
  decision)   printf 'supervise: STOPPED for a Supervisor decision.\n%s\n' "$DECISION" ;;
esac
