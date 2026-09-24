#!/usr/bin/env bash
#
# Unit tests for scripts/update_progress.py (fixture-driven).
#
# Exercises the progress-view regenerator against a sandbox copy of
# tasks/tasks.json + tasks/task_status.json + a stub tasks/progress.md
# (--root). The REAL tasks/progress.md is never read or written here.
# Runnable on a host with bash + coreutils + python3 (stdlib only).
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
UPDATER="$ROOT/scripts/update_progress.py"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILS=0
RC=0; OUT=""; ERR=""

fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

mkfixture() { # dir — sandbox with task files + stub progress.md
  local dir="$1"
  mkdir -p "$dir/tasks"
  python3 - "$dir" <<'PY'
import json, sys
dir_ = sys.argv[1]
json.dump({"version": "2026-09", "task_count": 2, "tasks": [
    {"id": "T0001", "title": "preflight", "phase": "P0"},
    {"id": "T0002", "title": "monorepo", "phase": "P0"}]},
    open(f"{dir_}/tasks/tasks.json", "w"), indent=2)
json.dump({"version": "2026-09", "overall": "P0_in_progress", "tasks": {
    "T0001": {"status": "merged", "merged_at": "2026-09-12T13:20:00Z"},
    "T0002": {"status": "todo"}}},
    open(f"{dir_}/tasks/task_status.json", "w"), indent=2)
PY
  cat > "$dir/tasks/progress.md" <<'MD'
# 开发进度

手写内容（hand-written narrative, must survive regeneration）.

MD
}

run_updater() { # dir [extra args...] -> sets RC/OUT/ERR
  OUT="$(python3 "$UPDATER" --root "$1" --stamp 2026-09-12T00:00:00Z "${@:2}" 2>"$WORK/err")"
  RC=$?
  ERR="$(cat "$WORK/err")"
}

run_real() { # dir [extra args...] -> sets RC/OUT/ERR; NO injected --stamp,
             # i.e. the invocation `make progress` / CI actually makes.
  OUT="$(python3 "$UPDATER" --root "$1" "${@:2}" 2>"$WORK/err")"
  RC=$?
  ERR="$(cat "$WORK/err")"
}

# --- 1. first run seeds the marked section, keeps hand-written content ------
mkfixture "$WORK/seed"
run_updater "$WORK/seed"
if [[ $RC -eq 0 ]] && grep -q "AUTO-PROGRESS:BEGIN" "$WORK/seed/tasks/progress.md" \
   && grep -q "任务状态自动总览" "$WORK/seed/tasks/progress.md" \
   && grep -q "手写内容" "$WORK/seed/tasks/progress.md"; then
  ok "first run: seeds marked section, hand-written content intact"
else
  fail "first run: rc=$RC err=$ERR"
fi

# --- 2. the generated table carries every task with its real status ---------
if grep -q "| T0001 | preflight | P0 | merged " "$WORK/seed/tasks/progress.md" \
   && grep -q "| T0002 | monorepo | P0 | todo " "$WORK/seed/tasks/progress.md" \
   && grep -q "状态分布：todo 1 · ready 0 · running 0 · worker_failed 0 · verification 0 · rejected 0 · blocked 0 · accepted 0 · merged 1（合计 2/2 个任务）" "$WORK/seed/tasks/progress.md"; then
  ok "generated table: every task listed with its status and counts"
else
  fail "generated table: unexpected content"
fi

# --- 3. second run is idempotent ---------------------------------------------
before="$(sha256sum "$WORK/seed/tasks/progress.md" | cut -d' ' -f1)"
run_updater "$WORK/seed"
after="$(sha256sum "$WORK/seed/tasks/progress.md" | cut -d' ' -f1)"
if [[ $RC -eq 0 && "$before" == "$after" ]] && echo "$OUT" | grep -q "already up to date"; then
  ok "second run: idempotent, no rewrite"
else
  fail "second run: rc=$RC before=$before after=$after out=$OUT"
