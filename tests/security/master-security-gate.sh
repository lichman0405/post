#!/usr/bin/env bash
#
# T1206 — the Master Security/Quality Gate: ONE command, every security and
# quality check this tree can actually run, and no way for a check to be
# quietly green.
#
# Why this file exists
# --------------------
# docs/23_SECURITY_PRIVACY.md §11 asks every Release to run a dependency
# audit, SAST, a secret scan, a container scan, an OWASP smoke and a
# permission E2E. Before this file, most of those were run by nobody in
# particular: the a11y smoke and the staging-template smoke were invoked by
# no CI job and no Makefile target, and the two that did run were run by
# whoever happened to remember. A check nobody runs is a check that cannot
# fail, and a check that cannot fail is not evidence.
#
# What it enforces on itself
# --------------------------
#  1. EVERY CHECK IS A NAMED ROW THAT CAN FAIL. Its command, the exit code
#     it must return and the evidence lines it must print are declared
#     together, in the registry below.
#  2. EXIT 0 IS NOT A PASS BY ITSELF. A row that returns 0 without printing
#     the lines it declared is UNSUBSTANTIATED and fails the gate. That is
#     the silent-skip guard: a check gutted to `echo skipping; exit 0` — or
#     one that lost half its assertions — is red, not green. `--selftest`
#     measures this accounting on purpose, with decoy checks.
#  3. A CHECK THAT CANNOT BE ASKED IS NOT ASKED, OUT LOUD. A missing tool, an
#     unreachable PostgreSQL: the row prints `NOT ASKED` with the reason, is
#     listed in the summary, lands in the run report, and fails the gate with
#     its own exit code (2). `--allow-not-asked` turns that exit code green
#     and nothing else: the rows are still printed and still recorded. It is
#     for a bare CI runner, never for a Release.
#  4. WHAT THE TREE CANNOT DO IS WRITTEN DOWN, NOT OMITTED, AND WHAT IT DOES
#     IS COUNTED. ops/security/absent-checks.json lists every capability
#     docs/23 §11 asks a Release to run, each one either covered by named rows
#     of this gate or named as absent, and the `absence-manifest` row below
#     keeps that file true — including its one remaining absence, the
#     container scan, which is a GUARDED absence: the `container-scan` row
#     here is green only while the tree really has no container build file,
#     and goes red the moment one appears. Whether an absent capability is an
#     accepted V1 risk is the Supervisor's written call, not this script's:
#     the manifest records the gap and the evidence, and says so.
#
# Two different "not here", deliberately kept apart
# -------------------------------------------------
#   * the CAPABILITY MANIFEST (ops/security/absent-checks.json) is static: it
#     is about what the tree does not have at all, and it does not change
#     because this host happens to lack a tool.
#   * the RUN REPORT (`--report FILE`, and always in the temp dir) is about
#     this run: which checks ran, what they returned, and which ones this
#     host could not ask — with the reason. A NOT ASKED row is never dropped.
#
# Exit codes
#   0  every selected check ran and passed
#   1  at least one check failed, or returned 0 without its evidence
#   2  nothing failed, but at least one check was NOT ASKED (not a full gate)
#   3  usage error, or the check registry itself is broken
#   4  --selftest found the accounting wrong (that is a bug in this file)
#
# Usage
#   bash tests/security/master-security-gate.sh                  # the gate
#   bash tests/security/master-security-gate.sh --list           # the rows
#   bash tests/security/master-security-gate.sh --list-json      # machine-readable
#   bash tests/security/master-security-gate.sh --selftest        # prove it can say no
#   bash tests/security/master-security-gate.sh --report /tmp/r.json
#
# Env
#   POSTGRES_TEST_ADMIN_URL  admin endpoint the owasp smoke uses (default: dev)
#   POST_G3_REDIS_ADDR       Redis the owasp smoke uses (default 127.0.0.1:6379)
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

PG_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"
REDIS_ADDR="${POST_G3_REDIS_ADDR:-127.0.0.1:6379}"

# The tool pins, so that a NOT ASKED reason quotes the version the tree
# actually pins instead of a second copy of it typed into a hint string. The
# vuln-go row's hint used to name its scanner with no version at all (`@latest`)
# — the one scanner in this gate with no pin anywhere except the CI job's own
# step. Now the hint quotes the pin, and the pin is the only place a version is
# written down.
VERSIONS="$ROOT/ops/security/tool-versions.sh"
# shellcheck source=/dev/null
. "$VERSIONS" || { echo "master-security-gate: cannot read $VERSIONS" >&2; exit $EXIT_USAGE; }

# The identity of THIS run, inherited by every row and by every script a row
# runs. It exists so that an artifact-producing row can stamp what it wrote
# and a row that reads an artifact can tell "produced by this run" from "found
# on disk from an earlier one" — the licence audit reads the three SBOMs the
# sbom-* rows write, and under --only, or with an sbom row NOT ASKED, those
# documents would otherwise be from a previous run with nothing in the output
# saying so. See tests/security/sbom.sh (writes the stamp) and
# tests/security/license_audit.py (reads it).
export POST_GATE_RUN_ID="gate-$(date -u +%Y%m%dT%H%M%SZ)-$$"

EXIT_PASS=0 EXIT_FAILED=1 EXIT_NOT_ASKED=2 EXIT_USAGE=3 EXIT_SELFTEST=4

WORK="$(mktemp -d)"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

