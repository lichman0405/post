#!/usr/bin/env bash
# T1204-TEST-01 — `runbook drill` (tests/acceptance/runbook-drill.sh).
#
# A dry run of the deployment, release and backup runbooks, plus the
# rollback / forward-fix scenario executed against a real PostgreSQL.
#
# The acceptance criterion this gate carries is "文档与实际命令一致" — the
# documents name the commands this tree actually has. Three instruments:
#
#   ops/runbook-verify.py    the doc-vs-tree rule engine. It resolves every
#                            command, flag, file, variable and path the corpus
#                            documents against the tree, and re-derives every
#                            claim the corpus makes about what the tree
#                            contains or lacks.
#   ops/runbook-steps.json   the runbooks as an inventory: every section of
#                            docs/35, docs/36 and docs/37 classified by HOW it
#                            is performed. A section nobody has classified is
#                            a finding, so a step that gets added to a runbook
#                            cannot be quietly absent from this drill.
#   ops/runbook-recovery     the rollback / forward-fix scenario, executed.
#
# WHAT THIS GATE IS NOT
#
# It is not a deployment. There is no Dockerfile in this repository, so no
# image exists and nothing can be brought up; `ops/deploy/README.md` § "What is
# missing" names that gap and its owner. What a dry run can honestly do here is
# (a) prove every command the runbooks name resolves, (b) run the instruments
# that exist over the artifacts that exist, and (c) execute the one half of the
# rollback story that does not need a running deployment — the database's
# forward-only rule — against real PostgreSQL. All three are done below, and
# every stage that cannot run is printed with its reason rather than dropped.
#
# It is not the T1203 `deployment smoke` gate either. That gate validates the
# staging template against its own 19 rules; the runbook it belongs to names it
# as its gate. This drill *runs the template's validator* as the executable
# part of the deployment dry run but does not re-run that gate, so the two stay
# independently failable.
#
# MUTATION CHECKS
#
# A checker that cannot fail is not evidence, so seven of the checks below are
# made to fail on purpose (each is printed as "MUTATION CHECK"):
#   1. the engine runs one mutation per rule and each rule must reject its own
#      (ops/runbook-verify.py --selftest);
#   2. that self-test is then made to fail on purpose, by deleting a rule from a
#      copy of the engine — its mutation must be reported as surviving;
#   3. a copy of the deployment runbook that names a script which does not exist
#      is fed to the engine through --override, and must be rejected;
#   4. a copy of the release runbook with an extra, unclassified bullet, and a
#      copy of the deployment runbook with an extra, unclassified HEADING, must
#      both be rejected the same way. Those are the two shapes a step takes in
#      these documents, and the heading case is one this drill found missing:
#      docs/35's section pattern used to cover only its numbered steps and
#      `## 回滚`, so a new `## section` could be added to it invisibly;
#   5. the schema snapshot is checked against a COPY of the migrations with one
#      of them edited, where it must report staleness;
#   6. the backup drill's fail-closed guards are executed for real (a usage
#      error, and an artifact directory inside the working tree that git does
#      not ignore) and must not exit 0;
#   7. the recovery program is handed a stop version that does not exist, where
#      its central assertion must fail rather than pass.
#
# The wiring of those seven into THIS script's exit code cannot be checked here:
# it would mean running the drill inside the drill. It was probed by hand
# instead — an unclassified `9. …` step appended to docs/35 made the engine
# report a finding and this script exit 1 — and the probe is recorded in the
# task's RESULT.json, not in this file. A green run of this script says nothing
# about that wiring on its own.
#
# Runnable on a host with bash + coreutils + python3 + pyyaml + go + make, and a
# reachable PostgreSQL. The Compose parser and the heavy backup/restore drill
# are opt-in (see RUNBOOK_DRILL_DOCKER / RUNBOOK_DRILL_BACKUP below).
#
# Exit codes
#   0  every check passed
#   1  at least one check failed
#   2  a required input, tool or service is missing (counted as a failure,
#      never a silent pass — tests/acceptance/deploy-staging-smoke.sh sets the
#      precedent, and docs/37 does the same for the backup drill)
#   3  usage error
set -u
export LC_ALL=C

