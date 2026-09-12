#!/usr/bin/env bash
#
# Unit tests for the POST spec validation suite (fixture-driven).
#
# Exercises scripts/validate_specs.py decision logic against injected
# fixture task-DAG trees (--root), and scripts/source_repo_preflight.py
# decision logic against injected remote/visibility state (--fixture /
# --observed-visibility, contract: docs/69 §2.1). Never touches the real
# host git state or the network: in --fixture mode the preflight executes
# no real git/gh commands, and the validator only reads the injected tree.
#
# Runnable on a host with bash + coreutils + python3 (stdlib only).
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
VALIDATOR="$ROOT/scripts/validate_specs.py"
PREFLIGHT="$ROOT/scripts/source_repo_preflight.py"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILS=0
RC=0; OUT=""; ERR=""

fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }
assert_eq() { # desc expected actual
  if [[ "$2" == "$3" ]]; then ok "$1"; else fail "$1: expected [$2] got [$3]"; fi
}
assert_contains() { # desc file needle
  if grep -qF -- "$3" "$2"; then ok "$1"; else fail "$1: [$3] not found in $2"; fi
}

jget() { # file key... -> value
  python3 - "$@" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
for k in sys.argv[2:]:
    d = d[int(k)] if isinstance(d, list) else d[k]
print(d)
PY
}

find_check() { # file id field -> value
  python3 - "$@" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
c = [x for x in d["checks"] if x["id"] == sys.argv[2]][0]
v = c.get(sys.argv[3], "")
print("" if v is None else v)
PY
}

reason_for() { # file check-id -> space-joined reason codes
  python3 - "$@" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
print(" ".join(r["reason"] for r in d["verdict"]["reasons"]
               if r["check"] == sys.argv[2]))
PY
}

# ==========================================================================
# 1. Task DAG validation (validate_specs.py --root fixtures)
# ==========================================================================

VALID_TASKS='{"version":1,"task_count":3,"tasks":[
 {"id":"T0000","dependencies":[],"requirements":["r"],"acceptance_criteria":["a"]},
 {"id":"T0001","dependencies":["T0000"],"requirements":["r"],"acceptance_criteria":["a"]},
 {"id":"T0002","dependencies":["T0001"],"requirements":["r"],"acceptance_criteria":["a"]}]}'

# mktree DIR: temp root with the real specs/docs/root-md + injected tasks.json
mktree() {
  local d="$1"
  mkdir -p "$d/tasks" "$d/docs"
  cp -r "$ROOT/specs" "$d/"
  cp "$ROOT/PROJECT_REPOSITORY.md" "$ROOT/README.md" "$d/"
  cp -r "$ROOT/docs/." "$d/docs/"
  printf '%s' "$VALID_TASKS" > "$d/tasks/tasks.json"
}

run_val() { # name dir [flags...]
  local name="$1" dir="$2"; shift 2
  OUT="$WORK/$name.out"; ERR="$WORK/$name.err"
  "$VALIDATOR" --root "$dir" "$@" >"$OUT" 2>"$ERR"
  RC=$?
}

# -- valid DAG: everything passes
mktree "$WORK/t-valid"
run_val valid "$WORK/t-valid"
assert_eq "dag valid: exit code" "0" "$RC"
assert_contains "dag valid: summary" "$WORK/valid.out" "checks passed"

# -- orphan dependency: T0002 depends on undefined T9999
mktree "$WORK/t-missing"
printf '%s' '{"version":1,"task_count":3,"tasks":[
 {"id":"T0000","dependencies":[],"requirements":["r"],"acceptance_criteria":["a"]},
 {"id":"T0001","dependencies":["T0000"],"requirements":["r"],"acceptance_criteria":["a"]},
 {"id":"T0002","dependencies":["T9999"],"requirements":["r"],"acceptance_criteria":["a"]}]}' \
  > "$WORK/t-missing/tasks/tasks.json"
run_val missing "$WORK/t-missing"
assert_eq "dag missing dependency: exit code" "1" "$RC"
assert_contains "dag missing dependency: names offender" "$WORK/missing.out" "T0002 -> T9999"

# -- JSON mode on the failing tree: exit_code field == real rc
run_val missing-json "$WORK/t-missing" --json
assert_eq "dag missing dependency --json: exit code" "1" "$RC"
assert_eq "dag missing dependency --json: exit_code field" "1" "$(jget "$WORK/missing-json.out" exit_code)"
assert_eq "dag missing dependency --json: ok field" "False" "$(jget "$WORK/missing-json.out" ok)"