# ---------------------------------------------------------------------------
# The registry.
#
# add_check <id> <name> <category> <prerequisites> <command> <evidence>...
#
#   prerequisites  space-separated tokens resolved by prereq_missing below;
#                  a token that is not satisfied makes the row NOT ASKED (it
#                  is then never executed, so a missing tool cannot be
#                  mistaken for a passing command).
#   command        run from $ROOT with bash -c; its exit code is the row's.
#   evidence       ERE lines that MUST each appear in a 0-exit run's output.
#                  This is what makes an exit code mean something: the
#                  patterns are the checks' own success lines, so a row that
#                  lost an assertion — or never ran it — is caught.
# ---------------------------------------------------------------------------
CHECK_ID=() CHECK_NAME=() CHECK_DOC=() CHECK_REQUIRES=() CHECK_CMD=() CHECK_EVIDENCE=()

add_check() { # add_check <id> <name> <doc> <requires> <command> <evidence>...
  local id="$1" name="$2" doc="$3" requires="$4" cmd="$5"
  shift 5
  local existing
  for existing in ${CHECK_ID[@]+"${CHECK_ID[@]}"}; do
    if [ "$existing" = "$id" ]; then
      echo "master-security-gate: registry error: duplicate check id '$id'" >&2
      exit $EXIT_USAGE
    fi
  done
  CHECK_ID+=("$id"); CHECK_NAME+=("$name"); CHECK_DOC+=("$doc")
  CHECK_REQUIRES+=("$requires"); CHECK_CMD+=("$cmd")
  CHECK_EVIDENCE+=("$(printf '%s\n' "$@")")
}

# The floor below is part of the gate, not a comment: deleting rows from this
# registry is the cheapest way to make a security gate green, and it is the
# one that leaves no failing output behind. See the registry-integrity guard
# in the run loop.
#
# A FLOOR, not an equality, and deliberately so: `--selftest` registers decoy
# rows through `--extra-checks`, so the count is allowed to exceed it and the
# guard uses `-lt`. What that costs is that a row registered ABOVE the floor
# can be deleted in silence — which is the one thing this guard exists to
# prevent — so raising the registry raises the floor with it.
#
# 17 -> 18 (2026-09-24, T1224), for the `sast-go-surface` row registered
# below it and nothing else: no row was removed, none gained a skip
# condition, no evidence pattern was widened, `-lt` is unchanged, and every
# row still has to print what it declared to be green. The row this bump
# brings inside the floor is the surface criterion of the go row — the half
# of it that a silent shrink would hide — so leaving it outside would have
# made it the cheapest row in this file to delete.
MIN_CHECKS=18

# --- security surface -------------------------------------------------------

add_check secret-scan \
  "Secret scan: no secret-shaped value in any committed *.env.example, repository-wide" \
  "docs/23 §11 (secret scan); docs/31 Gate H" \
  "go" \
  "go test ./internal/config -run 'TestRepoExampleFilesAreSecretFree|TestScanRepoExampleFilesFailsOnPlantedFile' -count=1 -v" \
  '^--- PASS: TestRepoExampleFilesAreSecretFree ' \
  '^--- PASS: TestScanRepoExampleFilesFailsOnPlantedFile '

add_check permission-negative-e2e \
  "Permission negative E2E: a private project stays invisible to anonymous and non-member readers" \
  "docs/23 §11 (permission E2E); docs/31 Gate H" \
  "go" \
  "go test ./tests/e2e -run TestE2EPrivacyNegative -count=1 -v" \
  '^--- PASS: TestE2EPrivacyNegative '

add_check owasp-smoke \
  "OWASP edge smoke against real services: header set, CORS, anonymous write, rate limit, fail-closed" \
  "docs/23 §11 (OWASP smoke); docs/67 G3 security-smoke" \
  "go python3 psql curl redis redis-cli node postgres file:tests/security/owasp-smoke.sh" \
  "bash tests/security/owasp-smoke.sh" \
  '^ok   S0 ' '^ok   S1 ' '^ok   S2 ' '^ok   S3 ' '^ok   S4 ' '^ok   S5 ' '^ok   S6 ' '^ok   S7 ' '^ok   S8 ' \
  '^G3 security-smoke: OK'

# --- quality surface --------------------------------------------------------

add_check a11y \
  "Accessibility: axe-core WCAG 2 A/AA + best-practice, and a keyboard walkthrough, in real Chromium" \
  "docs/31 Gate H (accessibility AA core pages); docs/67 (browser E2E)" \
  "node web-deps file:tests/web-smoke/run.sh" \
  "WEB_SMOKE_CHECKS=a11y bash tests/web-smoke/run.sh" \
  '^ok   axe wcag / @[0-9]+x[0-9]+: 0 violations' \
  '^ok   axe wcag /projects @[0-9]+x[0-9]+: 0 violations' \
  '^ok   axe best-practice / @[0-9]+x[0-9]+: 0 violations' \
  '^ok   axe best-practice /projects @[0-9]+x[0-9]+: 0 violations' \
  '^ok   keyboard: skip link is the first tab stop and visible on focus' \
  '^ok   keyboard: every header stop shows a visible focus indicator' \
  '^a11y smoke: all checks passed'

add_check deploy-template \
  "Staging deployment template: static smoke, including the validator's and the secret scan's own mutations" \
  "docs/25 (deploy); ops/deploy/README.md" \
  "python3 go py:yaml file:tests/acceptance/deploy-staging-smoke.sh" \
  "bash tests/acceptance/deploy-staging-smoke.sh" \
  '^ok   inputs present: ' \
  '^ok   self-test: ' \
  '^ok   secret scan: committed' \
  '^ok   MUTATION CHECK: the secret scan names a planted secret' \
  '^SMOKE TEST RESULT: PASS'

