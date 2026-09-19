#!/usr/bin/env bash
# T1203-TEST-01 — `deployment smoke` (tests/acceptance/deploy-staging-smoke.sh).
#
# What this gate is: a static smoke of the staging deployment template —
# ops/deploy/docker-compose.staging.yml, ops/deploy/staging.env.example and
# ops/deploy/reverse-proxy/nginx.conf. It proves the three files agree with
# each other and with the specifications the deployment is required to follow,
# that the template is fail-closed, and that every rule doing the checking can
# itself fail.
#
# What this gate is NOT: a deployment. There is no Dockerfile in this
# repository, so no application image exists and nothing can be brought up. A
# test that claimed "staging deploys" here would be measuring nothing. The
# deployment-shaped checks that need a real stack (docs/35_DEPLOYMENT_RUNBOOK
# steps 5-8 — real requests through TLS against a migrated database) stay with
# the real-services gates and with whoever lands the images.
# ops/deploy/README.md § "What is missing" names that gap and its owner.
#
# Four of the checks below are mutation-checked, because a checker that cannot
# fail is not evidence:
#   - the template validator runs one mutation per rule and requires each rule
#     to reject its own (validate-staging-compose.py --selftest);
#   - that self-test is then made to fail on purpose, by deleting a rule from a
#     copy of the validator — its mutation must be reported as surviving;
#   - the runbook's "every path it names exists" check is fed a path that does
#     not exist;
#   - the repository's own secret scanner is pointed at a planted secret and
#     must name the file it is in.
#
# Runnable on a host with bash + coreutils + python3 + pyyaml + go.
#
# Exit codes
#   0  every check passed
#   1  at least one check failed
#   2  a required input or tool is missing (counted as a failure, never a
#      silent pass — the backup drill sets the same precedent, docs/37)
#   3  usage error
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
COMPOSE="$ROOT/ops/deploy/docker-compose.staging.yml"
ENVEX="$ROOT/ops/deploy/staging.env.example"
PROXY="$ROOT/ops/deploy/reverse-proxy/nginx.conf"
RUNBOOK="$ROOT/ops/deploy/README.md"
VALIDATOR="$ROOT/ops/deploy/validate-staging-compose.py"
WORK="$(mktemp -d)"

# The planted file the secret-scan mutation check writes — inside the real
# repository on purpose, because that is the tree the scanner sweeps and a
# copy would prove nothing about the sweep's coverage. Removed on every exit
# path including signals; named so that a stray copy is unmistakable.
PLANTED="$ROOT/ops/deploy/staging.planted-check.env.example"
cleanup() {
  command rm -f "$PLANTED"
  command rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }
note() { printf '     %s\n' "$*"; }

# ---------------------------------------------------------------------------
# 0. inputs and tools
# ---------------------------------------------------------------------------
for f in "$COMPOSE" "$ENVEX" "$PROXY" "$RUNBOOK" "$VALIDATOR"; do
  if [[ ! -f "$f" ]]; then
    printf 'FAIL missing input %s\n' "${f#"$ROOT"/}"
    printf 'SMOKE TEST RESULT: FAIL (missing input)\n'
    exit 2
  fi
done
ok "inputs present: compose, env example, nginx config, runbook, validator"

if ! command -v python3 >/dev/null 2>&1 || ! python3 -c 'import yaml' >/dev/null 2>&1; then
  printf 'FAIL python3 with pyyaml is required (the template is YAML)\n'
  printf 'SMOKE TEST RESULT: FAIL (missing dependency)\n'
  exit 2
fi
if ! command -v go >/dev/null 2>&1; then
  printf 'FAIL go is required (the repository secret scan is a Go test)\n'
  printf 'SMOKE TEST RESULT: FAIL (missing dependency)\n'
  exit 2
fi
ok "tools present: python3 + pyyaml, go"

# ---------------------------------------------------------------------------
# 1. the rules, and proof that every rule can fail
# ---------------------------------------------------------------------------
python3 "$VALIDATOR" --compose "$COMPOSE" --env-example "$ENVEX" >"$WORK/plain.txt" 2>&1
PLAIN_RC=$?
if [[ $PLAIN_RC -eq 0 ]] && grep -q '^VALIDATE RESULT: PASS' "$WORK/plain.txt"; then
  ok "template rules: $(grep -c '^ok   \[' "$WORK/plain.txt") rules, 0 findings"
else
  fail "template rules: exit $PLAIN_RC"
  sed 's/^/     /' "$WORK/plain.txt"
fi

python3 "$VALIDATOR" --compose "$COMPOSE" --env-example "$ENVEX" --selftest >"$WORK/selftest.txt" 2>&1
SELF_RC=$?
MUTANTS=$(grep -c '^ok   selftest' "$WORK/selftest.txt")
if [[ $SELF_RC -eq 0 && "$MUTANTS" -ge 19 ]]; then
  ok "self-test: $MUTANTS mutations applied, each rejected by its own rule"
