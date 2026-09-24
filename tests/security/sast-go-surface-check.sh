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
#   7. a file git RE-INCLUDES: `*.go` + `!keep.go` in a package directory that
#                         stays in the surface. git does NOT ignore keep.go —
#                         `git check-ignore -q` says so — and yet
#                         `git check-ignore --stdin -v` prints a line for it
#                         whose pattern field starts with `!`, and exits 0.
#                         A reader that counts every printed line as a hit
#                         refuses that file and the surface with it: a red row
#                         on a tree that is fine (2026-09-24, T1222)
#   8. a package DIRECTORY git RE-INCLUDES (`negpkg/` + `!negpkg/`): the same
#                         reading one level up, in the direction that loses
#                         coverage — counted as a hit, a package of this module
#                         leaves the surface while the row prints a rule that
#                         says the opposite of what git said. The case also
#                         requires the finding of a file in that directory to
#                         come back from the scanner, so "it is in the surface"
#                         is measured and not inferred from a green row
#   9. a Go file under `testdata/`: the toolchain's package PATTERN never names
#                         such a directory, so the fixture is outside the
#                         surface (see the note in tests/security/sast.sh) —
#                         and the case proves the row reaches that package by
#                         moving the same file one directory up and requiring
#                         the finding back. The 17 tracked Go files under
#                         testdata/ are the real case of this, and the verdict
#                         is the row's, not this script's
#  10. the plants of 7-9 removed: the row is green again and nothing is left in
#                         the tree — each of those plants carries a .gitignore
#                         of its own, and one left behind would be read as the
#                         tree's state by every later run
#
# Cases 2-5 ask the instruments their own questions before they judge the row:
# case 3 checks with `git check-ignore` that the planted copy really is
# ignored AND with `go list` that it really is in the derived surface; case 4
# checks that its plant's directory is NOT ignored, that the file inside it IS,
# and that the directory is in the derived surface. A case whose plant stopped
# being ignored, or stopped being scanned, would otherwise pass by measuring
# nothing — which is the failure this script exists to catch, one level down.
# Cases 7-9 ask them the same way, and ask one more thing: that git really
# prints the shape the case is about (`negation_line_for`) and that the plant's
# file really is one the row feeds its surface check (`in_go_files`). A case
# about a `!` line on a tree where git stopped printing one would pass without
# ever exercising the reading it exists for.
#
# Runnable on a host with bash + go + gosec + git (`make security-tools`).
# It runs the go row ten times; see tests/security/sast.sh for the row itself.
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
# The three plants of cases 7-9: the shapes a NEGATED .gitignore pattern makes.
# Each one is a package of this module that the derived surface returns, so the
# row has to answer for it, and each one's own .gitignore is what makes it.
NEG_FILE_PLANT="tests/security/sast-go-surface-check-neg"     # *.go + !keep.go, keep.go beside _drop.go
NEG_DIR_PARENT="tests/security/sast-go-surface-check-negdir"  # negpkg/ + !negpkg/
NEG_DIR_PLANT="$NEG_DIR_PARENT/negpkg"                        # the directory git re-includes
TD_PLANT="tests/security/sast-go-surface-check-testdata"      # a package dir with a testdata/ of its own
WORK="$(mktemp -d)"
FAILS=0

die()  { echo "sast-go-surface-check: $*" >&2; exit 2; }
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }
step() { printf '\n== %s ==\n' "$*"; }