# --- dependency / CVE audits ------------------------------------------------
#
# Three languages, three instruments, each one its own row so a red Go audit
# cannot be hidden by a green Node one.

add_check vuln-go \
  "Dependency/CVE audit (Go): govulncheck over the module graph and reachable symbols" \
  "docs/23 §11 (dependency audit)" \
  "govulncheck" \
  "govulncheck ./..." \
  '^No vulnerabilities found\.$'

add_check vuln-node \
  "Dependency/CVE audit (Node/web): pnpm audit at --audit-level=low" \
  "docs/23 §11 (dependency audit)" \
  "pnpm" \
  "pnpm audit --audit-level=low" \
  '^No known vulnerabilities found'

add_check vuln-python \
  "Dependency/CVE audit (Python adapter): uv audit" \
  "docs/23 §11 (dependency audit)" \
  "uv file:services/scientific-adapter/pyproject.toml" \
  "cd services/scientific-adapter && uv audit" \
  '^Found no known vulnerabilities'

# --- SAST (docs/23 §11) -----------------------------------------------------
#
# Three languages, three instruments, three rows: a red Go scan cannot be
# hidden behind a green Node one. tests/security/sast.sh runs one face per
# invocation. Each row asserts the version of the tool it actually ran against
# ops/security/tool-versions.sh (`make security-tools` installs exactly those),
# and each one refuses a scan that covered too little of the tree — a scan of
# an empty tree reports no findings, which is indistinguishable from a clean
# one. The findings that exist today are baselined PER FINDING in
# ops/ci/*-baseline.txt with a written review attached to each key; no
# invocation below carries -exclude-dir, -nosec or a severity floor, and a
# finding that is not in its baseline is red.

add_check sast-go \
  "SAST (Go): gosec over the module, every finding new (red) or baselined individually" \
  "docs/23 §11 (SAST)" \
  "go gosec file:tests/security/sast.sh file:ops/ci/gosec-baseline.txt file:ops/security/tool-versions.sh" \
  "bash tests/security/sast.sh go" \
  '^ok   sast-go: gosec v2\.29\.0: [0-9]+ file\(s\) scanned, [0-9]+ finding\(s\) reported \([0-9]+ baselined \(reviewed\), 0 unbaselined\)' \
  '^     severity mix: '

# The go row's SURFACE, registered directly under it because the two read as
# one sentence: this is the half that decides what `sast.sh go` was pointed
# at. It was a complete instrument with no executor until now — nothing ran
# tests/security/sast-go-surface-check.sh, so its cases, including the two
# T1222 added for the `!` re-inclusion shapes, could not fail anywhere. A
# check nobody runs is a check that cannot fail.
#
# What it plants, and what it therefore measures rather than reads out of
# tests/security/sast.sh: an ignored copy under a dot directory, an ignored
# package directory, an ignored FILE inside a package that stays, a
# re-included file, a re-included package directory, a Go file under
# testdata/, and (T1225) a re-inclusion whose rule was read from an ignore file
# whose own PATH contains a colon — running the go row once per case and
# requiring the answer git and the toolchain actually give, not the one the
# comment claims. Its last case asserts the tree is as it was found, which is
# what keeps a plant from becoming the next run's input. The row is here in
# ADDITION to sast-go and not instead of it: sast-go judges the findings, this
# one judges the surface they were taken from, and a green sast-go on a
# silently shrunken surface is exactly the failure the instrument exists to
# catch.

add_check sast-go-surface \
  "SAST (Go) surface: what the go row scans, against planted copies, ignored files and re-included paths" \
  "docs/23 §11 (SAST); tests/security/sast.sh (the go row's surface)" \
  "go gosec git python3 file:tests/security/sast-go-surface-check.sh file:tests/security/sast.sh file:ops/ci/gosec-baseline.txt file:ops/security/tool-versions.sh" \
  "bash tests/security/sast-go-surface-check.sh" \
  '^sast-go-surface-check: OK — eleven cases: the derived surface is green and prints itself, a copy' \
  '^ok   case 7: green — the re-included file was not reported as an escape' \
  '^ok   case 8: green, and the re-included package directory stayed in the surface' \
  '^ok   case 11: the re-included package directory under a colon-named source was scanned'

add_check sast-python \
  "SAST (Python adapter): bandit over the adapter's source, no skip list and no severity floor" \
  "docs/23 §11 (SAST)" \
  "python3 bandit file:tests/security/sast.sh file:ops/ci/bandit-baseline.txt" \
  "bash tests/security/sast.sh python" \
  '^ok   sast-python: bandit 1\.8\.6: [0-9]+ file\(s\) scanned, [0-9]+ finding\(s\) reported \([0-9]+ baselined \(reviewed\), 0 unbaselined\)'

add_check sast-node \
  "SAST (web + shared UI): eslint with the security plugin's whole recommended set forced to error" \
  "docs/23 §11 (SAST)" \
  "node dir:tests/security/node-tools/node_modules file:tests/security/sast.sh file:ops/ci/eslint-security-baseline.txt" \
  "bash tests/security/sast.sh node" \
  '^ok   sast-node: eslint 9\.39\.5: [0-9]+ file\(s\) scanned, [0-9]+ rule\(s\), [0-9]+ finding\(s\) reported \([0-9]+ baselined \(reviewed\), 0 unbaselined\)' \
  '^     rules: [0-9]+ from eslint-plugin-security/recommended, every one of them at "error"'