usage() {
  cat <<'EOF'
Usage: tests/acceptance/runbook-drill.sh

No arguments. A dry run of the deployment, release and backup runbooks
(docs/35, docs/36, docs/37 and ops/deploy/README.md) against this tree, plus
the rollback / forward-fix scenario run against PostgreSQL.

  -h, --help   this text

There is deliberately no way to select a subset: the stages share one result and
one exit code, and a flag that turned some of them off would be a way to report
PASS having run less. Two stages that need what this host may not have are
opt-in through the environment instead, and each prints what it did not do:

  RUNBOOK_DRILL_DOCKER=1   also hand the staging template to the Compose parser
  RUNBOOK_DRILL_BACKUP=1   also run the heavy backup/restore drill (needs Docker)
  POSTGRES_TEST_ADMIN_URL  the database the recovery scenario creates and drops
                           (default postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post)

Exit codes:
  0  every check passed
  1  at least one check failed
  2  a required input, tool or service is missing (a gate failure, never a
     silent pass)
  3  usage error
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
  -h | --help)
    usage
    exit 0
    ;;
  *)
    printf 'runbook-drill.sh: unknown argument: %s (try --help)\n' "$1" >&2
    exit 3
    ;;
  esac
done

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ENGINE="$ROOT/ops/runbook-verify.py"
INVENTORY="$ROOT/ops/runbook-steps.json"
RECOVERY_DIR="$ROOT/ops/runbook-recovery"
COMPOSE="$ROOT/ops/deploy/docker-compose.staging.yml"
ENVEX="$ROOT/ops/deploy/staging.env.example"
VALIDATOR="$ROOT/ops/deploy/validate-staging-compose.py"
BACKUP_DRILL="$ROOT/ops/backup-restore-drill.sh"
SNAPSHOT_GEN="$ROOT/scripts/gen_schema_snapshot.py"
PG_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"
# PG_URL is never printed below — only its host and port, and the URL's own
# database name where a message needs it. The dev password in the default is
# documented and dev-only, but the habit is the point: this script's output is a
# drill log, and the same log collects a staging host's URL the day someone
# points POSTGRES_TEST_ADMIN_URL at one. ops/runbook-recovery redacts what it
# prints for the same reason.
WORK="$(mktemp -d)"

cleanup() { command rm -rf "$WORK"; }
trap cleanup EXIT INT TERM

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }
note() { printf '     %s\n' "$*"; }

# ---------------------------------------------------------------------------
# 0. inputs, tools, and the one service this drill cannot fake
# ---------------------------------------------------------------------------
for f in "$ENGINE" "$INVENTORY" "$RECOVERY_DIR/main.go" "$COMPOSE" "$ENVEX" \
         "$VALIDATOR" "$BACKUP_DRILL" "$SNAPSHOT_GEN" "$ROOT/Makefile"; do
  if [[ ! -f "$f" ]]; then
    printf 'FAIL missing input %s\n' "${f#"$ROOT"/}"
    printf 'RUNBOOK DRILL RESULT: FAIL (missing input)\n'
    exit 2
  fi
done
for doc in docs/35_DEPLOYMENT_RUNBOOK.md docs/36_RELEASE_RUNBOOK.md docs/37_BACKUP_DR.md \
           ops/deploy/README.md ops/DEV_COMMANDS.md; do
  if [[ ! -f "$ROOT/$doc" ]]; then
    printf 'FAIL missing corpus document %s\n' "$doc"
    printf 'RUNBOOK DRILL RESULT: FAIL (missing input)\n'
    exit 2
  fi
done
ok "inputs present: engine, inventory, recovery program, template, backup drill, runbooks"

if ! command -v python3 >/dev/null 2>&1 || ! python3 -c 'import yaml' >/dev/null 2>&1; then
  printf 'FAIL python3 with pyyaml is required (the deployment template is YAML)\n'
  printf 'RUNBOOK DRILL RESULT: FAIL (missing dependency)\n'
  exit 2
fi
for tool in go make git; do
  command -v "$tool" >/dev/null 2>&1 || {
    printf 'FAIL %s is required\n' "$tool"
    printf 'RUNBOOK DRILL RESULT: FAIL (missing dependency)\n'
    exit 2
  }
done
ok "tools present: python3 + pyyaml, go, make, git"