# -- cycle: T0000 <-> T0001
mktree "$WORK/t-cycle"
printf '%s' '{"version":1,"task_count":3,"tasks":[
 {"id":"T0000","dependencies":["T0001"],"requirements":["r"],"acceptance_criteria":["a"]},
 {"id":"T0001","dependencies":["T0000"],"requirements":["r"],"acceptance_criteria":["a"]},
 {"id":"T0002","dependencies":["T0001"],"requirements":["r"],"acceptance_criteria":["a"]}]}' \
  > "$WORK/t-cycle/tasks/tasks.json"
run_val cycle "$WORK/t-cycle"
assert_eq "dag cycle: exit code" "1" "$RC"
assert_contains "dag cycle: cycle path reported" "$WORK/cycle.out" "T0000 -> T0001 -> T0000"

# -- task missing acceptance criteria
mktree "$WORK/t-noaccept"
printf '%s' '{"version":1,"task_count":3,"tasks":[
 {"id":"T0000","dependencies":[],"requirements":["r"],"acceptance_criteria":[]},
 {"id":"T0001","dependencies":["T0000"],"requirements":["r"],"acceptance_criteria":["a"]},
 {"id":"T0002","dependencies":["T0001"],"requirements":["r"],"acceptance_criteria":["a"]}]}' \
  > "$WORK/t-noaccept/tasks/tasks.json"
run_val noaccept "$WORK/t-noaccept"
assert_eq "dag missing acceptance_criteria: exit code" "1" "$RC"
assert_contains "dag missing acceptance_criteria: names task" "$WORK/noaccept.out" "T0000: empty acceptance_criteria"

# ==========================================================================
# 2. Repository preflight (source_repo_preflight.py --fixture matrix)
# ==========================================================================

# write_spec FILE [with-expectation: 1|0] — canonical spec, optionally
# without the visibility_expectation block (the "unrecorded" case).
write_spec() {
  local f="$1" exp="${2:-1}"
  cat > "$f" <<'YAML'
version: 1
source_repository:
  provider: github
  owner: lichman0405
  name: post
  full_name: lichman0405/post
  https_clone_url: https://github.com/lichman0405/post.git
  ssh_clone_url: git@github.com:lichman0405/post.git
  default_branch: main
  visibility_at_package_generation: public
  observed_at: '2026-09-12'
YAML
  if [[ "$exp" == 1 ]]; then
    cat >> "$f" <<'YAML'
  visibility_expectation:
    value: private
    confirmed_by: repository owner
    confirmed_at: '2026-09-12'
    decision_ref: tasks/decisions.md L3-20260912-1
YAML
  fi
  cat >> "$f" <<'YAML'
  runtime_visibility_must_be_rechecked_before_first_push: true
YAML
}

# mkfix DIR [with-expectation] [remote-v: canon|ssh|nogit|evil|empty|none]
mkfix() {
  local d="$1" exp="${2:-1}" rv="${3:-canon}"
  mkdir -p "$d"
  write_spec "$d/source-repository.yaml" "$exp"
  case "$rv" in
    canon) printf 'origin\thttps://github.com/lichman0405/post.git (fetch)\norigin\thttps://github.com/lichman0405/post.git (push)\n' > "$d/git-remote-v.txt" ;;
    ssh)   printf 'origin\tgit@github.com:lichman0405/post.git (fetch)\norigin\tgit@github.com:lichman0405/post.git (push)\n' > "$d/git-remote-v.txt" ;;
    nogit) printf 'origin\thttps://github.com/lichman0405/post (fetch)\norigin\thttps://github.com/lichman0405/post (push)\n' > "$d/git-remote-v.txt" ;;
    evil)  printf 'origin\thttps://github.com/attacker/post.git (fetch)\norigin\thttps://github.com/attacker/post.git (push)\n' > "$d/git-remote-v.txt" ;;
    empty) : > "$d/git-remote-v.txt" ;;
    none)  ;; # no remote fixture at all
  esac
}

run_pf() { # name fixture-dir spec-file [flags...] -> sets RC, OUT, ERR
  local name="$1" fix="$2" spec="$3"; shift 3
  OUT="$WORK/$name.json"; ERR="$WORK/$name.err"
  "$PREFLIGHT" --spec "$spec" --fixture "$fix" "$@" --json >"$OUT" 2>"$ERR"
  RC=$?
  if ! python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$OUT" 2>/dev/null; then
    fail "$name: stdout is not valid JSON (rc=$RC)"
    cat "$OUT"
    return 1
  fi
}