# --- SBOM (docs/25 §1 item 10; docs/40) -------------------------------------
#
# One document per face, generated by the row that reports it, so a green row
# is a document produced in THIS run rather than one that happened to be lying
# around. The documents are written to the ignored .sbom/ directory and are
# never committed: they are reproducible from go.mod, pnpm-lock.yaml and
# uv.lock, and a committed SBOM is an inventory of a tree that no longer
# exists. A row is green only after tests/security/check-sbom.py has re-parsed
# the document, cleared the face's component floor and resolved every key
# component in tests/security/key-components.json — including the entries
# whose answer is "deliberately not in this tree", whose witness has to keep
# saying so.

add_check sbom-go \
  "SBOM (Go): cyclonedx-gomod over go.mod, licence evidence per module, validated" \
  "docs/25 §1 item 10; docs/40 §3" \
  "go cyclonedx-gomod file:tests/security/sbom.sh file:tests/security/check-sbom.py file:tests/security/key-components.json" \
  "bash tests/security/sbom.sh go" \
  '^ok   sbom-go: cyclonedx-gomod v1\.9\.0 \(go\.mod, license evidence included\): [0-9]+ component\(s\), spec 1\.[0-9]+' \
  '^     subject: github.com/lichman0405/post' \
  '^     chi: absent, as documented'

add_check sbom-node \
  "SBOM (npm workspace): pnpm sbom over the installed workspace, CycloneDX 1.5" \
  "docs/25 §1 item 10; docs/40 §3" \
  "node pnpm workspace-deps file:tests/security/sbom.sh file:tests/security/check-sbom.py file:tests/security/key-components.json" \
  "bash tests/security/sbom.sh node" \
  '^ok   sbom-node: pnpm [0-9.]+ sbom \(pnpm-lock\.yaml, CycloneDX 1\.5\): [0-9]+ component\(s\), spec 1\.5' \
  '^     Playwright: witnessed'

add_check sbom-python \
  "SBOM (Python adapter): uv export + the installed distributions' own METADATA for licences" \
  "docs/25 §1 item 10; docs/40 §3" \
  "uv dir:services/scientific-adapter/.venv file:tests/security/sbom.sh file:tests/security/sbom_python_licenses.py file:tests/security/key-components.json" \
  "bash tests/security/sbom.sh python" \
  '^ok   sbom-python: uv [0-9.]+ export --format cyclonedx1\.5 \+ licenses from .* dist-info METADATA: [0-9]+ component\(s\), spec 1\.5' \
  '^     pymatgen: witnessed'

# --- licence audit (docs/40) ------------------------------------------------
#
# Reads the three documents the rows above just produced and judges every
# component's licence against ops/security/license-allowlist.json, which
# carries a written reason per allowed licence and a deny class that goes red
# naming package, licence and version. It runs AFTER the sbom-* rows on
# purpose: a licence audit with no inventory to audit is NOT ASKED here, not
# green — hence the file prerequisites, which say exactly that.

add_check license-audit \
  "Licence audit: every SBOM component judged against ops/security/license-allowlist.json" \
  "docs/40_OPEN_SOURCE_LICENSES.md §3; docs/23 §11" \
  "python3 go file:ops/security/license-allowlist.json file:tests/security/key-components.json file:.sbom/go.cdx.json file:.sbom/node.cdx.json file:.sbom/python.cdx.json" \
  "python3 tests/security/license_audit.py" \
  '^ok   license-audit: [0-9]+ component\(s\) across 3 SBOM\(s\), every licence judged against ops/security/license-allowlist\.json' \
  '^     first-party, not third-party dependencies \([0-9]+\):'

# --- container scan: a GUARDED absence --------------------------------------
#
# There is no Dockerfile in this tree, so there is no image to scan, and a row
# that said "not applicable" would be a permanent green light nobody revisits.
# This row is instead the absence made falsifiable: green only while the tree
# really has no container build file — printing the claim it rests on, where
# that claim is registered and how much of the tree it walked — and RED the
# moment a Dockerfile or Containerfile appears, or if the `no-dockerfile`
# claim stops being registered. A green here is not a container scan and does
# not claim to be one; ops/security/absent-checks.json records the gap itself
# as the Supervisor's risk call to make.

add_check container-scan \
  "Container scan: a guarded absence — green only while the tree has no container build file" \
  "docs/23 §11 (container scan); docs/25 §1 item 10" \
  "python3 file:tests/security/container-scan.sh file:ops/runbook-steps.json" \
  "bash tests/security/container-scan.sh" \
  '^ok   container-scan: no container build file in the tree \([0-9]+ file\(s\) walked' \
  '^     the claim this rests on: ops/runbook-steps\.json \(id no-dockerfile, re-derived by ops/runbook-verify\.py T1\);'

# --- the absence manifest ---------------------------------------------------