else
  fail "self-test: exit $SELF_RC with $MUTANTS mutation(s) rejected"
  grep -E '^(FAIL|VALIDATE)' "$WORK/selftest.txt" | sed 's/^/     /'
fi

# The self-test above is evidence only if it can fail. Delete one rule from a
# copy of the validator: its mutation must then survive, and the run must be
# non-zero. A self-test that stays green here is measuring nothing.
if grep -q '("R13 tls-termination", rule_tls_termination),' "$VALIDATOR"; then
  sed 's/^    ("R13 tls-termination", rule_tls_termination),$//' \
    "$VALIDATOR" >"$WORK/validator-norule.py"
  python3 "$WORK/validator-norule.py" --compose "$COMPOSE" --env-example "$ENVEX" --selftest \
    >"$WORK/selftest-mutant.txt" 2>&1
  MUT_RC=$?
  if [[ $MUT_RC -ne 0 ]] && grep -q 'FAIL selftest R13 tls-termination: mutation survived' "$WORK/selftest-mutant.txt"; then
    ok "MUTATION CHECK: deleting a rule makes the self-test fail (exit $MUT_RC)"
  else
    fail "MUTATION CHECK: deleting a rule did NOT fail the self-test (exit $MUT_RC) — the self-test is vacuous"
    sed 's/^/     /' "$WORK/selftest-mutant.txt"
  fi
else
  fail "MUTATION CHECK: the R13 anchor is gone from the validator — this check has rotted"
fi

# ---------------------------------------------------------------------------
# 2. the runbook: every repository path it names exists
# ---------------------------------------------------------------------------
cat >"$WORK/runbook-paths.py" <<'PY'
"""Every repository FILE named in the runbook must exist.

A backticked token is a file claim when it contains a slash, starts with a
top-level entry of the repository, and its last segment has an extension.
Each of those three is load-bearing:

  - the top-level check keeps /etc/nginx/tls, <repo>@sha256:<digest> and
    #anchor links out without a hand-maintained exception list;
  - the slash keeps bare filenames out (the file table names its neighbours
    relatively: `staging.env.example`);
  - the extension keeps the repository's own short-form citation style out.
    `docs/35:8` is how this tree cites a document in code comments, dozens of
    times, and it names no file. A citation without an extension is not a path
    claim, and neither is a directory.

The whole document is scanned, including "What is missing" — that section is
the list of things that do not exist yet, and it names them as packages and
directories (`cmd/migrate`, `cmd/**`), which the extension rule already
excludes, while the files it cites as evidence for a gap
(`internal/application/notifications/ports.go:72`,
`cmd/api/authhttp/auth_handler.go:250-256`) are existing files that this check
should keep honest. The consequence is deliberate: a gap that must name a
specific file which does not exist yet will red this check, and the fix is to
name the directory — that path really does not exist.

`ops/deploy/.env` is the one exception: the runbook names it as the file the
OPERATOR creates from the example, and it is git-ignored, so its absence is
correct.
"""
import re, sys
from pathlib import Path

root, runbook = Path(sys.argv[1]), Path(sys.argv[2])
text = runbook.read_text(encoding="utf-8")
top = {p.name for p in root.iterdir()}

seen, missing = [], []
for tok in re.findall(r"`([^`\n]+)`", text):
    tok = re.sub(r":\d+(-\d+)?$", "", tok.strip())   # file.md:9-11 -> file.md
    if "/" not in tok or "*" in tok or tok.startswith(("http", "/", "..", "<")):
        continue
    if "." not in tok.rsplit("/", 1)[1]:             # `docs/35:8`, `cmd/migrate`
        continue
    if tok.split("/", 1)[0] not in top or tok == "ops/deploy/.env":
        continue
    if tok in seen:
        continue
    seen.append(tok)
    if not (root / tok).exists():
        missing.append(tok)

for m in missing:
    print("missing: %s" % m)
for s in seen:
    print("exist: %s" % s)
print("checked: %d file(s)" % len(seen))
sys.exit(1 if missing else 0)
PY

python3 "$WORK/runbook-paths.py" "$ROOT" "$RUNBOOK" >"$WORK/runbook.txt" 2>&1
RB_RC=$?
if [[ $RB_RC -eq 0 ]]; then
  ok "runbook: $(grep '^checked:' "$WORK/runbook.txt") it names, all exist"
  grep '^exist:' "$WORK/runbook.txt" | sed 's/^/     /'
else
  fail "runbook names repository files that do not exist (exit $RB_RC)"
  sed 's/^/     /' "$WORK/runbook.txt"
fi

# ... and proof the file check can fail: feed it a file that does not exist.
# The anchor is a row of the "See also" list — a real claim about this tree.
sed 's#infra/docker/README\.md#infra/docker/README.nonexistent.md#g' \
  "$RUNBOOK" >"$WORK/runbook-mut.md"
if cmp -s "$RUNBOOK" "$WORK/runbook-mut.md"; then
  fail "MUTATION CHECK: the runbook mutation did not apply — this check has rotted"
