#!/usr/bin/env bash
#
# T1220 — what makes the SAST go row's SURFACE a fact instead of a claim.
#
# `tests/security/sast.sh go` used to be handed `./...` from the repository
# root. gosec resolves that with its own filesystem walk, and that walk
# descends into directories Go's package patterns skip — dot-directories
# included. On a working copy it reached `.rddev/`: the orchestrator's
# rebaseline drafts and other Workers' worktrees are copies of this tree's
# source and of source nobody had committed yet, and the row reported their
# findings as this tree's. CI stayed green (a checkout has no `.rddev/`), so
# the defect was visible only where somebody ran the gate by hand.
#
# The row now derives its surface from the toolchain — the module's package
# set, as `go list ./...` reports it, minus the package directories git
# ignores — and then checks every file it is about to scan: under the
# repository root, and not something git ignores. There are two answers to an
# ignored path, and they are not the same answer:
#
#   * a package DIRECTORY git ignores is not this repository's source, so it
#     is out of the surface. This is not hypothetical: with the pinned tools
#     installed (`make security-tools`, which CI runs before the gate) the npm
#     package `flatted` ships a Go file under tests/security/node-tools/
#     node_modules/, and the toolchain returns it as a package of this module
#     (784 Go files against 783 without it). Refusing it would red every tree
#     that installed the tools; scanning it would judge a dependency's file
#     against this repository's baseline. The row drops it and PRINTS it, with
#     the .gitignore rule that dropped it, so the surface is short by exactly
#     what the log names.
#   * a FILE git ignores inside a package directory that stays in the surface
#     is refused: the row stops before the scanner runs and reports a failed
#     surface naming it. That is the lock — it is what an edit back to a walk
#     from the root, or a new ignore rule over source, has to get past.
#
# This script plants both shapes and requires the row to answer for them,
# because either half alone is a claim:
#
#   1. a clean tree       the row is green and prints the surface it derived
#   2. an ignored copy under an ignored DOT directory (the `.rddev/` shape):
#                         the row is green — the copy is not a package of this
#                         module, so it is not in the surface at all
#   3. an ignored copy under an ignored directory whose name does NOT begin
#                         with a dot: `go list ./...` does return it, so the row
#                         must drop it from the surface AND say so, naming the
#                         path and the rule, while staying green — and the
#                         copy's finding must never come back as a finding of
#                         this tree
#   4. an ignored FILE inside a package directory that is in the surface: the
#                         row must fail as a FAILED SURFACE naming the file,
#                         before the scanner runs — the copy's finding must
#                         never come back as a finding of this tree (that is
#                         the difference between a surface that was checked and
#                         a finding somebody has to judge)
#   5. POST_SAST_GO_TARGET pointing at the planted directory: the row must scan
#                         what it was pointed at, print it, and report the
#                         fixture's finding — the override exists to point this
#                         row at a planted fixture and watch it go red, so it is
#                         deliberately not subject to the surface check, and
#                         says so in its own output
#   6. the plants removed the row is green again, and the tree is left as it
#                         was found
#
# Cases 2-5 ask the instruments their own questions before they judge the row:
# case 3 checks with `git check-ignore` that the planted copy really is
# ignored AND with `go list` that it really is in the derived surface; case 4
# checks that its plant's directory is NOT ignored, that the file inside it IS,
# and that the directory is in the derived surface. A case whose plant stopped
# being ignored, or stopped being scanned, would otherwise pass by measuring
# nothing — which is the failure this script exists to catch, one level down.
#
# Runnable on a host with bash + go + gosec + git (`make security-tools`).
# It runs the go row six times; see tests/security/sast.sh for the row itself.
#
# Exit codes
#   0  every case behaved as above, and the tree was left as it was found
#   1  a case did not
#   2  a tool this script needs is missing (never a silent pass)
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT" || exit 2

# Two plants, both under this repository's own ignore rules. PLANT lives under
# the `node_modules/` rule (.gitignore) at any depth — the same class of path
# as the .rddev/ copies that made the row red on a working copy; DOT_PLANT is
# the same file under a dot-directory; FILE_PLANT is a directory no rule
# ignores, whose single Go file a nested .gitignore of its own does ignore —
# the shape of a generated file inside a package of this repository. Each
# directory is created and removed by this script (see cleanup).
PLANT_PARENT="tests/security/node_modules"
PLANT="$PLANT_PARENT/sast-go-surface-check"      # a package dir the derived surface returns
DOT_PLANT="$PLANT_PARENT/.sast-go-surface-copy"  # the same file, under a dot directory
FILE_PLANT="tests/security/sast-go-surface-check-file"  # a package dir with one ignored file
WORK="$(mktemp -d)"
FAILS=0

die()  { echo "sast-go-surface-check: $*" >&2; exit 2; }
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }
step() { printf '\n== %s ==\n' "$*"; }