add_check absence-manifest \
  "Absence manifest: every §11 capability covered by a named row or named as absent, and the guard itself checked" \
  "docs/23 §11; ops/security/absent-checks.json" \
  "python3 file:ops/security/absent-checks.json file:tests/security/check-absent-manifest.py" \
  "python3 tests/security/check-absent-manifest.py --selftest" \
  '^ok   manifest: ' \
  '^ok   selftest: ' \
  '^ok   item: sast — covered by sast-go, sast-python, sast-node' \
  '^ok   item: container-scan — guarded absence: the .container-scan. row of this gate is registered' \
  '^ok   item: sbom — covered by sbom-go, sbom-node, sbom-python, license-audit' \
  '^ok   item: dependency-audit' \
  '^ok   item: secret-scan' \
  '^ok   item: owasp-smoke' \
  '^ok   item: permission-e2e'

# ---------------------------------------------------------------------------
# Prerequisites. A token that cannot be satisfied is a REASON, printed with
# the row: the gate never runs a command it cannot trust to have measured the
# thing it names.
#
# Token forms: a bare name is a command on PATH (`go`, `redis-cli`) or, for
# `redis`/`postgres`/`web-deps`/`workspace-deps`, a probe; `dir:PATH` and
# `file:PATH` are tree paths; `py:MODULE` is a Python module the row's command
# imports. `py:yaml` exists because the deploy-template row's command needs
# PyYAML and, without it, dies with exit 2 twenty lines into its own log —
# where the gate's tail -25 window does not reach. A missing module has to
# read as NOT ASKED with the module named, not as a red whose reason is off
# screen.
#
# Every token here is a claim about the row's COMMAND, not about the row's
# subject: the owasp smoke needs node (S8 runs the web header test through
# `node --test`) and redis-cli (S0 resets its own rate-limit buckets and reads
# them back), so both are declared even though S1-S7 would run without them.
# ---------------------------------------------------------------------------
have() { command -v "$1" >/dev/null 2>&1; }

postgres_ready() {
  have python3 || return 1
  python3 "$ROOT/scripts/pg-ready.py" "$PG_URL" >/dev/null 2>&1
}

redis_ready() {
  # The probe runs in a subshell and its fd closes with it. Never write this
  # as a top-level `exec 3<&- 2>/dev/null`: a redirection on `exec` with no
  # command applies to THIS SHELL, so the script's own stderr would go to
  # /dev/null for the rest of the run — every FAIL and NOT ASKED message
  # included. The mutation check caught exactly that in an earlier revision.
  ( exec 3<>"/dev/tcp/${REDIS_ADDR%:*}/${REDIS_ADDR#*:}" ) 2>/dev/null || return 1
  return 0
}

prereq_missing() { # -> one reason per line, empty when everything resolves
  local token
  for token in $1; do
    case "$token" in
      go)          have go          || echo "go is not on PATH (https://go.dev/dl)";;
      node)        have node        || echo "node is not on PATH";;
      pnpm)        have pnpm        || echo "pnpm is not on PATH (corepack enable pnpm)";;
      python3)     have python3     || echo "python3 is not on PATH";;
      # The one tool below that is not a scanner: the go face's surface check
      # tells this repository's own source from the local state beside it by
      # asking git (tests/security/sast.sh repo_paths_only), and the instrument
      # that measures that check refuses to run without git at all. Declared
      # rather than left to its own `die`: a missing tool is a row this host
      # cannot ask, and NOT ASKED with the tool named is what the gate's third
      # rule says that reads as.
      git)         have git         || echo "git is not on PATH — the go face's surface check tells this repository's source from the local state beside it by asking git (tests/security/sast.sh repo_paths_only), and sast-go-surface-check.sh refuses a tree that is not a git work tree";;
      psql)        have psql        || echo "psql is not on PATH (the owasp smoke gives itself its own database)";;
      curl)        have curl        || echo "curl is not on PATH";;
      uv)          have uv          || echo "uv is not on PATH (pip install uv)";;
      govulncheck) have govulncheck || echo "govulncheck is not on PATH — run 'make security-tools' (it installs the pinned $GOVULNCHECK_VERSION from ops/security/tool-versions.sh into \$(go env GOPATH)/bin; by hand: go install golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION)";;
      redis-cli)   have redis-cli   || echo "redis-cli is not on PATH — the owasp smoke resets and reads back its own rate-limit buckets through it before measuring the boundary, so without it that measurement is of another run's traffic (Debian/Ubuntu: apt-get install redis-tools)";;
      gosec)       have gosec       || echo "gosec is not on PATH — run 'make security-tools' (it installs the pinned version into \$(go env GOPATH)/bin)";;
      bandit)      have bandit      || echo "bandit is not on PATH — run 'make security-tools' (uv tool install bandit)";;
      cyclonedx-gomod) have cyclonedx-gomod || echo "cyclonedx-gomod is not on PATH — run 'make security-tools'";;
      redis)       redis_ready      || echo "no Redis answering at $REDIS_ADDR";;
      postgres)    postgres_ready   || echo "no PostgreSQL accepting connections at ${PG_URL%%\?*} (make infra-up)";;
      web-deps)    [ -d "$ROOT/apps/web/node_modules" ] || echo "apps/web/node_modules is missing (pnpm install --frozen-lockfile)";;
      workspace-deps) [ -d "$ROOT/node_modules" ] || echo "the workspace's node_modules is missing (pnpm install --frozen-lockfile) — pnpm's SBOM reads the INSTALLED workspace";;
      dir:*)       [ -d "$ROOT/${token#dir:}" ] || echo "required directory is missing: ${token#dir:} (run 'make security-tools')";;
      file:*)      [ -f "$ROOT/${token#file:}" ] || echo "required file is missing: ${token#file:}";;
      py:*)        have python3 && python3 -c "import ${token#py:}" >/dev/null 2>&1 \
                     || echo "python3 cannot import the module '${token#py:}', which the command this row runs needs. The distribution name is often not the module name (PyYAML ships 'yaml'): install the one the row's own script names, then re-run.";;
      "")          ;;
      *)           echo "unknown prerequisite token '$token' (a typo here would silently pass)";;
    esac
  done
}

