#!/usr/bin/env bash
#
# ops/backup-restore-drill.sh — POST backup / restore drill (docs/37_BACKUP_DR.md, task T1110).
#
# What this is. docs/37 §需要备份 names four classes to back up — Postgres,
# S3 blobs/manifests, Gitea repositories, critical secrets/config METADATA —
# and §一致性 asks for a reconciliation over four axes, DB ↔ Git refs ↔ blob
# hashes ↔ release manifests. §V1 验收 (docs/37:10) is the hard one: at least
# one EMPTY-ENVIRONMENT restore, and Seed Project, Release, Asset, Files and
# Evidence Graph must ALL open on the restored environment.
#
# Split of responsibilities:
#   * the drill itself            cmd/api/backupdr/            (Go, the product code)
#   * the run that proves it      tests/integration/restore_drill_test.go
#                                 (`restore drill`, tasks/tests.json T1110-TEST-01)
#   * this script                 the G3 gate: a real stack underneath that run
#
# What this is NOT. docs/37 §RPO/RTO puts this round's acceptance at
# "V1 staging 先验证流程" — that the PROCESS runs end to end — and explicitly
# does not make it RPO 24h / RTO 4h. This script measures no RPO and no RTO,
# prints none, and asserts none.
#
# The empty environment. `make infra-up` + `make infra-init` + `make migrate`
# ARE the documented way to build one (Makefile:236-243, ops/DEV_COMMANDS.md
# §本地基础设施), and this script runs exactly those — it does not invent a
# second way to start an environment. The environment the drill restores INTO
# is then a run-scoped empty database, a bucket the object store does not have,
# and a Gitea organization that does not exist; the drill PROVES each of those
# empty before it writes anything (report.json → restore.emptiness_checks)
# rather than assuming it.
#
# A SKIP is a FAILURE here. `go test` exits 0 when a test skips, so a gate that
# checked only the exit code would go green on a machine with no Docker daemon
# at all. This gate requires the run's own PASS line, and a SKIP — the loud,
# visible skip tests/integration/git_reconciliation_test.go:18-24 establishes —
# is reported with its reason and a non-zero exit.
#
# Artifacts never enter the repository. The drill writes a database dump, blob
# copies and git mirrors; this script points it at .backup-dr/ (the drill's
# ignored default, .gitignore), and refuses to run if a caller aims the
# artifact directory anywhere inside the working tree that git does not ignore.
# Nothing under it is ever committed.
set -u
export LC_ALL=C

readonly SCRIPT_NAME="ops/backup-restore-drill.sh"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# The subtests one run must report. It is a floor, not a target: the nine
# below are what this task's acceptance criteria are carried by, and a run
# that reports fewer has had something removed from it. Adding a subtest
# keeps this green; deleting or merging one has to be argued for.
readonly MIN_SUBTESTS=9

NO_INFRA=0
ARTIFACTS_DIR=""
TIMEOUT="15m"

usage() {
  cat <<'EOF'
Usage: ops/backup-restore-drill.sh [--no-infra] [--artifacts DIR] [--timeout DUR] [-h|--help]

POST backup / restore drill (docs/37_BACKUP_DR.md). Run from anywhere in the repo.

  (no flags)         bring the dev stack up (make infra-up + infra-init + migrate),
                     then run the `restore drill` test against it
  --no-infra         skip the stack bring-up: the documented stack is already
                     up and migrated (useful when a service container provides it)
  --artifacts DIR    where the dump, the blob copies, the git mirrors and
                     report.json go. Default: <repo>/.backup-dr/drill-<run id>,
                     which .gitignore covers. A DIR inside the working tree that
                     git does not ignore is refused, because a backup artifact
                     must never be one `git add -A` from a commit.
  --timeout DUR      go test -timeout (default 15m; the test's own context is
                     10m, and Go's default panic timeout is also 10m)
  -h, --help         this text

Exit codes:
  0  the drill ran and reported PASS
  1  the drill FAILED
  2  the drill SKIPPED — a dependency of the heavy test was missing. The skip
     reason is printed. This is a gate failure on purpose: a heavy test that
     turned green without its services would certify nothing.
  3  usage error
  4  the stack could not be brought up (make infra-up / infra-init / migrate)
  5  the artifact directory is inside the working tree and not gitignored
EOF
}

# log writes one line to stderr so stdout stays the drill's own output.
log() { printf '%s %s\n' "$SCRIPT_NAME:" "$*" >&2; }
step() { printf '\n=== %s\n' "$*" >&2; }
die() { local code="$1"; shift; log "$*"; exit "$code"; }

while [ $# -gt 0 ]; do
  case "$1" in
  -h | --help)
    usage
    exit 0
    ;;
  --no-infra)
    NO_INFRA=1
    shift
    ;;
  --artifacts)
    [ $# -ge 2 ] || die 3 "--artifacts needs a directory"
    ARTIFACTS_DIR="$2"
    shift 2
    ;;
  --timeout)
    [ $# -ge 2 ] || die 3 "--timeout needs a duration"
    TIMEOUT="$2"
    shift 2
    ;;
  *)
    die 3 "unknown argument: $1 (try --help)"
    ;;
  esac