# PostgreSQL is not optional here. The forward-only rule in docs/35:17 is the
# one part of the rollback story that a dry run CAN execute, and a drill that
# skipped it would be certifying the runbook on the strength of the parts that
# needed nothing. A missing server is therefore exit 2 — a gate failure — not a
# note, in the same way the backup drill treats a skip as a failure (docs/37).
PG_HOST=""
PG_PORT=""
if ! PG_HOST="$(python3 - "$PG_URL" <<'PY'
import sys, urllib.parse
u = urllib.parse.urlparse(sys.argv[1])
print(u.hostname or "", u.port or 5432)
PY
)"; then
  printf 'FAIL cannot parse POSTGRES_TEST_ADMIN_URL\n'
  printf 'RUNBOOK DRILL RESULT: FAIL (bad input)\n'
  exit 2
fi
PG_PORT="${PG_HOST#* }"
PG_HOST="${PG_HOST%% *}"
if ! python3 - "$PG_HOST" "$PG_PORT" <<'PY'
import socket, sys
try:
    socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=5).close()
except OSError as exc:
    print("cannot reach %s:%s: %s" % (sys.argv[1], sys.argv[2], exc), file=sys.stderr)
    sys.exit(1)
PY
then
  printf 'FAIL PostgreSQL is not reachable at %s:%s — the rollback / forward-fix scenario cannot run.\n' \
    "$PG_HOST" "$PG_PORT"
  printf '     This is a gate failure on purpose: the forward-only rule is the part of the\n'
  printf '     runbooks that only a real database can check. Provide one through\n'
  printf '     POSTGRES_TEST_ADMIN_URL (the default is the dev stack on 127.0.0.1:5432), or use\n'
  printf '     the restore drill stack: make infra-up + make infra-init + make migrate.\n'
  printf 'RUNBOOK DRILL RESULT: FAIL (no PostgreSQL)\n'
  exit 2
fi
ok "PostgreSQL reachable at $PG_HOST:$PG_PORT (the scenario below needs it)"

# ---------------------------------------------------------------------------
# 1. the runbooks and the tree agree — the rule engine
# ---------------------------------------------------------------------------
python3 "$ENGINE" --root "$ROOT" >"$WORK/engine.txt" 2>&1
ENGINE_RC=$?
if [[ $ENGINE_RC -eq 0 ]] && grep -q '^runbook-verify: 0 finding(s)' "$WORK/engine.txt"; then
  ok "engine: 0 findings over $(grep -c '^ok   ' "$WORK/engine.txt") rule(s)"
  grep '^ok   ' "$WORK/engine.txt" | sed 's/^/     /'
else
  fail "engine: exit $ENGINE_RC"
  sed 's/^/     /' "$WORK/engine.txt"
fi

python3 "$ENGINE" --root "$ROOT" --selftest >"$WORK/engine-selftest.txt" 2>&1
SELF_RC=$?
REJECTED=$(grep -c '^ok   selftest ' "$WORK/engine-selftest.txt")
if [[ $SELF_RC -eq 0 && "$REJECTED" -ge 9 ]] && \
   grep -q '^selftest: 9 mutation(s) rejected, 0 survived' "$WORK/engine-selftest.txt"; then
  ok "self-test: $REJECTED mutation(s) applied, each rejected by its own rule"
  grep '^ok   selftest ' "$WORK/engine-selftest.txt" | sed 's/^/     /'
else
  fail "self-test: exit $SELF_RC with $REJECTED mutation(s) rejected"
  grep -E '^(FAIL|selftest)' "$WORK/engine-selftest.txt" | sed 's/^/     /'
fi

# The self-test is evidence only if it can fail. Delete one rule from a copy of
# the engine: its mutation must then survive and the run must be non-zero.
if grep -q '^    ("T1 tree-claims", rule_tree_claims),$' "$ENGINE"; then
  sed 's/^    ("T1 tree-claims", rule_tree_claims),$//' "$ENGINE" >"$WORK/engine-norule.py"
  python3 "$WORK/engine-norule.py" --root "$ROOT" --selftest >"$WORK/engine-norule.txt" 2>&1
  NR_RC=$?
  if [[ $NR_RC -ne 0 ]] && grep -q '^FAIL selftest T1 tree-claims: mutation survived' "$WORK/engine-norule.txt"; then
    ok "MUTATION CHECK: deleting a rule makes the self-test fail (exit $NR_RC)"
  else
    fail "MUTATION CHECK: deleting a rule did NOT fail the self-test (exit $NR_RC) — it is vacuous"
    sed 's/^/     /' "$WORK/engine-norule.txt"
  fi