# ---------------------------------------------------------------------------
# Arguments.
# ---------------------------------------------------------------------------
ONLY=""; EXTRA_CHECKS=""; REPORT=""; LIST=0; LIST_JSON=0; SELFTEST=0
ALLOW_NOT_ASKED="${POST_MASTER_GATE_ALLOW_NOT_ASKED:-0}"

usage() {
  sed -n '2,60p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' | sed '/^$/q'
}

while [ $# -gt 0 ]; do
  case "$1" in
    --list)            LIST=1;;
    --list-json)       LIST_JSON=1;;
    --only)            ONLY="${2:-}"; shift;;
    --extra-checks)    EXTRA_CHECKS="${2:-}"; shift;;
    --report)          REPORT="${2:-}"; shift;;
    --allow-not-asked) ALLOW_NOT_ASKED=1;;
    --selftest)        SELFTEST=1;;
    -h|--help)         usage; exit $EXIT_PASS;;
    *) echo "master-security-gate: unknown argument '$1'" >&2; usage >&2; exit $EXIT_USAGE;;
  esac
  shift
done

# Additive only, and it says what it added: the extra-checks hook exists for
# --selftest's decoys (and for a future CI stage that wants an extra row). It
# cannot replace a row — a duplicate id is a registry error above.
if [ -n "$EXTRA_CHECKS" ]; then
  [ -f "$EXTRA_CHECKS" ] || {
    echo "master-security-gate: --extra-checks file not found: $EXTRA_CHECKS" >&2
    exit $EXIT_USAGE
  }
  before="${#CHECK_ID[@]}"
  # shellcheck disable=SC1090
  . "$EXTRA_CHECKS"
  echo "master-security-gate: $(( ${#CHECK_ID[@]} - before )) extra check(s) registered from $EXTRA_CHECKS"
fi

if [ "${#CHECK_ID[@]}" -lt "$MIN_CHECKS" ]; then
  echo "master-security-gate: registry integrity FAILED — ${#CHECK_ID[@]} check(s) registered, floor is $MIN_CHECKS." >&2
  echo "  Deleting a security check is the cheapest way to make a gate green; it is not a way to make it pass." >&2
  exit $EXIT_USAGE
fi

jesc() { printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr -d '\000-\037'; }

list_text() {
  local i
  printf 'id                          requires                          what it checks\n'
  for i in "${!CHECK_ID[@]}"; do
    printf '%-27s %-33s %s\n' "${CHECK_ID[$i]}" "${CHECK_REQUIRES[$i]}" "${CHECK_NAME[$i]}"
  done
}

list_json() {
  local i first=1
  printf '{"version":1,"min_checks":%d,"checks":[' "$MIN_CHECKS"
  for i in "${!CHECK_ID[@]}"; do
    [ "$first" = 1 ] || printf ','
    first=0
    printf '{"id":"%s","name":"%s","doc":"%s","requires":"%s","command":"%s"' \
      "$(jesc "${CHECK_ID[$i]}")" "$(jesc "${CHECK_NAME[$i]}")" "$(jesc "${CHECK_DOC[$i]}")" \
      "$(jesc "${CHECK_REQUIRES[$i]}")" "$(jesc "${CHECK_CMD[$i]}")"
    printf ',"evidence":['
    local pat pfirst=1
    while IFS= read -r pat; do
      [ -n "$pat" ] || continue
      [ "$pfirst" = 1 ] || printf ','
      pfirst=0
      printf '"%s"' "$(jesc "$pat")"
    done <<<"${CHECK_EVIDENCE[$i]}"
    printf ']}'
  done
  printf ']}\n'
}

[ "$LIST" = 1 ] && { list_text; exit $EXIT_PASS; }
[ "$LIST_JSON" = 1 ] && { list_json; exit $EXIT_PASS; }

# ---------------------------------------------------------------------------
# Selection. --only is a selection, never a smaller gate that calls itself
# the gate: it says PARTIAL in the summary and in the report.
# ---------------------------------------------------------------------------
SELECTED=()
if [ -n "$ONLY" ]; then
  IFS=',' read -r -a wanted <<<"$ONLY"
  for want in "${wanted[@]}"; do
    want="${want// /}"
    [ -n "$want" ] || continue
    found=0
    for id in "${CHECK_ID[@]}"; do
      if [ "$id" = "$want" ]; then found=1; SELECTED+=("$id"); fi
    done
    if [ "$found" = 0 ]; then
      # A name matching nothing is a configuration error, not a smaller run.
      echo "master-security-gate: '$want' is not a check id here; known: ${CHECK_ID[*]}" >&2
      exit $EXIT_USAGE
    fi
  done
  [ "${#SELECTED[@]}" -gt 0 ] || {
    echo "master-security-gate: --only selected no checks; an empty run is not a green run" >&2
    exit $EXIT_USAGE
  }
else
  SELECTED=("${CHECK_ID[@]}")
fi

index_of() {
  local id="$1" i
  for i in "${!CHECK_ID[@]}"; do
    [ "${CHECK_ID[$i]}" = "$id" ] && { printf '%s' "$i"; return 0; }
  done
  return 1
}