cleanup() {
  rm -rf "$PLANT" "$DOT_PLANT" "$FILE_PLANT" \
         "$NEG_FILE_PLANT" "$NEG_DIR_PARENT" "$TD_PLANT"
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

# A Go file that carries NO finding, in a package of its own name. Cases 7-9
# plant these in package directories the row is expected to scan and stay
# green on: a plant that carried a finding would red the row for a reason that
# has nothing to do with what the case is asking.
clean_go() { # clean_go <file> <package>
  cat >"$1" <<GO
// Planted by tests/security/sast-go-surface-check.sh. Removed again by that
// script's exit trap.
package $2

func Keep() int { return 1 }
GO
}
plant_negated_file() { # *.go + !keep.go, with an ignored _drop.go beside keep.go
  mkdir -p "$1"
  printf '*.go\n!keep.go\n' >"$1/.gitignore"
  clean_go "$1/keep.go" negfileprobe
  # The file the pattern really does exclude is named `_drop.go` on purpose:
  # git ignores it (checked below), and Go's own rule — a file whose name
  # begins with `_` is not a source file of the package — keeps it out of the
  # file list the row feeds its surface check. A file that git ignores AND
  # the toolchain returns would be refused by the row for the right reason
  # (case 4), so it could not be the second half of THIS case: what case 7
  # asks is whether a path git RE-INCLUDED is read as an escape, and a plant
  # that is refused for a real exclusion measures the opposite.
  clean_go "$1/_drop.go" negfileprobe
}
plant_negated_dir() { # a package directory that is excluded and then re-included
  mkdir -p "$1"
  printf 'negpkg/\n!negpkg/\n' >"$(dirname "$1")/.gitignore"
  clean_go "$1/keep.go" negdirprobe
}
finding_go() { # finding_go <file> <package> — a Go file carrying one gosec finding
  cat >"$1" <<GO
// Planted by tests/security/sast-go-surface-check.sh. Removed again by that
// script's exit trap.
package $2

import "os"

func CopiedSource(path string) ([]byte, error) {
	return os.ReadFile(path)
}
GO
}
plant_testdata() { # a package dir the row scans, with a testdata/ dir it does not
  # The two files are the SAME package and the same shape; the only difference
  # between them is the directory one of them sits in. Case 9 moves the one
  # under testdata/ up beside the other and requires the row to report it
  # there — which is what makes "testdata is out of the surface" a measurement
  # rather than a reading of the row's comment.
  mkdir -p "$1/testdata"
  clean_go "$1/probe.go" tdprobe
  finding_go "$1/testdata/planted.go" tdprobe
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
in_go_files() { # 0 when `go list` returns this file as a source of its package
  # The row's own file set: the same expression tests/security/sast.sh feeds
  # its surface check (`{{range .GoFiles}}`), so a case that needs its plant to
  # REACH that check can say so instead of assuming it.
  go list -e -f '{{range .GoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}' ./... 2>/dev/null | grep -Fxq "$ROOT/$1"
}
negation_line_for() { # 0 when git prints a RE-INCLUSION ('!') line for this path
  # The shape the reader in tests/security/sast.sh has to survive, asked of git
  # directly and not of the row: `git check-ignore --stdin -v` answers a path
  # whose last matching rule is a negation with a line whose pattern field
  # starts with `!` — while `git check-ignore -q` on that same path exits 1,
  # because the path is NOT ignored. A case planted on a tree where git stopped
  # printing that line would otherwise pass by measuring nothing.
  local path="${1%/}" line out
  out="$(printf '%s\n' "$path" | git -C "$ROOT" check-ignore --stdin -v 2>/dev/null)"
  while IFS= read -r line; do
    [ "${line##*$'\t'}" = "$path" ] || continue
    case "${line%%$'\t'*}" in *':!'*) return 0;; esac
  done <<<"$out"
  return 1
}
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
step "7. a file git RE-INCLUDES is not an escape: '*.go' + '!keep.go'"
# `git check-ignore -v` prints a line for a path whose last matching pattern is
# a re-inclusion, and the command exits 0 — while `git check-ignore -q` on that
# same path exits 1, because the path is NOT ignored. A reader that counts every
# printed line as a hit refuses a file git never ignored, and refuses the whole
# surface with it: a red row on a tree that is fine (2026-09-24, T1222).
plant_negated_file "$NEG_FILE_PLANT"
if ! in_surface "$NEG_FILE_PLANT"; then
  fail "case 7: the plant's package directory $NEG_FILE_PLANT is not in the derived surface, so the row never asks git about its files"
elif ! ignored "$NEG_FILE_PLANT/_drop.go"; then
  fail "case 7: '*.go' no longer ignores $NEG_FILE_PLANT/_drop.go, so the re-inclusion is not being asked against a real exclusion"
elif ignored "$NEG_FILE_PLANT/keep.go"; then
  fail "case 7: git ignores $NEG_FILE_PLANT/keep.go too, so '!keep.go' re-included nothing and this case measures nothing"
elif ! negation_line_for "$NEG_FILE_PLANT/keep.go"; then
  fail "case 7: git prints no re-inclusion ('!') line for $NEG_FILE_PLANT/keep.go, so the reading this case exists for is not exercised at all"
elif ! in_go_files "$NEG_FILE_PLANT/keep.go"; then
  fail "case 7: $NEG_FILE_PLANT/keep.go is not in the file set the row feeds its surface check, so its answer cannot be observed"
else
  run_row negated-file
  if [ "$ROW_RC" -ne 0 ]; then
    row_fails "case 7: the row failed on a file git explicitly does NOT ignore — a re-included path ('!keep.go') read as an escape"
  elif grep -qF "$NEG_FILE_PLANT/keep.go" "$ROW_LOG"; then
    row_fails "case 7: the row named the re-included file $NEG_FILE_PLANT/keep.go among the paths of a failed surface"
  elif grep -q 'SCAN SURFACE FAILED' "$ROW_LOG"; then
    row_fails "case 7: the row failed its surface, while the only ignored file in the plant is one git ignores for real and Go does not call source"
  else
    ok "case 7: green — the re-included file was not reported as an escape"
    ok "     (git ignores $NEG_FILE_PLANT/_drop.go; it does not ignore keep.go, and the row agrees)"
    grep -E "^     target: this module|^ok   sast-go" "$ROW_LOG" | sed 's/^/     /'
  fi
fi
# Removed here and not at the end, so that a case which fails still leaves the
# tree fit to ask the next question: a plant left standing is read by every
# later run of the row (under a reader that mistakes a `!` line for a hit, its
# keep.go reds EVERY later case, and each one's failure would be about this
# plant instead of its own subject). Case 10 is where the removal is checked.
rm -rf "$NEG_FILE_PLANT"

# ---------------------------------------------------------------------------
step "8. a package directory git RE-INCLUDES stays in the surface: 'negpkg/' + '!negpkg/'"
# The same reading, one level up, and the direction that costs coverage: a
# package directory whose last matching rule is a re-inclusion is source of
# this repository. Counted as a hit it leaves the surface — and the row prints
# a rule that says the opposite of what git said — which is the shrinking
# surface T1220 was about, arriving through the check T1220 added.
plant_negated_dir "$NEG_DIR_PLANT"
if ignored "$NEG_DIR_PLANT"; then
  fail "case 8: git ignores $NEG_DIR_PLANT, so '!negpkg/' re-included nothing and this case measures nothing"
elif ! in_surface "$NEG_DIR_PLANT"; then
  fail "case 8: $NEG_DIR_PLANT is not in the derived surface, so the row never considers it"
elif ! negation_line_for "$NEG_DIR_PLANT"; then
  fail "case 8: git prints no re-inclusion ('!') line for $NEG_DIR_PLANT, so the reading this case exists for is not exercised at all"
else
  # First: the directory really is scanned. The plant carries a finding, so
  # this run is red ON PURPOSE — a row that dropped the directory cannot report
  # the finding, and that is the failure this half is looking for.
  finding_go "$NEG_DIR_PLANT/probe.go" negdirprobe
  run_row negated-dir-scanned
  if grep -qF "$NEG_DIR_PLANT  — git-ignored by" "$ROW_LOG"; then
    row_fails "case 8: the row dropped the re-included package directory from the surface, naming a '!' rule as the one that ignored it"
  elif ! reported_as_finding "$NEG_DIR_PLANT/probe.go"; then
    row_fails "case 8: the row did not report the finding planted in $NEG_DIR_PLANT/probe.go — the re-included package directory never reached the scanner"
  else
    ok "case 8: the re-included package directory was scanned (its planted finding came back)"
    grep -E "^  $NEG_DIR_PLANT/probe.go" "$ROW_LOG" | sed 's/^/     /'
  fi
  # Then: with the finding gone, the row is green and the directory is not
  # named as dropped — the surface it derived is the one it prints.
  rm -f "$NEG_DIR_PLANT/probe.go"
  run_row negated-dir
  if [ "$ROW_RC" -ne 0 ]; then
    row_fails "case 8: the row is not green with a package directory git re-included"
  elif grep -qF "$NEG_DIR_PLANT  — git-ignored by" "$ROW_LOG"; then
    row_fails "case 8: the row dropped the re-included package directory from the surface (printed as NOT scanned)"
  else
    ok "case 8: green, and the re-included package directory stayed in the surface"
    grep -E "^     target: this module|^ok   sast-go" "$ROW_LOG" | sed 's/^/     /'
  fi
fi
rm -rf "$NEG_DIR_PARENT"   # out of the way of case 9, for the reason case 7 gives

# ---------------------------------------------------------------------------
step "9. a Go file under testdata/ is outside the surface — and here is how you would know"
# `go list ./...` is a package PATTERN, and Go's patterns never expand into a
# directory named testdata (Go reserves the name for the go command's own
# data). 17 Go files of this repository are tracked under testdata/ and none of
# them is scanned by the go row; a finding-bearing file inside one of them is
# the measurement of that, and the control below is what keeps it from being a
# reading of the comment: the SAME file, one directory up, must come back as a
# finding. See the note in the go face of tests/security/sast.sh.
plant_testdata "$TD_PLANT"
if ! in_surface "$TD_PLANT"; then
  fail "case 9: the plant's package directory $TD_PLANT is not in the derived surface, so the row never reaches the testdata/ beside it"
elif in_surface "$TD_PLANT/testdata"; then
  fail "case 9: 'go list ./...' now returns $TD_PLANT/testdata as a package of this module, so the premise of the testdata verdict has moved"
elif ignored "$TD_PLANT/testdata/planted.go" || ignored "$TD_PLANT/testdata"; then
  fail "case 9: git ignores the plant's testdata/ (or the file in it), so a green row there would say nothing about the package pattern"
elif ! in_go_files "$TD_PLANT/probe.go"; then
  fail "case 9: $TD_PLANT/probe.go is not in the file set the row feeds its surface check, so the row never looks at this directory"
else
  run_row testdata-fixture
  if [ "$ROW_RC" -ne 0 ]; then
    row_fails "case 9: the row is not green with a finding-bearing Go file under a testdata/ directory — it is being scanned"
  elif grep -qF "$TD_PLANT/testdata/planted.go" "$ROW_LOG"; then
    row_fails "case 9: the row named $TD_PLANT/testdata/planted.go — a testdata fixture is in the surface"
  else
    ok "case 9: green — the fixture under testdata/ was not scanned (the same package has a file the row does read)"
  fi
  # The control: the identical file, moved out of testdata/ into the package
  # itself. If the row reports it here and not there, the difference is the
  # directory name and nothing else — which is the whole of the verdict.
  mv "$TD_PLANT/testdata/planted.go" "$TD_PLANT/planted.go"
  run_row testdata-control
  if reported_as_finding "$TD_PLANT/planted.go"; then
    ok "case 9 control: the same file one directory up IS reported — so the verdict above is about testdata/, not about the row's reach"
    grep -E "^  $TD_PLANT/planted.go" "$ROW_LOG" | sed 's/^/     /'
  elif grep -qF "$TD_PLANT  — git-ignored by" "$ROW_LOG"; then
    row_fails "case 9 control: the row refused the file as an escape instead — $TD_PLANT is git-ignored"
  else
    row_fails "case 9 control: the row did not report the finding of $TD_PLANT/planted.go either, so case 9 measured nothing about the scan surface"
  fi
  rmdir "$TD_PLANT/testdata" 2>/dev/null || true
fi
rm -rf "$TD_PLANT"

# ---------------------------------------------------------------------------
step "10. the plants of cases 7-9: the row is green again, and they are all gone"
# The same last question case 6 asks about its own three plants, asked without
# removing anything first — a cleanup followed by a check that the thing is
# gone checks nothing, and the plants of cases 7-9 are the ones that would
# follow every later run of this instrument (and of the row) if one were left:
# they carry a .gitignore that ignores `*.go`, and one of them an ignored file
# with a finding of its own. So this step asks the tree, not its own rm.
run_row clean-after-negation
if [ "$ROW_RC" -ne 0 ]; then
  row_fails "case 10: the row is not green after cases 7-9 — one of their plants is still in the tree (a file carrying a finding, or a rule that ignores a source file)"
elif [ -e "$NEG_FILE_PLANT" ] || [ -e "$NEG_DIR_PARENT" ] || [ -e "$TD_PLANT" ]; then
  fail "case 10: this script left a plant of cases 7-9 behind"
else
  ok "case 10: green again, and the three plants of cases 7-9 are gone"
fi

# ---------------------------------------------------------------------------
printf '\n== sast-go-surface-check: summary ==\n'
if [ "$FAILS" -gt 0 ]; then
  printf 'sast-go-surface-check: FAILED — %d finding(s)\n' "$FAILS" >&2
  exit 1
fi
printf 'sast-go-surface-check: OK — ten cases: the derived surface is green and prints itself, a copy\n'
printf 'under an ignored dot directory is out of it, a package directory git ignores is dropped and\n'
printf 'named with its rule, an ignored file inside a package that stays fails the surface by name,\n'
printf 'POST_SAST_GO_TARGET still scans and prints what it is pointed at, every plant comes out\n'
printf 'again, a file git RE-INCLUDES is not an escape, a package directory git RE-INCLUDES stays in\n'
printf 'the surface and is scanned, a Go file under testdata/ is not scanned while the same file one\n'
printf 'directory up is, and the tree was left as it was found\n'