else
  fail "MUTATION CHECK: the T1 anchor is gone from the engine — this check has rotted"
fi

# ... and the same for the engine's real (non-self-test) path, through the same
# command the drill runs above: a corpus document that names a script the tree
# does not have must produce findings.
if sed 's#ops/deploy/validate-staging-compose\.py#ops/deploy/validate-staging-compose.missing.py#g' \
     "$ROOT/ops/deploy/README.md" >"$WORK/readme-mut.md" &&
   ! cmp -s "$ROOT/ops/deploy/README.md" "$WORK/readme-mut.md"; then
  python3 "$ENGINE" --root "$ROOT" \
    --override ops/deploy/README.md="$WORK/readme-mut.md" >"$WORK/engine-mut.txt" 2>&1
  EM_RC=$?
  if [[ $EM_RC -ne 0 ]] && grep -q 'validate-staging-compose.missing.py` — no such file' "$WORK/engine-mut.txt"; then
    ok "MUTATION CHECK: a runbook naming a script that does not exist is caught"
  else
    fail "MUTATION CHECK: the engine accepted a runbook naming a missing script (exit $EM_RC)"
    sed 's/^/     /' "$WORK/engine-mut.txt"
  fi
else
  fail "MUTATION CHECK: the deployment-runbook mutation did not apply — this check has rotted"
fi

# ---------------------------------------------------------------------------
# 2. deployment dry run
# ---------------------------------------------------------------------------
# The executable part of the deployment runbook is the template validator: it
# is what actually inspects ops/deploy/docker-compose.staging.yml, the env
# example and the proxy config, and it is the only deployment instrument in the
# tree that runs without images.
python3 "$VALIDATOR" --compose "$COMPOSE" --env-example "$ENVEX" >"$WORK/template.txt" 2>&1
TPL_RC=$?
if [[ $TPL_RC -eq 0 ]] && grep -q '^VALIDATE RESULT: PASS' "$WORK/template.txt"; then
  ok "deployment template: $(grep -c '^ok   \[' "$WORK/template.txt") rules, 0 findings"
else
  fail "deployment template: exit $TPL_RC"
  sed 's/^/     /' "$WORK/template.txt"
fi

python3 "$VALIDATOR" --compose "$COMPOSE" --env-example "$ENVEX" --selftest \
  >"$WORK/template-selftest.txt" 2>&1
TS_RC=$?
TS_MUT=$(grep -c '^ok   selftest' "$WORK/template-selftest.txt")
if [[ $TS_RC -eq 0 && "$TS_MUT" -ge 19 ]]; then
  ok "deployment template self-test: $TS_MUT mutations, each rejected by its own rule"
else
  fail "deployment template self-test: exit $TS_RC with $TS_MUT mutation(s) rejected"
  grep -E '^(FAIL|VALIDATE)' "$WORK/template-selftest.txt" | sed 's/^/     /'
fi

# The deployment order itself, as the runbooks give it: every step of docs/35
# §顺序 and §回滚 is classified in the inventory, and the drill prints what
# each one resolved to. This is the dry run a person reads; the classifications
# themselves are checked by the engine (rule D6), not here.
python3 - "$INVENTORY" <<'PY'
import json, sys
inv = json.load(open(sys.argv[1], encoding="utf-8"))
order = [s for s in inv["steps"] if s["doc"] == "docs/35_DEPLOYMENT_RUNBOOK.md"]
print("docs/35 deployment order, %d step(s), as classified in ops/runbook-steps.json:" % len(order))
for s in order:
    cmd = s.get("command") or "-"
    print("  %-26s %-18s %s" % (s["id"], s["kind"], cmd))
PY

