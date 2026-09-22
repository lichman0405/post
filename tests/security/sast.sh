#!/usr/bin/env bash
#
# SAST — the static application security testing face of the Master
# Security/Quality Gate (docs/23_SECURITY_PRIVACY.md §11 "SAST").
#
#   bash tests/security/sast.sh go       # gosec over the whole Go module
#   bash tests/security/sast.sh python   # bandit over the scientific adapter
#   bash tests/security/sast.sh node     # eslint + eslint-plugin-security over apps/web and packages/ui
#
# Three rows of tests/security/master-security-gate.sh call this script, one
# per language, so a red Go scan cannot be hidden behind a green Node one.
#
# WHAT MAKES A ROW HERE MEAN SOMETHING
# ------------------------------------
#  1. THE TOOL IS PINNED, AND THE PIN IS CHECKED BEFORE THE SCAN. The version
#     this script asserts comes from ops/security/tool-versions.sh; `make
#     security-tools` installs exactly those versions. A binary whose version
#     is not the pin is a FAILED row, not a warning: "gosec was green" has to
#     mean a specific scanner.
#  2. EXIT 0 FROM THE SCANNER IS NOT THE VERDICT. The report is parsed
#     (tests/security/sast_report.py), the file count must be positive, and
#     every finding must either be new (red) or be one finding in the
#     per-finding baseline with a written review attached. A scan that covered
#     nothing is red, and a rule set that failed to load is red — the two ways
#     this row could have gone quietly green.
#  3. NO EXCLUSION FLAGS. The invocations below are the whole invocation: no
#     -exclude-dir, no -nosec, no severity floor, no rule disabled. The
#     baselines record findings the reviewer judged individually, not classes
#     of finding — sast_report.py rejects a baseline key that names a
#     directory, a glob or a rule without a file and line.
#  4. THE TARGET IS PRINTED. Each row prints what it scanned, so an override
#     (POST_SAST_*_TARGET, the hook the mutation check uses to scan a planted
#     fixture) shows up in the row's own output instead of changing what
#     "green" means in silence. The Node face's override has to point INSIDE
#     the repository: eslint refuses to lint a directory outside the base path
#     it was started in ("all of the files matching the glob pattern ... are
#     ignored", exit 2), so a planted TypeScript fixture for that face lives in
#     a temporary directory in the tree and is removed again, while the Go and
#     Python faces take any directory — their tools do not have that rule.
#
# Exit codes
#   0  the pinned tool ran over the target, and every finding it reported is
#      baselined with a review
#   1  a finding is not baselined, the tool is not the pinned version, the
#      scan covered nothing, or the baseline file itself is unusable
#   2  usage error, or a tool/file this row needs is missing (`make security-tools`)
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

VERSIONS="$ROOT/ops/security/tool-versions.sh"
# shellcheck source=/dev/null
. "$VERSIONS" || { echo "sast: cannot read $VERSIONS" >&2; exit 2; }

# Floors on the scanned surface. They exist because every way this row could
# go quietly green without being told to is a way of scanning nothing: an
# empty target, a target list that silently matched no source, a tool whose
# file walker changed. They apply to the DEFAULT surface only — a mutation run
# points the row at a planted fixture on purpose, and prints the target it
# used. (2026-09-23, the counts the three rows print on this tree: gosec 797
# files, eslint 129, bandit 8. They are the observed numbers, printed by the
# rows themselves — not a list this comment maintains.)
#
# Each face enforces its own floor inline, right after its scanner writes its
# report and before the report is judged — it is bound to that face's report
# format (gosec's Stats.files, bandit's metrics keys, eslint's file array), so
# a shared helper here could only ever have restated one of them. A shell
# floor helper used to sit at this point; it was defined and never called (the
# three faces had each grown their own copy in Python), so it was removed
# rather than left as a second, dead statement of the rule — one copy that
# runs, not two that have to be kept in step.
FLOOR_GO_FILES=400
FLOOR_NODE_FILES=50
FLOOR_PY_FILES=5

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