# -- verdict matrix: match -> bless
FIX="$WORK/pf-match"; mkfix "$FIX" 1 canon
printf 'private\n' > "$FIX/visibility.txt"
printf 'refs/remotes/origin/main\n' > "$FIX/origin-head.txt"
run_pf match "$FIX" "$FIX/source-repository.yaml"
assert_eq "pf match: exit code" "0" "$RC"
assert_eq "pf match: exit_code field" "0" "$(jget "$OUT" exit_code)"
assert_eq "pf match: VISIBILITY status" "passed" "$(find_check "$OUT" VISIBILITY status)"
assert_eq "pf match: SPEC-EXPECTATION status" "passed" "$(find_check "$OUT" SPEC-EXPECTATION status)"
assert_eq "pf match: BRANCH-DEFAULT status" "passed" "$(find_check "$OUT" BRANCH-DEFAULT status)"
assert_eq "pf match: push_blessing" "bless" "$(jget "$OUT" verdict push_blessing)"
assert_eq "pf match: no reasons" "[]" "$(jget "$OUT" verdict reasons)"

# -- verdict matrix: mismatch (observed public vs expectation private)
FIX="$WORK/pf-mismatch"; mkfix "$FIX" 1 canon
printf 'public\n' > "$FIX/visibility.txt"
run_pf mismatch "$FIX" "$FIX/source-repository.yaml"
assert_eq "pf mismatch: exit code" "1" "$RC"
assert_eq "pf mismatch: VISIBILITY status" "failed" "$(find_check "$OUT" VISIBILITY status)"
assert_eq "pf mismatch: reason" "visibility_mismatch" "$(reason_for "$OUT" VISIBILITY)"
assert_eq "pf mismatch: push_blessing" "refused" "$(jget "$OUT" verdict push_blessing)"

# -- verdict matrix: unknown / cannot verify
FIX="$WORK/pf-unknown"; mkfix "$FIX" 1 canon
printf 'unknown\n' > "$FIX/visibility.txt"
run_pf unknown "$FIX" "$FIX/source-repository.yaml"
assert_eq "pf unknown: exit code" "1" "$RC"
assert_eq "pf unknown: VISIBILITY status" "unknown" "$(find_check "$OUT" VISIBILITY status)"
assert_eq "pf unknown: reason" "visibility_unverifiable" "$(reason_for "$OUT" VISIBILITY)"
assert_eq "pf unknown: push_blessing" "refused" "$(jget "$OUT" verdict push_blessing)"
assert_contains "pf unknown: detail says cannot verify" "$OUT" "cannot be verified"

# -- verdict matrix: expectation unrecorded (spec without the block)
FIX="$WORK/pf-unrecorded"; mkfix "$FIX" 0 canon
printf 'private\n' > "$FIX/visibility.txt"
run_pf unrecorded "$FIX" "$FIX/source-repository.yaml"
assert_eq "pf unrecorded: exit code" "1" "$RC"
assert_eq "pf unrecorded: SPEC-EXPECTATION status" "failed" "$(find_check "$OUT" SPEC-EXPECTATION status)"
assert_eq "pf unrecorded: reason" "expectation_unrecorded" "$(reason_for "$OUT" SPEC-EXPECTATION)"
assert_eq "pf unrecorded: VISIBILITY status" "error" "$(find_check "$OUT" VISIBILITY status)"
assert_eq "pf unrecorded: push_blessing" "refused" "$(jget "$OUT" verdict push_blessing)"

# -- remote normalization: SSH scp form (git@github.com:owner/name.git)
FIX="$WORK/pf-ssh"; mkfix "$FIX" 1 ssh
printf 'private\n' > "$FIX/visibility.txt"
run_pf ssh "$FIX" "$FIX/source-repository.yaml"
assert_eq "pf ssh: REPO-CANONICAL status" "passed" "$(find_check "$OUT" REPO-CANONICAL status)"
assert_eq "pf ssh: normalized owner/name" "lichman0405/post" "$(find_check "$OUT" REPO-CANONICAL measured)"

# -- remote normalization: HTTPS without trailing .git
FIX="$WORK/pf-nogit"; mkfix "$FIX" 1 nogit
printf 'private\n' > "$FIX/visibility.txt"
run_pf nogit "$FIX" "$FIX/source-repository.yaml"
assert_eq "pf no-.git: REPO-CANONICAL status" "passed" "$(find_check "$OUT" REPO-CANONICAL status)"
assert_eq "pf no-.git: normalized owner/name" "lichman0405/post" "$(find_check "$OUT" REPO-CANONICAL measured)"

# -- non-canonical origin is rejected
FIX="$WORK/pf-evil"; mkfix "$FIX" 1 evil
printf 'private\n' > "$FIX/visibility.txt"
run_pf evil "$FIX" "$FIX/source-repository.yaml"
assert_eq "pf evil: exit code" "1" "$RC"
assert_eq "pf evil: REPO-CANONICAL status" "failed" "$(find_check "$OUT" REPO-CANONICAL status)"
assert_eq "pf evil: reason" "remote_not_canonical" "$(reason_for "$OUT" REPO-CANONICAL)"
assert_eq "pf evil: push_blessing" "refused" "$(jget "$OUT" verdict push_blessing)"