if [[ "${RUNBOOK_DRILL_DOCKER:-0}" == "1" ]]; then
  note "RUNBOOK_DRILL_DOCKER=1: handing the template to the real Compose parser"
  python3 "$VALIDATOR" --compose "$COMPOSE" --env-example "$ENVEX" --with-docker \
    >"$WORK/template-docker.txt" 2>&1
  TD_RC=$?
  if grep -q '^skip docker compose config' "$WORK/template-docker.txt"; then
    note "$(grep '^skip docker compose config' "$WORK/template-docker.txt")"
    note "not counted as a pass and not counted as a failure: the parser was unavailable"
  elif [[ $TD_RC -eq 0 ]] && grep -q '^ok   docker compose config' "$WORK/template-docker.txt"; then
    ok "docker compose config accepted the template"
  else
    fail "docker compose config rejected the template (exit $TD_RC)"
    sed 's/^/     /' "$WORK/template-docker.txt"
  fi
else
  note "NOT RUN: the real Compose parser (docker compose config) and every documented"
  note "  compose subcommand. There is no Dockerfile in this repository, so no image exists"
  note "  and nothing can be started; the Compose binary's availability also varies by host,"
  note "  and a gate whose verdict depends on that is a flaky gate. The resolution of each"
  note "  documented compose command is checked by the engine (rule D4) instead."
  note "  Set RUNBOOK_DRILL_DOCKER=1 to add the parser as well."
fi

# ---------------------------------------------------------------------------
# 3. release dry run
# ---------------------------------------------------------------------------
# The release runbook's checkable half is the migration/schema one: the
# migrations are the canonical history and specs/database/postgres.sql is their
# generated snapshot, so "migration 完成" is a claim about this tree that
# `make check-schema-snapshot` decides.
(cd "$ROOT" && make check-schema-snapshot) >"$WORK/snapshot.txt" 2>&1
SNP_RC=$?
if [[ $SNP_RC -eq 0 ]]; then
  ok "release: make check-schema-snapshot — the snapshot is the ordered migrations"
else
  fail "release: make check-schema-snapshot exited $SNP_RC"
  sed 's/^/     /' "$WORK/snapshot.txt"
fi

# ... and proof that check can fail. The generator's --root exists so that the
# whole mechanism is fixture-testable without touching the real tree, which is
# what makes this mutation safe: the copy is edited, the repository is not.
mkdir -p "$WORK/fixture/infra" "$WORK/fixture/specs/database"
cp -r "$ROOT/infra/migrations" "$WORK/fixture/infra/"
cp "$ROOT/specs/database/postgres.sql" "$WORK/fixture/specs/database/"
python3 "$SNAPSHOT_GEN" --check --root "$WORK/fixture" >"$WORK/fixture-clean.txt" 2>&1
FIX_CLEAN=$?
LAST_MIG="$(command ls "$WORK/fixture/infra/migrations"/*.sql | tail -1)"
printf '\nALTER TABLE projects ADD COLUMN t1204_drill_marker text;\n' >>"$LAST_MIG"
python3 "$SNAPSHOT_GEN" --check --root "$WORK/fixture" >"$WORK/fixture-mut.txt" 2>&1
FIX_MUT=$?
if [[ $FIX_CLEAN -ne 0 ]]; then
  fail "MUTATION CHECK: the untouched fixture already reports staleness (exit $FIX_CLEAN) — this check is unusable"
  sed 's/^/     /' "$WORK/fixture-clean.txt"
elif [[ $FIX_MUT -ne 0 ]] && grep -q 'STALE' "$WORK/fixture-mut.txt"; then
  ok "MUTATION CHECK: an edited migration makes the snapshot check report STALE (exit $FIX_MUT)"
else
  fail "MUTATION CHECK: an edited migration did NOT make the snapshot check fail (exit $FIX_MUT)"
  sed 's/^/     /' "$WORK/fixture-mut.txt"
fi

# The release runbook's security-scan half, as the repository performs it today.
timeout 600 go test ./internal/config -run TestRepoExampleFilesAreSecretFree -count=1 \
  >"$WORK/secretscan.txt" 2>&1
SC_RC=$?
if [[ $SC_RC -eq 0 ]]; then
  ok "release: security scan — committed *.env.example files are placeholder-only"
else
  fail "release: security scan exited $SC_RC"
  tail -12 "$WORK/secretscan.txt" | sed 's/^/     /'
fi