fi

# --- 4. --check passes on the fresh section ----------------------------------
run_updater "$WORK/seed" --check
if [[ $RC -eq 0 ]] && echo "$OUT" | grep -q "up to date"; then
  ok "--check: passes when the section matches task_status"
else
  fail "--check fresh: rc=$RC out=$OUT"
fi

# --- 5. --check fails after task_status changes ------------------------------
python3 - "$WORK/seed" <<'PY'
import json, sys
p = f"{sys.argv[1]}/tasks/task_status.json"
d = json.load(open(p))
d["tasks"]["T0002"]["status"] = "running"
json.dump(d, open(p, "w"))
PY
run_updater "$WORK/seed" --check
if [[ $RC -eq 1 ]] && echo "$ERR" | grep -q "stale"; then
  ok "--check: fails loudly when the view is stale"
else
  fail "--check stale: rc=$RC out=$OUT err=$ERR"
fi

# --- 6. regeneration repairs the stale section -------------------------------
run_updater "$WORK/seed"
run_updater "$WORK/seed" --check
if [[ $RC -eq 0 ]] && grep -q "| T0002 | monorepo | P0 | running " "$WORK/seed/tasks/progress.md"; then
  ok "regeneration: stale section repaired, --check green again"
else
  fail "regeneration: rc=$RC out=$OUT"
fi

# --- 7. --check on a file without markers only notices, never fails ----------
mkfixture "$WORK/nomark"
run_updater "$WORK/nomark" --check
if [[ $RC -eq 0 ]] && echo "$OUT" | grep -q "no auto section"; then
  ok "--check without markers: prints notice, exit 0"
else
  fail "--check no markers: rc=$RC out=$OUT"
fi

# --- 8. --check compares content, not the generation stamp (T1228) -----------
# The section was generated at a stamp far in the past and is checked with the
# real clock (no --stamp): the content is current, so --check must say yes.
# While the stamp was part of the comparison this could only ever answer "no"
# for a committed file, which is what made it unusable as a gate.
mkfixture "$WORK/stampok"
run_real "$WORK/stampok" --stamp 2020-01-01T00:00:00Z   # seed at an old stamp
run_real "$WORK/stampok" --check
if [[ $RC -eq 0 ]] && echo "$OUT" | grep -q "up to date"; then
  ok "--check: a past stamp does not make a current section stale (exit 0)"
else
  fail "--check stamp-blind: rc=$RC out=$OUT err=$ERR"
fi

# --- 9. ... and it still answers "no" when the content really moved ----------
# Same sandbox, same real clock, only task_status.json changes: the red may not
# come from the clock. Both halves are asserted in one case on purpose — a check
# that always says yes would satisfy the second half alone (T1228: an instrument
# that can only say "no" is worth no more than one that can only say "yes").
mkfixture "$WORK/seesaw"
run_real "$WORK/seesaw" --stamp 2019-06-01T00:00:00Z    # seed at an old stamp
run_real "$WORK/seesaw" --check
rc_current=$RC
python3 - "$WORK/seesaw" <<'PY'
import json, sys
p = f"{sys.argv[1]}/tasks/task_status.json"
d = json.load(open(p))
d["tasks"]["T0001"]["status"] = "accepted"      # merged -> accepted
json.dump(d, open(p, "w"))
PY
run_real "$WORK/seesaw" --check
rc_stale=$RC
if [[ $rc_current -eq 0 && $rc_stale -eq 1 ]] && echo "$ERR" | grep -q "stale"; then
  ok "--check: answers yes while current, no once the content moved"
else
  fail "--check two-sided: rc_current=$rc_current rc_stale=$rc_stale out=$OUT err=$ERR"
fi

# --- summary -------------------------------------------------------------------
echo ""
if [[ $FAILS -eq 0 ]]; then
  echo "progress-update-test: all cases passed"
  exit 0
fi
echo "progress-update-test: $FAILS case(s) failed"
exit 1
