#!/usr/bin/env bash
#
# The driver must outlive the thing that started it.
#
# A driver owned by the Supervisor's conversation is not a driver: it stops when
# the conversation does, and finished Workers then sit uncollected with nobody to
# notice. That is not hypothetical — it is what happened, so this asserts the
# process properties rather than the code paths: a driver launched from a parent
# that then exits is still alive, still heartbeating, still holding its slot,
# and still adopting the Worker it found rather than starting a second one.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

WORK="$(mktemp -d)"
DRIVER_PID=""
SLEEPER_PID=""
cleanup() {
  [[ -n "$DRIVER_PID" ]] && kill "$DRIVER_PID" 2>/dev/null
  [[ -n "$SLEEPER_PID" ]] && kill "$SLEEPER_PID" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

REPO="$WORK/repo"
mkdir -p "$REPO/tasks" "$REPO/.rddev/workers/T9001" "$REPO/.rddev/worktrees/T9001"
cat > "$REPO/tasks/tasks.json" <<'JSON'
{"version":1,"task_count":1,"phases":{"P1":"x"},"tasks":[
 {"id":"T9001","phase":"P1","title":"a task with a Worker already running",
  "dependencies":[],"requirements":["r"],"acceptance_criteria":["a"],
  "allowed_scope":["x/**"],"decision_level_max":"L1"}]}
JSON
cat > "$REPO/tasks/task_status.json" <<'JSON'
{"version":2,"tasks":{"T9001":{"status":"running","started_at":"2026-09-13T00:00:00Z","worker_run_id":"run-existing"}}}
JSON

# A Worker that is already running: the driver must ADOPT this, not replace it.
sleep 300 & SLEEPER_PID=$!
# The recorded start time must be the process's REAL one: discovery compares it
# against /proc to catch a recycled pid, so a made-up value reads as "not this
# process" and the Worker looks gone.
START_TICKS="$(python3 -c "
import sys
f=open('/proc/$SLEEPER_PID/stat').read()
print(f[f.rindex(chr(41))+2:].split()[19])
")"
cat > "$REPO/.rddev/workers/T9001/registry.json" <<JSON
{"task_id":"T9001","run_id":"run-existing","session_id":"s","claude_version":"v",
 "pid":$SLEEPER_PID,"start_time":$START_TICKS,"worktree":"$REPO/.rddev/worktrees/T9001",
 "branch":"task/T9001-x","baseline_sha":"0000000000000000000000000000000000000000",
 "refs_before":[],"log_path":"$REPO/.rddev/workers/T9001/worker.log",
 "result_dir":"$REPO/.rddev/workers/T9001","started_at":"2026-09-13T00:00:00Z"}
JSON
: > "$REPO/.rddev/workers/T9001/worker.log"

go build -o "$WORK/rddev" ./cmd/rddev || { fail "building rddev"; printf '\n%d failure(s)\n' "$FAILS"; exit 1; }
ok "rddev built"

# Launch from a subshell that EXITS immediately. This is the property under
# test: the driver must not be a child of the launcher's lifetime.
( setsid "$WORK/rddev" drive --parallel 1 --poll 1s \
    --repo-root "$REPO" --tasks-json "$REPO/tasks/tasks.json" --state-json "$REPO/tasks/task_status.json" \
    >"$WORK/driver.log" 2>&1 & echo $! > "$WORK/driver.pid" )
sleep 3
DRIVER_PID="$(cat "$WORK/driver.pid" 2>/dev/null || true)"

if [[ -z "$DRIVER_PID" ]] || ! kill -0 "$DRIVER_PID" 2>/dev/null; then
  fail "the driver did not survive its launcher: $(tail -3 "$WORK/driver.log")"
  printf '\n%d failure(s)\n' "$FAILS"; exit 1
fi
ok "the driver outlived the subshell that started it (pid $DRIVER_PID)"

if [[ "$(ps -o ppid= -p "$DRIVER_PID" | tr -d ' ')" == "1" ]]; then
  ok "it is reparented to init, not owned by the launcher"
else
  fail "the driver's parent is still $(ps -o ppid= -p "$DRIVER_PID" | tr -d ' ') — it is tied to something else's lifetime"
fi

# Heartbeat advances: liveness is a fact on disk, not an inference.
H1="$("$WORK/rddev" status --repo-root "$REPO" --tasks-json "$REPO/tasks/tasks.json" --state-json "$REPO/tasks/task_status.json" --json | python3 -c 'import json,sys;print(json.load(sys.stdin).get("heartbeat_age",""))')"
sleep 3
STATUS="$("$WORK/rddev" status --repo-root "$REPO" --tasks-json "$REPO/tasks/tasks.json" --state-json "$REPO/tasks/task_status.json" --json)"
echo "$STATUS" | python3 -c '
import json,sys
d=json.load(sys.stdin)
assert d["driver"]=="alive", d["driver"]
assert d["driver_pid"]>0, d
assert any(w["task"]=="T9001" for w in d["running_workers"]), d
' && ok "status reports the driver alive and the adopted Worker running" \
  || fail "status did not report a live driver with the adopted Worker: $STATUS"

# Adoption, not replacement: no second Worker was spawned for a task that
# already had one.
if pgrep -f "rddev-worker-T9001" >/dev/null 2>&1; then
  fail "a second Worker was spawned for a task whose Worker was still running"
else
  ok "the running Worker was adopted, not re-spawned"
fi

# The slot is exclusive across processes.
if "$WORK/rddev" drive --once --repo-root "$REPO" --tasks-json "$REPO/tasks/tasks.json" --state-json "$REPO/tasks/task_status.json" >"$WORK/second.log" 2>&1; then
  fail "a second driver acquired the slot while the first held it"
else
  grep -q 'another driver already holds the slot' "$WORK/second.log" \
    && ok "a second driver refuses and names the holder" \
    || fail "the second driver failed for the wrong reason: $(tail -2 "$WORK/second.log")"
fi

# After the driver stops, the slot frees so a crashed driver cannot wedge it.
kill "$DRIVER_PID" 2>/dev/null; sleep 2
if "$WORK/rddev" drive --once --repo-root "$REPO" --tasks-json "$REPO/tasks/tasks.json" --state-json "$REPO/tasks/task_status.json" >/dev/null 2>&1; then
  ok "the slot is released when the driver exits (a crash cannot wedge it)"
else
  fail "the slot stayed held after the driver exited"
fi
DRIVER_PID=""

printf '\n'
if (( FAILS )); then
  printf 'driver-persistence-e2e: %d failure(s)\n' "$FAILS"
  exit 1
fi
printf 'driver-persistence-e2e: all checks passed\n'