# ---------------------------------------------------------------------------
# The selftest: measure the accounting, not the tree.
#
# Every row above is only as good as the rule that decides what its exit code
# meant. So the rule is exercised on decoy checks — including the two shapes
# this file claims to catch: a check that exits 0 having printed nothing, and
# a check that exits 0 having printed only part of what it declared.
# ---------------------------------------------------------------------------
if [ "$SELFTEST" = 1 ]; then
  DECOYS="$WORK/decoys.sh"
  cat >"$DECOYS" <<'DECOY'
add_check decoy-silent-skip \
  "exits 0 and prints nothing at all" "selftest" "" \
  "printf 'web-smoke: skipping (nothing to do)\n'" \
  '^ok   '
add_check decoy-partial-evidence \
  "prints one of its two declared evidence lines" "selftest" "" \
  "printf 'ok   first half\n'" \
  '^ok   first half' \
  '^ok   second half that never runs'
add_check decoy-passes \
  "exits 0 and prints what it declared" "selftest" "" \
  "printf 'ok   decoy measured something\n'" \
  '^ok   decoy measured something'
add_check decoy-fails \
  "prints its evidence and exits 1 anyway" "selftest" "" \
  "printf 'ok   looks fine\n'; exit 1" \
  '^ok   looks fine'
add_check decoy-not-asked \
  "needs a tool this host does not have" "selftest" "no-such-tool-anywhere" \
  "printf 'ok   should never run\n'" \
  '^ok   should never run'
DECOY

  selftest_fails=0
  probe() { # probe <check-id> <expect-in-summary> <expect-rc> [extra args...]
    local id="$1" expect="$2" want_rc="$3"; shift 3
    local out rc
    out="$(bash "${BASH_SOURCE[0]}" --extra-checks "$DECOYS" --only "$id" "$@" 2>&1)"
    rc=$?
    if [ "$rc" != "$want_rc" ]; then
      printf 'FAIL selftest %s: exit %s, want %s\n%s\n' "$id" "$rc" "$want_rc" "$out"
      seltest_fails=$((selftest_fails + 1))
      return
    fi
    if ! grep -qE "$expect" <<<"$out"; then
      printf 'FAIL selftest %s: summary never said %s\n%s\n' "$id" "$expect" "$out"
      seltest_fails=$((selftest_fails + 1))
      return
    fi
    printf 'ok   selftest %s: exit %s, reported as %s\n' "$id" "$rc" "$expect"
  }

  # The two shapes that must never read as green.
  probe decoy-silent-skip      'UNSUBSTANTIATED' $EXIT_FAILED
  probe decoy-partial-evidence 'UNSUBSTANTIATED' $EXIT_FAILED
  # And the counter-cases: a real pass is still a pass, a real failure is
  # still a failure, and an unaskable check is loud without being a failure.
  probe decoy-passes           '^  ok '          $EXIT_PASS
  probe decoy-fails            'FAIL'            $EXIT_FAILED
  probe decoy-not-asked        'NOT ASKED'       $EXIT_NOT_ASKED
  probe decoy-not-asked        'NOT ASKED'       $EXIT_PASS --allow-not-asked

  if [ "$selftest_fails" -ne 0 ]; then
    printf '\nmaster-security-gate --selftest: %d of 6 probe(s) wrong — the accounting itself is broken\n' "$selftest_fails" >&2
    exit $EXIT_SELFTEST
  fi
  printf '\nmaster-security-gate --selftest: OK — exit 0 without evidence is red, a measured pass is green, NOT ASKED is loud\n'
  exit $EXIT_PASS
fi

# ---------------------------------------------------------------------------
# Run.
# ---------------------------------------------------------------------------
R_ID=() R_RESULT=() R_RC=() R_REASON=() R_LOG=() R_NAME=() R_CMD=()
NA_ID=() NA_REASON=() NA_CMD=()

printf '== master-security-gate: %d check(s)%s ==\n' "${#SELECTED[@]}" \
  "$( [ -n "$ONLY" ] && printf ' (PARTIAL RUN — --only, this is not the full gate)' )"