cleanup() {
  rm -rf "$PLANT" "$DOT_PLANT" "$FILE_PLANT"
  rmdir "$PLANT_PARENT" 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

for tool in go gosec git python3; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is not on PATH — the row cannot be measured without it"
done
[ -f "$ROOT/tests/security/sast.sh" ] || die "no tests/security/sast.sh at $ROOT"

probe_go() { # probe_go <dir> — a Go file with one finding, the shape of the escape
  cat >"$1/probe.go" <<'GO'
// Planted by tests/security/sast-go-surface-check.sh. A copy of repository
// source in a directory this repository ignores — the shape the SAST go row
// has to answer for. Removed again by that script's exit trap.
package surfaceprobe

import "os"

func CopiedSource(path string) ([]byte, error) {
	return os.ReadFile(path)
}
GO
}
plant() { mkdir -p "$1"; probe_go "$1"; }
plant_ignored_file() { # the dir is not ignored; the file inside it is
  mkdir -p "$1"
  printf '*.go\n' >"$1/.gitignore"
  probe_go "$1"
}

ROW_RC=0 ROW_LOG=""
run_row() { # run_row <name> [POST_SAST_GO_TARGET]
  local name="$1" target="${2:-}"
  ROW_LOG="$WORK/$name.log"
  if [ -n "$target" ]; then
    POST_SAST_GO_TARGET="$target" bash tests/security/sast.sh go >"$ROW_LOG" 2>&1
  else
    bash tests/security/sast.sh go >"$ROW_LOG" 2>&1
  fi
  ROW_RC=$?
  printf '     [%s] row exited %d\n' "$name" "$ROW_RC"
}
show_row() { sed 's/^/     | /' "$ROW_LOG" | tail -14; }
row_fails() { fail "$1"; show_row; }

ignored() { git -C "$ROOT" check-ignore -q -- "$1"; }         # 0 when git ignores it
in_surface() { go list -e -f '{{.Dir}}' ./... 2>/dev/null | grep -Fxq "$ROOT/$1"; }
reported_as_finding() { # 0 when the row printed a finding line for this path
  grep -qE "^  $1:[0-9]+:[0-9]+: [A-Z]" "$ROW_LOG"
}

# ---------------------------------------------------------------------------
step "0. the tree this script was pointed at"
if git -C "$ROOT" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  ok "a git work tree: $(printf '%s' "$ROOT")"
else
  die "$ROOT is not a git work tree — the go row tells this repository's source from the local state
beside it with git, and this script measures that row"
fi
cleanup_plants() { rm -rf "$PLANT" "$DOT_PLANT" "$FILE_PLANT"; }
cleanup_plants

# ---------------------------------------------------------------------------
step "1. a clean tree: the row is green and prints the surface it derived"
run_row clean
if [ "$ROW_RC" -ne 0 ]; then
  row_fails "case 1: the go row is not green on a tree with no planted copy"
elif ! grep -q "target: this module's own packages" "$ROW_LOG"; then
  row_fails "case 1: the row did not print the surface it derived from 'go list ./...'"
elif ! grep -qE '0 unbaselined' "$ROW_LOG"; then
  row_fails "case 1: the row did not report zero unbaselined findings"
else
  ok "case 1: green, and its own line says what it scanned"
  grep -E 'target: this module|^ok   sast-go' "$ROW_LOG" | sed 's/^/     /'
fi

# ---------------------------------------------------------------------------
step "2. the .rddev/ shape: a copy under an ignored DOT directory is not in the surface"
plant "$DOT_PLANT"
if ! ignored "$DOT_PLANT/probe.go"; then
  fail "case 2: the planted copy at $DOT_PLANT/probe.go is not git-ignored, so this case measures nothing"
elif in_surface "$DOT_PLANT"; then
  fail "case 2: the planted copy at $DOT_PLANT IS in the derived surface — the derivation does not skip dot-directories"
else
  run_row dotdir
  if [ "$ROW_RC" -eq 0 ] && ! grep -q "$DOT_PLANT/probe.go" "$ROW_LOG"; then
    ok "case 2: green, and the copy under the dot directory was not scanned"
  else
    row_fails "case 2: the row did not stay green with a copy under an ignored dot directory"
  fi
fi
rm -rf "$DOT_PLANT"

# ---------------------------------------------------------------------------
step "3. an ignored directory that is NOT a dot directory: dropped from the surface, and named"
plant "$PLANT"
if ! ignored "$PLANT/probe.go"; then
  fail "case 3: the planted copy at $PLANT/probe.go is not git-ignored, so this case measures nothing"
elif ! in_surface "$PLANT"; then
  fail "case 3: the planted copy at $PLANT is not in the derived surface, so the row cannot be asked about it"
else
  run_row dropped-dir
  if [ "$ROW_RC" -ne 0 ]; then
    row_fails "case 3: the row is not green with an ignored package directory — the toolchain returns installed dependencies as packages of this module, so refusing them is a red no checkout can fix"
  elif ! grep -qF "$PLANT" "$ROW_LOG"; then
    row_fails "case 3: the row was green without naming the package directory it dropped — a surface that shrank in silence"
  elif ! grep -qF "$PLANT  — git-ignored by" "$ROW_LOG"; then
    row_fails "case 3: the row named the dropped directory without the rule that dropped it"
  elif reported_as_finding "$PLANT/probe.go"; then
    row_fails "case 3: the copy's finding was reported as a finding of this tree"
  else
    ok "case 3: green, the ignored directory is out of the surface, and the row's output names it (and the rule)"
    grep -E "^     surface: NOT scanned|^       $PLANT" "$ROW_LOG" | sed 's/^/     /'
  fi
fi

# ---------------------------------------------------------------------------
step "4. an ignored FILE inside a package that stays: the surface fails by name"
plant_ignored_file "$FILE_PLANT"
if ignored "$FILE_PLANT"; then
  fail "case 4: the plant's directory $FILE_PLANT is git-ignored, so this case would measure case 3 again"
elif ! ignored "$FILE_PLANT/probe.go"; then
  fail "case 4: the plant's file $FILE_PLANT/probe.go is not git-ignored, so this case measures nothing"
elif ! in_surface "$FILE_PLANT"; then
  fail "case 4: the plant's directory $FILE_PLANT is not in the derived surface, so the row never sees its file"
else
  run_row escape
  if [ "$ROW_RC" -eq 0 ]; then
    row_fails "case 4: the row passed with a git-ignored file inside the surface it was about to scan"
  elif ! grep -q 'SCAN SURFACE FAILED' "$ROW_LOG"; then
    row_fails "case 4: the row failed, but not as a failed surface"
  elif ! grep -qF "$FILE_PLANT/probe.go" "$ROW_LOG"; then
    row_fails "case 4: the row refused the surface without naming the file that left the repository"
  elif reported_as_finding "$FILE_PLANT/probe.go"; then
    row_fails "case 4: the ignored file's finding was reported as a finding of this tree — the surface check ran too late"
  else
    ok "case 4: refused as a failed surface, naming the ignored file (its finding was never reported)"
    grep -E 'SCAN SURFACE FAILED|git-ignored by' "$ROW_LOG" | sed 's/^/     /'
  fi
fi

# ---------------------------------------------------------------------------
step "5. POST_SAST_GO_TARGET: the row scans what it is pointed at, and prints it"
run_row override "$PLANT"
if [ "$ROW_RC" -eq 0 ]; then
  row_fails "case 5: the row passed while pointed at a fixture that carries a finding"
elif ! grep -qF "target: $PLANT" "$ROW_LOG"; then
  row_fails "case 5: the row did not print the target it was pointed at"
elif ! reported_as_finding "$PLANT/probe.go"; then
  row_fails "case 5: the row did not report the fixture's finding — it scanned something else"
elif grep -q 'SCAN SURFACE FAILED' "$ROW_LOG"; then
  row_fails "case 5: the surface check was applied to a deliberate fixture run — POST_SAST_GO_TARGET is not usable"
elif ! grep -q 'is not applied to it' "$ROW_LOG"; then
  row_fails "case 5: the row did not say that the surface check is relaxed for an override"
else
  ok "case 5: scanned the fixture, reported its finding, and said why the surface check is off"
  grep -E "target: |is not applied to it|^FAIL sast-go" "$ROW_LOG" | sed 's/^/     /'
fi

# ---------------------------------------------------------------------------
step "6. the plants removed: the row is green again, and the tree is as it was found"
rm -rf "$PLANT" "$DOT_PLANT" "$FILE_PLANT"
run_row clean-again
if [ "$ROW_RC" -ne 0 ]; then
  row_fails "case 6: the row is still red after removing the planted copies — the red above was not caused by them"
elif [ -e "$PLANT" ] || [ -e "$DOT_PLANT" ] || [ -e "$FILE_PLANT" ]; then
  fail "case 6: this script left a planted copy behind"
else
  ok "case 6: green again, and all three plants are gone"
fi

# ---------------------------------------------------------------------------
printf '\n== sast-go-surface-check: summary ==\n'
if [ "$FAILS" -gt 0 ]; then
  printf 'sast-go-surface-check: FAILED — %d finding(s)\n' "$FAILS" >&2
  exit 1
fi
printf 'sast-go-surface-check: OK — six cases: the derived surface is green and prints itself, a copy\n'
printf 'under an ignored dot directory is out of it, a package directory git ignores is dropped and\n'
printf 'named with its rule, an ignored file inside a package that stays fails the surface by name,\n'
printf 'POST_SAST_GO_TARGET still scans and prints what it is pointed at, and the tree was left as it\n'
printf 'was found\n'