else
  python3 "$WORK/runbook-paths.py" "$ROOT" "$WORK/runbook-mut.md" >"$WORK/runbook-mut.txt" 2>&1
  if [[ $? -ne 0 ]] && grep -q 'missing: infra/docker/README.nonexistent.md' "$WORK/runbook-mut.txt"; then
    ok "MUTATION CHECK: a runbook file that does not exist is caught"
  else
    fail "MUTATION CHECK: the runbook file check accepted a file that does not exist"
    sed 's/^/     /' "$WORK/runbook-mut.txt"
  fi
fi

# The runbook must state what is missing, in a section of its own. The whole
# point of this deliverable is that it does not claim a deployment which cannot
# happen; deleting that section is a regression, so it is checked — and made to
# fail once, on a copy, because this is the check that carries the honesty of
# the whole file.
missing_section_ok() {
  grep -q '^## What is missing' "$1" && grep -qi 'no `Dockerfile`' "$1"
}
if missing_section_ok "$RUNBOOK"; then
  ok "runbook names the missing pieces ('What is missing' present, image gap stated)"
else
  fail "runbook has no 'What is missing' section naming the absent Dockerfiles"
fi

sed 's/^## What is missing$/## Gaps/' "$RUNBOOK" >"$WORK/runbook-nogaps.md"
if cmp -s "$RUNBOOK" "$WORK/runbook-nogaps.md"; then
  fail "MUTATION CHECK: the 'What is missing' heading was not found to mutate — this check has rotted"
elif missing_section_ok "$WORK/runbook-nogaps.md"; then
  fail "MUTATION CHECK: the check passed with the 'What is missing' section removed"
else
  ok "MUTATION CHECK: dropping the 'What is missing' section is caught"
fi

# ---------------------------------------------------------------------------
# 3. the repository's own secret scanner, over the new example file
# ---------------------------------------------------------------------------
cd "$ROOT" || exit 1
timeout 600 go test ./internal/config -run TestRepoExampleFilesAreSecretFree -count=1 \
  >"$WORK/secretscan.txt" 2>&1
SC_RC=$?
if [[ $SC_RC -eq 0 ]]; then
  ok "secret scan: committed *.env.example files are placeholder-only"
else
  fail "secret scan: exit $SC_RC"
  tail -12 "$WORK/secretscan.txt" | sed 's/^/     /'
fi

# ... and proof the sweep reads THIS file. Its rules are not re-implemented
# here on purpose — the real scanner is exercised, over a planted file placed
# where the sweep walks. (internal/config is an internal package: a test binary
# is the only honest way to reach it.)
printf 'POST_GITEA_TOKEN=planted-value-not-a-documented-placeholder\n' >"$PLANTED"
timeout 600 go test ./internal/config -run TestRepoExampleFilesAreSecretFree -count=1 \
  >"$WORK/planted.txt" 2>&1
PL_RC=$?
command rm -f "$PLANTED"
if [[ $PL_RC -ne 0 ]] && grep -q 'staging.planted-check.env.example' "$WORK/planted.txt"; then
  ok "MUTATION CHECK: the secret scan names a planted secret in ops/deploy/"
else
  fail "MUTATION CHECK: planted secret not caught (exit $PL_RC) — the scan may not cover this directory"
  tail -12 "$WORK/planted.txt" | sed 's/^/     /'
fi

# ---------------------------------------------------------------------------
# 4. optional: hand the template to the real Compose parser
# ---------------------------------------------------------------------------
if [[ "${DEPLOY_SMOKE_DOCKER:-0}" == "1" ]]; then
  note "DEPLOY_SMOKE_DOCKER=1: running the Compose parser over the template"
  python3 "$VALIDATOR" --compose "$COMPOSE" --env-example "$ENVEX" --with-docker \
    >"$WORK/docker.txt" 2>&1
  DC_RC=$?
  if grep -q '^skip docker compose config' "$WORK/docker.txt"; then
    note "$(grep '^skip docker compose config' "$WORK/docker.txt")"
    note "not counted as a pass and not counted as a failure: the parser was unavailable"
  elif [[ $DC_RC -eq 0 ]] && grep -q '^ok   docker compose config' "$WORK/docker.txt"; then
    ok "docker compose config accepted the template"
  else
    fail "docker compose config rejected the template (exit $DC_RC)"
    grep -E '^(FAIL|     )' "$WORK/docker.txt" | sed 's/^/  /'
  fi
else
  note "NOT RUN: docker compose config — this host's Compose parser was not invoked."
  note "  The Compose binary is a third-party parser whose availability varies by host,"
  note "  and a gate whose verdict depends on whether a tool happens to be installed is a"
  note "  flaky gate. The 19 rules above are the gate; set DEPLOY_SMOKE_DOCKER=1 to add it."
fi

# ---------------------------------------------------------------------------
printf '\nsmoke: %d failure(s)\n' "$FAILS"
if [[ "$FAILS" -gt 0 ]]; then
  printf 'SMOKE TEST RESULT: FAIL\n'
  exit 1
fi
printf 'SMOKE TEST RESULT: PASS\n'
exit 0