die() { echo "sast: $*" >&2; exit 1; }
usage() { sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' | sed '/^$/q'; exit 2; }

have() { command -v "$1" >/dev/null 2>&1; }

report() { # report <tool> <report.json> <baseline> <id> <version> [rule-count]
  local tool="$1" file="$2" baseline="$3" id="$4" version="$5" rules="${6:-0}"
  [ -f "$file" ] || die "$id: the scanner wrote no report at $file — it did not measure anything"
  python3 tests/security/sast_report.py \
    --tool "$tool" --report "$file" --baseline "$baseline" --id "$id" --version "$version" --rules "$rules"
}

case "${1:-}" in
  go)
    have go || die "go is not on PATH (https://go.dev/dl)"
    BIN="$(command -v gosec)" || die "gosec is not on PATH — run 'make security-tools' (pins $GOSEC_VERSION)"
    # gosec's own -version prints "dev" for a `go install`ed binary: the module
    # version lives in the binary's build info, which is the only honest source.
    HAVE="$(go version -m "$BIN" 2>/dev/null | awk '$1=="mod" && $2=="github.com/securego/gosec/v2" {print $3}')"
    [ "$HAVE" = "$GOSEC_VERSION" ] || die "gosec at $BIN is ${HAVE:-unknown}, the pin is $GOSEC_VERSION \
(ops/security/tool-versions.sh). Run 'make security-tools'. A scanner whose version floated is a \
verdict from an unknown instrument."
    TARGET="${POST_SAST_GO_TARGET:-.}"
    [ -d "$TARGET" ] || die "sast go: no such directory to scan: $TARGET"
    echo "     target: $TARGET (POST_SAST_GO_TARGET; the default is the repository root)"
    # The `/...` suffix is not decoration: `gosec .` loads the one package in the
    # current directory (at this root: a package with no Go files) and writes no
    # report at all, which the row then reports as "the scanner measured nothing".
    gosec -fmt=json -out="$WORK/gosec.json" -quiet "${TARGET%/}/..." >"$WORK/gosec.log" 2>&1
    rc=$?
    if [ "$rc" -gt 1 ]; then
      tail -20 "$WORK/gosec.log" | sed 's/^/     /'
      die "sast-go: gosec exited $rc (it exits 1 when it finds issues; anything else is the tool failing)"
    fi
    python3 - "$WORK/gosec.json" "$TARGET" "$FLOOR_GO_FILES" "${POST_SAST_GO_TARGET:-}" <<'PY'
import json, os, sys
report, target, floor, override = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]
try:
    files = int((json.load(open(report)).get("Stats") or {}).get("files") or 0)
except Exception as exc:
    sys.exit(f"sast: the gosec report at {report} is unreadable ({exc})")
if not override and files < floor:
    sys.exit(
        f"sast-go: {target} yielded {files} file(s) to scan, and this row requires at least {floor} "
        "when it scans the repository's own source. A scan of an empty tree reports no findings, "
        "which is indistinguishable from a clean one."
    )
PY
    [ $? -eq 0 ] || exit 1
    report gosec "$WORK/gosec.json" ops/ci/gosec-baseline.txt sast-go "$GOSEC_VERSION" || exit 1
    ;;

  python)
    have bandit || die "bandit is not on PATH — run 'make security-tools' (pins $BANDIT_VERSION)"
    HAVE="$(bandit --version 2>&1 | awk '$1=="bandit" {print $2; exit}')"
    [ "$HAVE" = "$BANDIT_VERSION" ] || die "bandit on PATH is ${HAVE:-unknown}, the pin is $BANDIT_VERSION \
(ops/security/tool-versions.sh). Run 'make security-tools'."
    TARGET="${POST_SAST_PY_TARGET:-services/scientific-adapter/src}"
    [ -d "$TARGET" ] || die "sast python: no such directory to scan: $TARGET"
    echo "     target: $TARGET (POST_SAST_PY_TARGET; the default is the adapter's source)"
    # -r walks the target; no -s (skip), no -t/-s rule selection, no #nosec.
    bandit -r "$TARGET" -f json -o "$WORK/bandit.json" -q >"$WORK/bandit.log" 2>&1
    rc=$?
    if [ "$rc" -gt 1 ]; then
      tail -20 "$WORK/bandit.log" | sed 's/^/     /'
      die "sast-python: bandit exited $rc (it exits 1 when it finds issues; anything else is the tool failing)"
    fi
    python3 - "$WORK/bandit.json" "$TARGET" "$FLOOR_PY_FILES" "${POST_SAST_PY_TARGET:-}" <<'PY'
import json, sys
report, target, floor, override = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]
data = json.load(open(report))
files = len([k for k in (data.get("metrics") or {}) if k != "_totals"])
if not override and files < floor:
    sys.exit(
        f"sast-python: {target} yielded {files} file(s) to scan, and this row requires at least "
        f"{floor} when it scans the repository's own source. A scan of an empty tree reports no "
        "findings, which is indistinguishable from a clean one."
    )