# The inventory is what makes "Changelog、migration、feature flags、security
# scan 完成" answerable at all: the release runbook names no command for most of
# it, and the inventory says so, in class and in note. A section added to
# docs/36 without an entry must be a finding — this is how a new release step
# gets noticed instead of silently skipping the drill.
{ cat "$ROOT/docs/36_RELEASE_RUNBOOK.md"; printf -- '- 新步骤：T1204 演练。\n'; } >"$WORK/mut36.md"
python3 "$ENGINE" --root "$ROOT" --override docs/36_RELEASE_RUNBOOK.md="$WORK/mut36.md" \
  >"$WORK/engine-36.txt" 2>&1
E36_RC=$?
if [[ $E36_RC -ne 0 ]] && grep -q 'is not classified in ops/runbook-steps.json' "$WORK/engine-36.txt"; then
  ok "MUTATION CHECK: an unclassified release-runbook bullet is caught"
else
  fail "MUTATION CHECK: an unclassified release-runbook bullet was accepted (exit $E36_RC)"
  sed 's/^/     /' "$WORK/engine-36.txt"
fi

# The other shape a step takes here, and the one that was NOT caught before this
# drill existed: a new `## heading` in docs/35. Its section pattern used to list
# the numbered steps and `## 回滚` one by one, so anything else in that document
# was invisible to the inventory — an unclassified section could be added and
# every check stayed green. The pattern now covers `## ` headings and the
# inventory declares the one structural heading (## 顺序) it should skip.
{ cat "$ROOT/docs/35_DEPLOYMENT_RUNBOOK.md"; printf '\n## 临时新章节探针\n'; } >"$WORK/mut35.md"
python3 "$ENGINE" --root "$ROOT" --override docs/35_DEPLOYMENT_RUNBOOK.md="$WORK/mut35.md" \
  >"$WORK/engine-35.txt" 2>&1
E35_RC=$?
if [[ $E35_RC -ne 0 ]] && grep -q "section '## 临时新章节探针' is not classified" "$WORK/engine-35.txt"; then
  ok "MUTATION CHECK: an unclassified deployment-runbook heading is caught"
else
  fail "MUTATION CHECK: an unclassified deployment-runbook heading was accepted (exit $E35_RC)"
  sed 's/^/     /' "$WORK/engine-35.txt"
fi

python3 - "$INVENTORY" <<'PY'
import collections, json, sys
inv = json.load(open(sys.argv[1], encoding="utf-8"))
by_kind = collections.Counter(s["kind"] for s in inv["steps"])
print("runbook steps by class (%d total): %s"
      % (sum(by_kind.values()), ", ".join("%s=%d" % kv for kv in sorted(by_kind.items()))))
print("steps with no command in this tree today — these are what the dry runs above did NOT do:")
for s in inv["steps"]:
    if not s.get("command"):
        print("  %-30s %-18s %s" % (s["id"], s["kind"], s["note"].split(".")[0]))
PY

# ---------------------------------------------------------------------------
# 4. backup dry run
# ---------------------------------------------------------------------------
"$BACKUP_DRILL" --help >"$WORK/backup-help.txt" 2>&1
BH_RC=$?
if [[ $BH_RC -eq 0 ]] && grep -q 'Exit codes:' "$WORK/backup-help.txt"; then
  ok "backup: ops/backup-restore-drill.sh --help exits 0 and documents its exit codes"
else
  fail "backup: --help exited $BH_RC"
  tail -12 "$WORK/backup-help.txt" | sed 's/^/     /'
fi

# The guards, executed. Each one is a fail-closed path: run it and require that
# it refuses. These cost nothing and are the only part of the heavy drill that
# can be exercised without Docker.
"$BACKUP_DRILL" --artifacts >"$WORK/backup-usage.txt" 2>&1
BU_RC=$?
if [[ $BU_RC -eq 3 ]]; then
  ok "backup: a usage error exits 3, not 0 ($(head -1 "$WORK/backup-usage.txt"))"
else
  fail "backup: a usage error exited $BU_RC, want 3"
  sed 's/^/     /' "$WORK/backup-usage.txt"
fi

"$BACKUP_DRILL" --no-infra --artifacts "$ROOT/ops/unignored-artifacts" \
  >"$WORK/backup-unignored.txt" 2>&1