for id in "${SELECTED[@]}"; do
  i="$(index_of "$id")"
  log="$WORK/$id.log"
  missing="$(prereq_missing "${CHECK_REQUIRES[$i]}")"
  printf '\n---- %s: %s ----\n' "$id" "${CHECK_NAME[$i]}"
  if [ -n "$missing" ]; then
    reason="$(printf '%s' "$missing" | paste -sd'; ' -)"
    printf 'NOT ASKED %s\n' "$id"
    printf '     this host cannot run it: %s\n' "$reason"
    printf '     the command that was NOT run: %s\n' "${CHECK_CMD[$i]}"
    R_ID+=("$id"); R_RESULT+=("NOT ASKED"); R_RC+=(""); R_REASON+=("$reason")
    R_LOG+=(""); R_NAME+=("${CHECK_NAME[$i]}"); R_CMD+=("${CHECK_CMD[$i]}")
    NA_ID+=("$id"); NA_REASON+=("$reason"); NA_CMD+=("${CHECK_CMD[$i]}")
    continue
  fi
  ( eval "${CHECK_CMD[$i]}" ) >"$log" 2>&1
  rc=$?
  tail -25 "$log" | sed 's/^/     /'
  if [ "$rc" -ne 0 ]; then
    printf 'FAIL %s: exit %s\n' "$id" "$rc"
    R_ID+=("$id"); R_RESULT+=("FAIL"); R_RC+=("$rc"); R_REASON+=("exit $rc")
    R_LOG+=("$log"); R_NAME+=("${CHECK_NAME[$i]}"); R_CMD+=("${CHECK_CMD[$i]}")
    continue
  fi
  # Exit 0 is not yet a pass: the row has to have printed what it declared.
  unevidenced=""
  while IFS= read -r pat; do
    [ -n "$pat" ] || continue
    grep -qE "$pat" "$log" || unevidenced="$unevidenced|$pat"
  done <<<"${CHECK_EVIDENCE[$i]}"
  if [ -n "$unevidenced" ]; then
    printf 'UNSUBSTANTIATED %s: exit 0, but the evidence it declared is missing:\n' "$id"
    printf '%s\n' "$unevidenced" | tr '|' '\n' | sed '/^$/d;s/^/     never printed: /'
    R_ID+=("$id"); R_RESULT+=("UNSUBSTANTIATED"); R_RC+=("0")
    R_REASON+=("exit 0 without evidence ($(printf '%s' "$unevidenced" | tr '|' ' '))")
    R_LOG+=("$log"); R_NAME+=("${CHECK_NAME[$i]}"); R_CMD+=("${CHECK_CMD[$i]}")
    continue
  fi
  printf 'ok   %s\n' "$id"
  R_ID+=("$id"); R_RESULT+=("ok"); R_RC+=("0"); R_REASON+=(""); R_LOG+=("$log")
  R_NAME+=("${CHECK_NAME[$i]}"); R_CMD+=("${CHECK_CMD[$i]}")
done

# ---------------------------------------------------------------------------
# Summary, report, exit code.
# ---------------------------------------------------------------------------
fails=0 not_asked=0
printf '\n== master-security-gate: summary ==\n'
for n in "${!R_ID[@]}"; do
  case "${R_RESULT[$n]}" in
    ok)              printf '  ok              %-24s %s\n' "${R_ID[$n]}" "${R_NAME[$n]}" ;;
    FAIL)            printf '  FAIL            %-24s %s\n' "${R_ID[$n]}" "${R_REASON[$n]}"; fails=$((fails+1)) ;;
    UNSUBSTANTIATED) printf '  UNSUBSTANTIATED %-24s %s\n' "${R_ID[$n]}" "${R_REASON[$n]}"; fails=$((fails+1)) ;;
    "NOT ASKED")     printf '  NOT ASKED       %-24s %s\n' "${R_ID[$n]}" "${R_REASON[$n]}"; not_asked=$((not_asked+1)) ;;
  esac
done
if [ -n "$ONLY" ]; then
  printf '  (PARTIAL RUN — --only %s; the rows not listed were not run at all)\n' "$ONLY"
fi

report="${REPORT:-$WORK/master-gate-report.json}"
{
  printf '{"version":1,"root":"%s","partial":%s,"allow_not_asked":%s,' "$(jesc "$ROOT")" \
    "$( [ -n "$ONLY" ] && printf true || printf false )" \
    "$( [ "$ALLOW_NOT_ASKED" = 1 ] && printf true || printf false )"
  printf '"checks":['
  for n in "${!R_ID[@]}"; do
    [ "$n" = 0 ] || printf ','
    printf '{"id":"%s","name":"%s","result":"%s","exit_code":%s,"reason":"%s","command":"%s"}' \
      "$(jesc "${R_ID[$n]}")" "$(jesc "${R_NAME[$n]}")" "$(jesc "${R_RESULT[$n]}")" \
      "$( [ -n "${R_RC[$n]}" ] && printf '%s' "${R_RC[$n]}" || printf null )" \
      "$(jesc "${R_REASON[$n]}")" "$(jesc "${R_CMD[$n]}")"
  done
  printf '],"not_asked":['
  for n in "${!NA_ID[@]}"; do
    [ "$n" = 0 ] || printf ','
    printf '{"id":"%s","reason":"%s","command":"%s"}' \
      "$(jesc "${NA_ID[$n]}")" "$(jesc "${NA_REASON[$n]}")" "$(jesc "${NA_CMD[$n]}")"
  done
  printf '],"static_absence_manifest":"ops/security/absent-checks.json"}\n'
} >"$report"

if [ "$fails" -ne 0 ]; then
  printf '\nmaster-security-gate: FAILED — %d check(s) red (report: %s)\n' "$fails" "$report" >&2
  exit $EXIT_FAILED
fi
if [ "$not_asked" -ne 0 ]; then
  if [ "$ALLOW_NOT_ASKED" = 1 ]; then
    printf '\nmaster-security-gate: NOT A FULL GREEN — %d check(s) NOT ASKED and acknowledged (--allow-not-asked).\n' "$not_asked" >&2
    printf '  A Release gate must not be run with that flag: see docs/23 §11 and ops/security/absent-checks.json.\n' >&2
    printf '  report: %s\n' "$report" >&2
    exit $EXIT_PASS
  fi
  printf '\nmaster-security-gate: NOT ASKED — %d check(s) could not be asked here, so this is not a green gate (report: %s)\n' "$not_asked" "$report" >&2
  printf '  Re-run where the prerequisites named above exist, or acknowledge explicitly with --allow-not-asked.\n' >&2
  exit $EXIT_NOT_ASKED
fi
printf '\nmaster-security-gate: PASS — %d check(s) ran and each printed its own evidence (report: %s)\n' "${#R_ID[@]}" "$report"
exit $EXIT_PASS