PY
    [ $? -eq 0 ] || exit 1
    report bandit "$WORK/bandit.json" ops/ci/bandit-baseline.txt sast-python "$BANDIT_VERSION" || exit 1
    ;;

  node)
    have node || die "node is not on PATH"
    TOOLS="$ROOT/tests/security/node-tools"
    ESLINT="$TOOLS/node_modules/.bin/eslint"
    [ -x "$ESLINT" ] || die "the Node SAST toolchain is not installed at $TOOLS (run 'make security-tools')"
    HAVE_ESLINT="$(node -p "require('$TOOLS/node_modules/eslint/package.json').version" 2>/dev/null)"
    HAVE_PLUGIN="$(node -p "require('$TOOLS/node_modules/eslint-plugin-security/package.json').version" 2>/dev/null)"
    HAVE_TSESLINT="$(node -p "require('$TOOLS/node_modules/typescript-eslint/package.json').version" 2>/dev/null)"
    [ "$HAVE_ESLINT" = "$ESLINT_VERSION" ] || die "eslint at $TOOLS is ${HAVE_ESLINT:-unknown}, the pin is \
$ESLINT_VERSION. Run 'make security-tools'."
    [ "$HAVE_PLUGIN" = "$ESLINT_PLUGIN_SECURITY_VERSION" ] || die "eslint-plugin-security at $TOOLS is \
${HAVE_PLUGIN:-unknown}, the pin is $ESLINT_PLUGIN_SECURITY_VERSION. Run 'make security-tools'."
    [ "$HAVE_TSESLINT" = "$TYPESCRIPT_ESLINT_VERSION" ] || die "typescript-eslint at $TOOLS is \
${HAVE_TSESLINT:-unknown}, the pin is $TYPESCRIPT_ESLINT_VERSION. Run 'make security-tools'."
    # The rule count comes out of the config file itself, so the row's evidence
    # line reports the size of the rule set it actually ran with.
    RULES="$(node -e "import('file://$TOOLS/eslint-sast.config.mjs').then(m => console.log(m.sastRuleCount)).catch(e => { console.error(e.message); process.exit(1) })")" \
      || die "the SAST eslint config at $TOOLS/eslint-sast.config.mjs could not be loaded"
    OVERRIDE="${POST_SAST_NODE_TARGET:-}"
    # shellcheck disable=SC2086
    TARGET="${OVERRIDE:-apps/web packages/ui}"
    for d in $TARGET; do
      [ -d "$d" ] || die "sast node: no such directory to lint: $d"
    done
    echo "     target: $TARGET (POST_SAST_NODE_TARGET; the default is the web app and the shared UI package)"
    echo "     rules: $RULES from eslint-plugin-security/recommended, every one of them at \"error\""
    # shellcheck disable=SC2086
    "$ESLINT" --no-config-lookup --config "$TOOLS/eslint-sast.config.mjs" \
      --format json --output-file "$WORK/eslint.json" $TARGET >"$WORK/eslint.log" 2>&1
    rc=$?
    if [ "$rc" -gt 1 ]; then
      tail -20 "$WORK/eslint.log" | sed 's/^/     /'
      die "sast-node: eslint exited $rc (1 = findings, 2 = the run itself failed; 2 means nothing was measured)"
    fi
    python3 - "$WORK/eslint.json" "$TARGET" "$FLOOR_NODE_FILES" "$OVERRIDE" <<'PY'
import json, sys
report, target, floor, override = sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4]
try:
    data = json.load(open(report))
except Exception as exc:
    sys.exit(f"sast: the eslint report at {report} is unreadable ({exc})")
files = len(data)
if not override and files < floor:
    sys.exit(
        f"sast-node: {target} yielded {files} file(s) to lint, and this row requires at least "
        f"{floor} when it lints the repository's own source. A lint of an empty tree reports no "
        "findings, which is indistinguishable from a clean one."
    )
PY
    [ $? -eq 0 ] || exit 1
    report eslint "$WORK/eslint.json" ops/ci/eslint-security-baseline.txt sast-node "$ESLINT_VERSION" "$RULES" || exit 1
    ;;

  ""|-h|--help) usage;;
  *) echo "sast: unknown language '$1' (expected go, python or node)" >&2; exit 2;;
esac