BI_RC=$?
if [[ $BI_RC -eq 5 ]] && grep -q 'NOT ignore' "$WORK/backup-unignored.txt"; then
  ok "backup: an artifact directory inside the tree that git does not ignore is refused (exit 5)"
else
  fail "backup: the unignored-artifacts guard exited $BI_RC, want 5 — a dump could reach a commit"
  sed 's/^/     /' "$WORK/backup-unignored.txt"
fi
if [[ -e "$ROOT/ops/unignored-artifacts" ]]; then
  fail "backup: the refused artifact directory was created anyway — the guard runs too late"
fi

# The default artifact directory is the one the drill will actually use, so the
# ignore rule it depends on is asserted directly rather than inferred.
if git -C "$ROOT" check-ignore -q -- "$ROOT/.backup-dr/drill-t1204-probe"; then
  ok "backup: the default artifact directory .backup-dr/ is git-ignored"
else
  fail "backup: .backup-dr/ is not git-ignored — the drill's default would be committable"
fi

if [[ "${RUNBOOK_DRILL_BACKUP:-0}" == "1" ]]; then
  note "RUNBOOK_DRILL_BACKUP=1: running the heavy backup/restore drill (this needs Docker)"
  "$BACKUP_DRILL" >"$WORK/backup-heavy.txt" 2>&1
  HB_RC=$?
  case $HB_RC in
  0) ok "backup: the restore drill reported PASS" ;;
  2) fail "backup: the restore drill SKIPPED — a gate failure on purpose (docs/37)"
     tail -20 "$WORK/backup-heavy.txt" | sed 's/^/     /' ;;
  *) fail "backup: the restore drill exited $HB_RC"
     tail -20 "$WORK/backup-heavy.txt" | sed 's/^/     /' ;;
  esac
else
  note "NOT RUN: the heavy backup/restore drill itself (ops/backup-restore-drill.sh with no"
  note "  flags). It brings up the dev stack and needs a Docker daemon, which this host does"
  note "  not provide to the drill; the empty-environment restore is the subject of the"
  note "  'restore drill' gate (tasks/tests.json T1110-TEST-01), not of this one. What is"
  note "  checked here is what that drill's own flags and guards promise."
  note "  Set RUNBOOK_DRILL_BACKUP=1 to run it as well."
fi

# ---------------------------------------------------------------------------
# 5. the rollback / forward-fix scenario, executed
# ---------------------------------------------------------------------------
# First, what is WRITTEN DOWN, because it is the half that rots quietly.
# ops/deploy/README.md § Rollback says the migration files carry goose
# `-- +goose Down` sections and that none of them is reachable. The engine's
# rule T2 re-derives the second half from the source; the first half is
# measured here, so the sentence cannot go on describing a tree that no longer
# has any — and, in the other direction, a paragraph that quietly claimed there
# was nothing to reach would be caught by a reader who opened a migration file.
python3 - "$ROOT" >"$WORK/down-sections.txt" 2>&1 <<'PY'
import sys
from pathlib import Path
files = sorted((Path(sys.argv[1]) / "infra/migrations").glob("*.sql"))
with_down, executable = [], []
for p in files:
    body = p.read_text(encoding="utf-8")
    if "-- +goose Down" not in body:
        continue
    with_down.append(p.name)
    stmts = [l for l in body.split("-- +goose Down", 1)[1].splitlines()
             if l.strip() and not l.strip().startswith("--")]
    if stmts:
        executable.append(p.name)
print("forward-only, measured: %d of %d migration files carry a '-- +goose Down' section, "
      "%d of them with executable SQL" % (len(with_down), len(files), len(executable)))
print("  with executable SQL: %s" % ", ".join(executable))
if not executable:
    print("  FAIL: ops/deploy/README.md § Rollback describes migration files that carry Down")
    print("        sections with executable SQL. None does. That paragraph has to be revisited,")
    print("        not left describing a tree that no longer exists.")
    sys.exit(1)
PY
DOWN_RC=$?
sed 's/^/     /' "$WORK/down-sections.txt"
if [[ $DOWN_RC -eq 0 ]]; then
  ok "forward-only: $(head -1 "$WORK/down-sections.txt" | sed 's/^forward-only, measured: //')"