# -- get-url output form (no remote -v fixture)
FIX="$WORK/pf-geturl"; mkfix "$FIX" 1 none
printf 'https://github.com/lichman0405/post.git\n' > "$FIX/git-remote-get-url.txt"
printf 'private\n' > "$FIX/visibility.txt"
run_pf geturl "$FIX" "$FIX/source-repository.yaml"
assert_eq "pf get-url: REPO-CANONICAL status" "passed" "$(find_check "$OUT" REPO-CANONICAL status)"
assert_eq "pf get-url: normalized owner/name" "lichman0405/post" "$(find_check "$OUT" REPO-CANONICAL measured)"
assert_eq "pf get-url: source" "fixture" "$(find_check "$OUT" REPO-CANONICAL source)"

# -- no remotes configured (empty remote -v) -> remote_missing
FIX="$WORK/pf-noremote"; mkfix "$FIX" 1 empty
printf 'private\n' > "$FIX/visibility.txt"
run_pf noremote "$FIX" "$FIX/source-repository.yaml"
assert_eq "pf no-remotes: exit code" "1" "$RC"
assert_eq "pf no-remotes: REPO-CANONICAL status" "failed" "$(find_check "$OUT" REPO-CANONICAL status)"
assert_eq "pf no-remotes: reason" "remote_missing" "$(reason_for "$OUT" REPO-CANONICAL)"

# -- no remote input at all (fixture mode runs no real git) -> unverifiable
FIX="$WORK/pf-nocapture"; mkfix "$FIX" 1 none
printf 'private\n' > "$FIX/visibility.txt"
run_pf nocapture "$FIX" "$FIX/source-repository.yaml"
assert_eq "pf no-capture: exit code" "1" "$RC"
assert_eq "pf no-capture: REPO-CANONICAL status" "unknown" "$(find_check "$OUT" REPO-CANONICAL status)"
assert_eq "pf no-capture: reason" "remote_unverifiable" "$(reason_for "$OUT" REPO-CANONICAL)"

# -- --check-visibility uses the gh-visibility fixture, no real gh
FIX="$WORK/pf-gh"; mkfix "$FIX" 1 canon
printf 'private\n' > "$FIX/gh-visibility.txt"
run_pf ghprobe "$FIX" "$FIX/source-repository.yaml" --check-visibility
assert_eq "pf gh probe: VISIBILITY status" "passed" "$(find_check "$OUT" VISIBILITY status)"
assert_eq "pf gh probe: VISIBILITY source" "fixture" "$(find_check "$OUT" VISIBILITY source)"
assert_eq "pf gh probe: measured" "private" "$(find_check "$OUT" VISIBILITY measured)"

# -- --observed-visibility takes precedence over the fixture
FIX="$WORK/pf-flag"; mkfix "$FIX" 1 canon
printf 'public\n' > "$FIX/visibility.txt"
run_pf flag "$FIX" "$FIX/source-repository.yaml" --observed-visibility private
assert_eq "pf flag: VISIBILITY status" "passed" "$(find_check "$OUT" VISIBILITY status)"
assert_eq "pf flag: VISIBILITY source" "flag" "$(find_check "$OUT" VISIBILITY source)"

# -- usage error: invalid --observed-visibility value
FIX="$WORK/pf-usage"; mkfix "$FIX" 1 canon
"$PREFLIGHT" --spec "$FIX/source-repository.yaml" --fixture "$FIX" \
  --observed-visibility bogus --json >"$WORK/pf-usage.json" 2>"$WORK/pf-usage.err"
assert_eq "pf usage error: exit code" "2" "$?"

# -- determinism: same fixture twice -> byte-identical JSON
FIX="$WORK/pf-det"; mkfix "$FIX" 1 canon
printf 'private\n' > "$FIX/visibility.txt"
"$PREFLIGHT" --spec "$FIX/source-repository.yaml" --fixture "$FIX" --json \
  >"$WORK/pf-det1.json" 2>/dev/null
"$PREFLIGHT" --spec "$FIX/source-repository.yaml" --fixture "$FIX" --json \
  >"$WORK/pf-det2.json" 2>/dev/null
if cmp -s "$WORK/pf-det1.json" "$WORK/pf-det2.json"; then
  ok "pf determinism: two --json runs byte-identical"
else
  fail "pf determinism: two --json runs differ"
fi

# ==========================================================================
echo
if [[ "$FAILS" == 0 ]]; then
  echo "ALL UNIT TESTS PASSED"
  exit 0
else
  echo "$FAILS UNIT TEST(S) FAILED"
  exit 1
fi