done

for tool in go git; do
  command -v "$tool" >/dev/null 2>&1 || die 4 "$tool is not on PATH (see ops/DEV_COMMANDS.md)"
done

RUN_ID="$(date -u +%Y%m%dt%H%M%SZ)"
[ -n "$ARTIFACTS_DIR" ] || ARTIFACTS_DIR="$ROOT/.backup-dr/drill-$RUN_ID"

# Canonicalise the path BEFORE the guard below, because the guard compares it
# against $ROOT — which is absolute — and a RELATIVE --artifacts
# (./cmd/api/artifacts) never matches that pattern while pointing straight
# into the working tree. realpath -m resolves . and .. without requiring the
# path to exist, so the check still runs before anything is created.
if command -v realpath >/dev/null 2>&1; then
  ARTIFACTS_DIR="$(realpath -m -- "$ARTIFACTS_DIR")" || die 4 "cannot resolve the artifact directory $ARTIFACTS_DIR"
else
  # No realpath: refuse a relative path rather than let it through unchecked.
  case "$ARTIFACTS_DIR" in
  /*) ;;
  *) die 5 "the artifact directory $ARTIFACTS_DIR is not absolute and realpath is not installed to resolve it" ;;
  esac
fi

# The location is checked BEFORE anything is created in it: refusing after a
# mkdir would leave the very directory it refused sitting in the working tree.
# `git check-ignore` answers about paths that do not exist yet, which is the
# whole point of asking it first.
case "$ARTIFACTS_DIR" in
"$ROOT"/* | "$ROOT")
  git -C "$ROOT" check-ignore -q -- "$ARTIFACTS_DIR" ||
    die 5 "the artifact directory $ARTIFACTS_DIR is inside the working tree and git does NOT ignore it: " \
      "a dump, blob copies and a git mirror must never be committable. Use the default .backup-dr/, " \
      "or a directory outside the repository"
  ;;
esac

mkdir -p "$ARTIFACTS_DIR" || die 4 "cannot create the artifact directory $ARTIFACTS_DIR"
ARTIFACTS_DIR="$(cd "$ARTIFACTS_DIR" && pwd)"

# postgres_url prints a URL with the credential removed. The dev stack's
# password is a documented dev-only default, but the habit is the point: this
# script prints URLs, and a URL with a password in it ends up in logs.
redacted() { printf '%s' "$1" | sed -E 's#(://[^:/@]+):[^@]*@#\1@#'; }

POSTGRES_TEST_ADMIN_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"
POST_BLOB_ENDPOINT="${POST_BLOB_ENDPOINT:-http://127.0.0.1:9000}"
GITEA_BASE="${POST_GITEA_BASE_URL:-http://127.0.0.1:3000}"
export POSTGRES_TEST_ADMIN_URL POST_BLOB_ENDPOINT
# The drill writes where this script decided, not into the test's temp dir.
export POST_DRILL_ARTIFACTS="$ARTIFACTS_DIR"

step "backup / restore drill — run $RUN_ID"
log "repo:       $ROOT"
log "artifacts:  $ARTIFACTS_DIR"
log "postgres:   $(redacted "$POSTGRES_TEST_ADMIN_URL")"
log "blob store: $POST_BLOB_ENDPOINT"
log "gitea:      $GITEA_BASE"
log "This run demonstrates the PROCESS (docs/37 §RPO/RTO: V1 先验证流程). No RPO or RTO is measured or claimed."

if [ "$NO_INFRA" -eq 0 ]; then
  step "1/2  the documented empty-environment path: make infra-up + infra-init + migrate"
  for target in infra-up infra-init migrate; do
    log "make $target"
    make -C "$ROOT" "$target" || die 4 "make $target failed — the drill needs a real PostgreSQL, MinIO and Gitea"
  done
else
  step "1/2  --no-infra: using the stack that is already up"
  log "skipped make infra-up / infra-init / migrate at the caller's request"
fi

step "2/2  the blocking test: restore drill"
log "go test ./tests/integration -run '^TestRestoreDrill\$' -count=1 -v -timeout $TIMEOUT"
TEST_LOG="$ARTIFACTS_DIR/go-test.log"
# tee keeps the log AND shows the run; the status that matters is the left
# side of the pipe, so it is read out of PIPESTATUS rather than inferred from
# the pipeline's own (tee's) exit code. `set -u` is the only shell option in
# force here — nothing aborts the script before the log has been examined.
(cd "$ROOT" && go test ./tests/integration -run '^TestRestoreDrill$' -count=1 -v -timeout "$TIMEOUT") 2>&1 | tee "$TEST_LOG"
GO_STATUS="${PIPESTATUS[0]}"

if grep -q -- "--- PASS: TestRestoreDrill" "$TEST_LOG"; then
  :
elif grep -q -- "--- SKIP: TestRestoreDrill" "$TEST_LOG"; then
  log ""
  log "the drill SKIPPED — a dependency of the heavy test is missing. Why:"
  # The reason is logged BEFORE the result line (that is where t.Skipf's own
  # output lands), so the block from the test's start to the SKIP line is what
  # has to be shown; grepping after it would print the surrounding PASS/ok
  # lines and call that an explanation.
  awk '/^=== RUN   TestRestoreDrill$/{f=1} f{print} /^--- SKIP: TestRestoreDrill/{exit}' "$TEST_LOG" |
    head -40 | sed 's/^/    /' >&2
  log ""
  log "exit 2: a skipped drill is a gate failure. Bring the stack up (drop --no-infra) and re-run."
  exit 2
else
  log "the drill did not report PASS (go test exit $GO_STATUS); see $TEST_LOG"
  exit 1
fi
[ "$GO_STATUS" -eq 0 ] || die 1 "go test exited $GO_STATUS while also reporting PASS — treating the run as failed"

# The gates the acceptance criteria are carried by, read back out of the run's
# own output rather than inferred from the exit status.
SUBTESTS="$(grep -c -- "^    --- PASS: TestRestoreDrill/" "$TEST_LOG")"
if [ "$SUBTESTS" -lt "$MIN_SUBTESTS" ]; then
  die 1 "the run reported $SUBTESTS passing subtests, fewer than the $MIN_SUBTESTS this task's acceptance " \
    "criteria are carried by — if a subtest was removed or merged, that is a weakening of the drill " \
    "(CLAUDE.md §5.1) and has to be argued for, not silently allowed"
fi
log "subtests passed: $SUBTESTS"

# The report the drill wrote, read back for the reader. Every field printed
# here is one an acceptance criterion names.
REPORT="$ARTIFACTS_DIR/report.json"
if command -v python3 >/dev/null 2>&1 && [ -f "$REPORT" ]; then
  step "the run's report ($REPORT)"
  python3 - "$REPORT" <<'PY' || log "could not summarise the report; read $REPORT directly"
import json, sys

r = json.load(open(sys.argv[1]))
restore = r.get("restore") or {}
rec = r.get("reconciliation") or {}
scan = r.get("secret_scan") or {}

print("  snapshot timestamp   %s" % r.get("snapshot_timestamp"))
print("  restored             %s tables, %s rows, %s blobs, %s repositories" % (
    restore.get("tables_restored"), restore.get("rows_restored"),
    restore.get("blobs_restored"), restore.get("git_repositories_restored")))
for c in restore.get("emptiness_checks") or []:
    print("  proven empty         %-12s %s" % (c.get("class"), c.get("observed")))
print("  axes checked         %s" % ", ".join("%s=%s" % (k, v) for k, v in sorted((rec.get("checked") or {}).items())))
print("  reconciliation       %s (%d findings, %d audit rows recorded, %d repairs applied)" % (
    rec.get("verdict"), len(rec.get("findings") or []), r.get("findings_recorded"), r.get("repairs_applied")))
for o in (r.get("opened") or {}).get("objects") or []:
    print("  opened               %-16s %s  (%s)" % (o.get("kind"), "yes" if o.get("opened") else "NO", o.get("via")))
print("  credential scan      searched %s declared values (%s too short to search), "
      "%s artifact files carried one" % (
          scan.get("values_searched"), scan.get("values_skipped_too_short"), scan.get("hits")))
print("  round acceptance     %s" % r.get("round_acceptance"))
print("  rpo claim            %r" % (r.get("rpo_claim"),))
PY
fi

step "result"
log "PASS — the drill ran end to end on a real stack"
log "report:    $REPORT"
log "log:       $TEST_LOG"
log "artifacts: $ARTIFACTS_DIR (gitignored; never committed)"
log "No RPO/RTO was measured, asserted or claimed. docs/37 §RPO/RTO makes V1's acceptance the process."
exit 0