else
  fail "forward-only: no migration file carries an executable Down section any more — ops/deploy/README.md § Rollback now describes nothing (exit $DOWN_RC)"
fi

# Second, what can actually RUN. docs/35:17 — "应用可回滚 previous image；DB 只
# forward repair". The application half needs a running deployment, which needs
# images, which do not exist. The database half needs only PostgreSQL, so it is
# done rather than described: ops/runbook-recovery pins a scratch database at an
# older schema
# version, repairs it forward to head, re-runs the migrate job at head, and
# then attempts a rollback below head and requires that it move nothing.
#
# Built rather than `go run`, because the drill has to read its exit code and
# `go run` collapses every non-zero exit into 1.
if ! (cd "$ROOT" && go build -o "$WORK/runbook-recovery" ./ops/runbook-recovery) \
     >"$WORK/recovery-build.txt" 2>&1; then
  fail "recovery: the program does not build"
  sed 's/^/     /' "$WORK/recovery-build.txt"
else
  ok "recovery: ops/runbook-recovery builds"
  "$WORK/runbook-recovery" --admin-url "$PG_URL" >"$WORK/recovery.txt" 2>&1
  REC_RC=$?
  if [[ $REC_RC -eq 0 ]] && grep -q 'runbook-recovery: PASS' "$WORK/recovery.txt"; then
    ok "recovery: $(grep -c -v '^runbook-recovery:' "$WORK/recovery.txt") step(s), the scenario reproduced the runbook"
    grep -v '^runbook-recovery:' "$WORK/recovery.txt" | sed 's/^/     /'
  else
    fail "recovery: exit $REC_RC"
    sed 's/^/     /' "$WORK/recovery.txt"
  fi

  # ... and proof that scenario's central assertion can fail: a stop version
  # that no migration reaches must be rejected, not accepted. Without this, a
  # program that printed PASS unconditionally would pass the check above too.
  "$WORK/runbook-recovery" --admin-url "$PG_URL" --stop-version 999999 \
    >"$WORK/recovery-nc.txt" 2>&1
  NC_RC=$?
  if [[ $NC_RC -eq 1 ]] && grep -q 'runbook-recovery: FAIL' "$WORK/recovery-nc.txt"; then
    ok "MUTATION CHECK: a stop version beyond head fails the scenario (exit $NC_RC): $(grep '^FAIL ' "$WORK/recovery-nc.txt")"
  else
    fail "MUTATION CHECK: a stop version beyond head did not fail the scenario (exit $NC_RC, want 1)"
    sed 's/^/     /' "$WORK/recovery-nc.txt"
  fi

  # The scratch databases are dropped on every path, including that failing one.
  leftovers="$(python3 - "$PG_URL" <<'PY'
import sys, urllib.parse, subprocess
u = urllib.parse.urlparse(sys.argv[1])
env = {"PGPASSWORD": u.password or "", "PATH": "/usr/bin:/bin"}
try:
    out = subprocess.run(
        ["psql", "-h", u.hostname or "localhost", "-p", str(u.port or 5432),
         "-U", u.username or "postgres", "-d", u.path.lstrip("/") or "postgres",
         "-Atc", "select datname from pg_database where datname like 'test_T1204_%'"],
        capture_output=True, text=True, env=env, timeout=30).stdout
except Exception as exc:
    print("PROBE-FAILED %s" % exc)
    sys.exit(0)
print("\n".join(l for l in out.splitlines() if l.strip()))
PY
)"
  if [[ "$leftovers" == PROBE-FAILED* ]]; then
    note "NOT RUN: the leftover-database probe — psql could not be run (${leftovers#PROBE-FAILED })"
  elif [[ -z "$leftovers" ]]; then
    ok "recovery: no test_T1204_* database was left behind"
  else
    fail "recovery: scratch databases left behind: $(printf '%s' "$leftovers" | tr '\n' ' ')"
  fi
fi

# ---------------------------------------------------------------------------
printf '\nrunbook drill: %d failure(s)\n' "$FAILS"
if [[ "$FAILS" -gt 0 ]]; then
  printf 'RUNBOOK DRILL RESULT: FAIL\n'
  exit 1
fi
printf 'RUNBOOK DRILL RESULT: PASS\n'
exit 0
