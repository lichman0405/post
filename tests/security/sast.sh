#!/usr/bin/env bash
#
# SAST — the static application security testing face of the Master
# Security/Quality Gate (docs/23_SECURITY_PRIVACY.md §11 "SAST").
#
#   bash tests/security/sast.sh go       # gosec over this module's OWN PACKAGES — see §5
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
#  5. THE GO FACE'S DEFAULT SURFACE IS THIS REPOSITORY'S OWN SOURCE, AND THE
#     SURFACE IS CHECKED. That surface is DERIVED — the module's package set,
#     as `go list ./...` reports it, minus the package directories git ignores
#     (installed and generated state: the pinned tools' node_modules holds an
#     npm package that ships a Go file, which the toolchain returns as a
#     package of this module) — and not a walk from the repository root. The
#     walk is what a scanner does by itself when it is handed `./...`, and
#     gosec's walk goes where Go's package patterns do not: into
#     dot-directories. A working copy's root also holds `.rddev/`, and in it
#     the orchestrator's rebaseline drafts and other Workers' worktrees are
#     COPIES of this tree's source and of source that is not committed yet.
#     Scanned, they arrive as this tree's findings: such a run reds on findings
#     that exist in no commit (a working copy of this repository, 2026-09-24:
#     350 findings, 94 of them copies), while CI — a checkout, no `.rddev/` —
#     stays green, so the defect is visible only where nobody gates on it.
#     Deriving the surface fixes what is scanned; asserting that every file fed
#     to the scanner is under $ROOT and not git-ignored is what keeps it fixed
#     (repo_paths_only below, and the note in the go face). Both halves are
#     needed: only the derivation and the next edit to the invocation can undo
#     it; only the assertion and an ignored file inside a package that stays in
#     the surface arrives as a finding of a tree nobody reviewed. Every package
#     directory the derivation drops is printed with the .gitignore rule that
#     dropped it, so a surface that shrank is a line in the log, not a silence.
#  6. WHAT THE PACKAGE PATTERN LEAVES OUT, AND WHY THOSE FILES ARE NOT SCANNED.
#     `go list ./...` is a PATTERN, and a Go pattern never expands into a
#     directory named `testdata` — the name is reserved for the go command's
#     own data, not for source — so the 17 Go files this repository tracks
#     under `testdata/` (`git ls-files '*.go' | grep -c testdata`, 2026-09-24)
#     are not packages of this module and are not in the surface. The old walk
#     from the repository root did read them; this row does not. That is the
#     intended verdict and not a gap: they are the synthetic fixtures the
#     contract harness is run against — one tree per case
#     (tests/contract/testdata/cases/<case>/tree/api/main.go) and the route
#     forms it enumerates (tests/contract/testdata/forms/api/**), read by
#     tests/contract/*_test.go as INPUT — data for a test, not source of this
#     repository. A finding in one of them would be a finding about a fixture,
#     judged against a baseline whose every line is supposed to be a reviewed
#     finding of this tree; none of them carries one today (0 entries of
#     ops/ci/gosec-baseline.txt name a testdata path, 2026-09-24). What makes
#     that a measurement rather than a reading of this paragraph: gosec is
#     handed package DIRECTORIES and reads the files of those packages. A Go
#     file carrying a G304 finding, planted in a `testdata/` directory of a
#     package this row does scan, was NOT scanned — the row stayed green and
#     its count did not move (783 files with the plant and without it,
#     2026-09-24) — while the same file IS reported and the row goes red the
#     moment the row is pointed at a directory with `/...`
#     (POST_SAST_GO_TARGET), which is where gosec walks. Case 9 of
#     tests/security/sast-go-surface-check.sh re-runs that pair on every use of
#     the instrument, and moves the same file one directory up to show it
#     reported there.
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
# files, eslint 129, bandit 8. 2026-09-24, after the go row's surface became
# this module's packages — the set `go list ./...` names, minus the package
# directories git ignores — instead of a walk from the repository root: gosec
# 783. They are the observed numbers, printed by the rows themselves — not a
# list this comment maintains.)
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

# The help text is this file's own header comment, printed as written: from
# the line after the shebang to the first line that is not a comment. It used
# to be `sed -n '2,30p'` cut at the first blank line, and line 2 is a bare `#`
# — so `--help` printed nothing at all, and the one-line description of the go
# face's surface lived inside text no user could see (2026-09-24, T1222).
# A help text that cannot print is a claim nobody can read, which is how the
# line stayed wrong: it now prints the whole block, blank lines included.
usage() {
  awk 'NR == 1 { next }                 # the shebang is not part of the text
       /^#/     { sub(/^# ?/, ""); print; next }
                { exit }' "${BASH_SOURCE[0]}"
  exit 2
}

have() { command -v "$1" >/dev/null 2>&1; }

check_ignore_hit() { # check_ignore_hit <line of 'git check-ignore -v'>  -> "<path><TAB><rule>", or exit 1
  # WHAT A `git check-ignore -v` LINE SAYS, AND WHAT IT DOES NOT.
  #
  # `-v` prints ONE line per path whose last matching pattern git found:
  #
  #   <source>:<linenum>:<pattern><TAB><path>
  #
  # The pattern field is git's own verdict for that path, and a pattern that
  # begins with `!` is a RE-INCLUSION: git's rule (gitignore(5)) is that such
  # a path is NOT ignored, and that is what the path-specific question says —
  # `git check-ignore -q -- <path>` exits 1 for it. The line is printed all
  # the same, and the whole command exits 0, because a pattern matched.
  # Measured on this tree, 2026-09-24 (T1222):
  #
  #   .gitignore: "*.go" / "!keep.go", with keep.go and drop.go beside it
  #     git check-ignore -q -- keep.go     -> 1          (git does not ignore it)
  #     git check-ignore -q -- drop.go     -> 0          (git does ignore it)
  #     printf 'keep.go\ndrop.go\n' | git check-ignore --stdin -v   -> rc=0, two lines:
  #       .../.gitignore:2:!keep.go<TAB>keep.go
  #       .../.gitignore:1:*.go<TAB>drop.go
  #   .gitignore: "negpkg/" / "!negpkg/", with the package directory negpkg
  #     git check-ignore -q -- negpkg       -> 1
  #     printf 'negpkg\n' | git check-ignore --stdin -v             -> rc=0, one line:
  #       .../.gitignore:2:!negpkg/<TAB>negpkg
  #
  # Both callers below ask git exactly one question — "is this path ignored?"
  # — so a `!` line is the answer NO. Read as a hit instead, it refuses a file
  # git never ignored (a failed surface for a tree that is fine) and it drops
  # a package directory that is this repository's source, which is the
  # shrinking surface T1220 fixed, arriving through the check T1220 added.
  #
  # A `!` line cannot hide a path that IS ignored, which is the direction that
  # would cost coverage: when a re-inclusion is overruled — its parent
  # directory excluded (gitignore(5): "it is not possible to re-include a file
  # if a parent directory of that file is excluded") — git prints the
  # EXCLUDING pattern for that path, never the `!` one. Measured, same date:
  # `.gitignore: "ignored/" / "!ignored/keep.go"` gives
  # `check-ignore -q -- ignored/keep.go` -> 0 and `-v` prints the `ignored/`
  # line for it.
  #
  # A line with no TAB is not a verdict line at all (`2>&1` puts anything git
  # says on stderr into the same buffer), and is no more a hit than a `!` line.
  local line="$1" hdr pat
  case "$line" in *$'\t'*) ;; *) return 1;; esac
  hdr="${line%%$'\t'*}"             # <source>:<linenum>:<pattern>
  pat="${hdr#*:}"; pat="${pat#*:}"  # strip the source, then the line number
  case "$pat" in '!'*) return 1;; esac
  printf '%s\t%s\n' "${line##*$'\t'}" "$hdr"
}

report() { # report <tool> <report.json> <baseline> <id> <version> [rule-count]
  local tool="$1" file="$2" baseline="$3" id="$4" version="$5" rules="${6:-0}"
  [ -f "$file" ] || die "$id: the scanner wrote no report at $file — it did not measure anything"
  python3 tests/security/sast_report.py \
    --tool "$tool" --report "$file" --baseline "$baseline" --id "$id" --version "$version" --rules "$rules"
}

repo_paths_only() { # repo_paths_only <row id> <path>...  -> 0, or names the paths and returns 1
  # Every path a row feeds its scanner has to be a path of THIS repository:
  # under $ROOT, and not something git ignores.
  #
  # It is a check and not an assumption because the two are told apart by
  # something no scanner knows about. A working copy's root holds local state
  # beside the source — `.rddev/` carries the orchestrator's rebaseline drafts
  # and other Workers' worktrees, each one a copy of this tree's source, some
  # of it uncommitted — and the tools here cannot tell a copy from the original:
  # they read files. Git can, and that is the whole of this function.
  #
  # What makes it worth asserting rather than trusting is what the wrong answer
  # costs. A copy's finding is not this tree's finding: judged against
  # ops/ci/*-baseline.txt it is unbaselined and red, so the row reports another
  # tree's code as this one's — and the fix that suggests itself, adding the
  # path to the baseline, writes a file name that no commit contains into a
  # document whose every line is supposed to be a reviewed finding of this
  # repository. So an ignored path is refused HERE, as a failed surface, and
  # never reaches the scanner to come back as a finding to be judged.
  #
  # The oracle is asked both ways before it is believed, because an oracle
  # stuck on one answer turns this into a check that can only pass (it never
  # reports anything, and the surface escapes in silence) or only fail (every
  # path looks like an escape). The two questions have known answers: a path
  # under this repository's own runtime state is ignored on purpose, and a
  # tracked file of it is not.
  local id="$1"; shift
  local p bad=() inside=() out line hit rc
  for p in "$@"; do
    case "$p" in
      "$ROOT"|"$ROOT"/*) inside+=("$p");;
      *) bad+=("$p  — not a path of this repository: it is not under $ROOT");;
    esac
  done
  if [ "${#inside[@]}" -gt 0 ]; then
    if printf '%s\n' "$ROOT/.rddev/sast-surface-probe" | git -C "$ROOT" check-ignore --stdin -q \
       && ! printf '%s\n' "$ROOT/go.mod" | git -C "$ROOT" check-ignore --stdin -q; then
      :
    else
      die "$id: the surface check cannot be trusted on this tree: 'git check-ignore' did not answer \
'ignored' for this repository's own runtime state and 'not ignored' for a tracked file of it, so it \
cannot tell this repository's source from the local state that sits beside it. Until it can, this row \
cannot say what it scanned."
    fi
    out="$(printf '%s\n' "${inside[@]}" | git -C "$ROOT" check-ignore --stdin -v 2>&1)"; rc=$?
    case "$rc" in
      0) while IFS= read -r line; do
           [ -n "$line" ] || continue
           # read through check_ignore_hit: a `!` line is git saying this path
           # is NOT ignored, and is not a hit (see the note above it)
           hit="$(check_ignore_hit "$line")" || continue
           p="${hit%%$'\t'*}"
           # named the way the rest of the row names files: relative to $ROOT
           bad+=("${p#"$ROOT"/}  — git-ignored by ${hit#*$'\t'}, so it is local state, not source of this repository")
         done <<<"$out";;
      1) ;;
      *) printf '%s\n' "$out" | sed 's/^/     /' >&2
         die "$id: the surface check could not be run: 'git check-ignore' exited $rc. The default \
surface of the Go face is told apart from local state by git, so this row has to run from a checkout \
of this repository (not from an unpacked copy of it).";;
    esac
  fi
  [ "${#bad[@]}" -eq 0 ] && return 0
  {
    echo "$id: SCAN SURFACE FAILED — this row scans this repository's own source and nothing else,"
    echo "and these paths are not that:"
    printf '  %s\n' "${bad[@]}"
    echo ""
    echo "A path git ignores is local state, not a file of this repository: a scratch directory, a"
    echo "copy of the tree, or another checkout (.rddev/ holds all three on a working copy). Scanning"
    echo "one reports a tree nobody reviewed as if it were this one — and baselining such a finding"
    echo "would write a path into ops/ci/*-baseline.txt that no commit contains. If a path above"
    echo "really is source that belongs to this repository, git does not know about it yet: add it,"
    echo "then re-run this row."
  } >&2
  return 1
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
    if [ -n "${POST_SAST_GO_TARGET:-}" ]; then
      [ -d "$TARGET" ] || die "sast go: no such directory to scan: $TARGET"
      echo "     target: $TARGET (POST_SAST_GO_TARGET; the default is the module's own packages)"
      # The override scans exactly what it points at, which is what it is for:
      # a mutation check points this row at a planted fixture to watch the row
      # go red on it. So `/...` is kept here (the fixture may have directories
      # under it) and repo_paths_only is NOT applied — a fixture is planted on
      # purpose, and on a working copy a deliberately ignored directory is the
      # natural place to plant one. The default surface above is unaffected.
      echo "     surface: POST_SAST_GO_TARGET is set, so this run scans what it points at and nothing"
      echo "     else — including anything inside it that is not this repository's source. The check"
      echo "     that every scanned path is this repository's, unignored, is not applied to it."
      SURFACE=("${TARGET%/}/...")
    else
      TARGET="the module's own packages"   # what the floor message below names
      # DERIVED, not hand-maintained: the module's package set, as the Go
      # toolchain itself lists it. `go list ./...` is the difference between
      # this face and the defect above — it skips the directories Go does not
      # treat as source (a leading `.` or `_`, and testdata), which is exactly
      # what gosec's own walk from the root does not do. A package that moves
      # or appears is in the surface because the toolchain says so.
      #
      # The package set alone is not yet this repository's source. Installed
      # and generated state lives in directories whose names Go reserves
      # nothing for, and the toolchain cannot tell it from source: with the
      # pinned tools installed (`make security-tools`) the npm package
      # `flatted` ships a Go file under tests/security/node-tools/node_modules/,
      # which `go list ./...` returns as a package of this module (784 Go files
      # against 783 without it). Judging a dependency's file against this
      # repository's baseline is the same defect as scanning a rebaseline
      # draft, one directory over — so the surface is the module's packages
      # MINUS the directories git ignores, and git is the oracle the assertion
      # below also uses. Nothing is dropped in silence: every ignored package
      # directory is printed with the rule that ignores it.
      have git || die "git is not on PATH — this row's default surface is this repository's own \
source, and it is told apart from the local state that sits beside it (.rddev/: rebaseline drafts and \
other Workers' worktrees, each one a copy of this tree's source) with 'git check-ignore'. A copy's \
finding is not this tree's finding, so the row will not scan without that answer. Run it from a \
checkout of this repository."
      # -e so that a package the toolchain fails to list is still returned (and
      # then reported by gosec as one it could not type-check, as before)
      # instead of taking the whole surface down with it. There is no `set -e`
      # here, so a `die` inside the substitution below exits only the subshell:
      # every call is followed by `|| exit 1`.
      packages() { # packages <go list format> — the module's package set, or a die that says why not
        local out
        if ! out="$(go list -e -f "$1" ./... 2>"$WORK/golist.log")"; then
          sed 's/^/     /' "$WORK/golist.log" >&2
          die "sast go: 'go list ./...' failed, so this row cannot say which packages of the module it \
would scan. The surface is derived from the toolchain; with no answer from it there is no surface to \
scan, and scanning the root instead is the defect this row was fixed for."
        fi
        printf '%s' "$out"
      }
      DIRS_LIST="$(packages '{{.Dir}}')" || exit 1
      FILES_LIST="$(packages '{{range .GoFiles}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}')" || exit 1
      DIRS=()
      while IFS= read -r d; do [ -n "$d" ] && DIRS+=("$d"); done <<<"$DIRS_LIST"
      [ "${#DIRS[@]}" -gt 0 ] || die "sast go: 'go list ./...' named no package of this module — \
a scan of an empty surface reports no findings, which is indistinguishable from a clean one."

      # Which of those directories git ignores, and by which rule — one call
      # for all of them. `-v` is what makes the dropped list auditable: it
      # names the .gitignore line that did it, so a rule that is too broad is
      # visible in the row's own output instead of being a smaller surface.
      # Read through check_ignore_hit, for the reason written there: a line
      # whose pattern field starts with `!` is git re-including that package
      # directory, and counting it as a hit takes a package of this module out
      # of the surface while printing a rule that says the opposite.
      declare -A IGNORED_OF=()
      DROP_RC=0
      DROP_OUT="$(printf '%s\n' "${DIRS[@]}" | git -C "$ROOT" check-ignore --stdin -v 2>&1)" || DROP_RC=$?
      case "$DROP_RC" in
        0) while IFS= read -r line; do
             [ -n "$line" ] || continue
             hit="$(check_ignore_hit "$line")" || continue
             IGNORED_OF["${hit%%$'\t'*}"]="${hit#*$'\t'}"
           done <<<"$DROP_OUT";;
        1) ;;   # nothing ignored: every package of the module is a package of this repository
        *) printf '%s\n' "$DROP_OUT" | sed 's/^/     /' >&2
           die "sast go: 'git check-ignore' exited $DROP_RC, so this row cannot tell which packages of \
the module are this repository's source and which are installed or generated state git ignores. Run it \
from a checkout of this repository.";;
      esac

      SURFACE=(); DROPPED=(); FILES=()
      for d in "${DIRS[@]}"; do
        if [ -n "${IGNORED_OF[$d]:-}" ]; then
          DROPPED+=("${d#"$ROOT"/}  — git-ignored by ${IGNORED_OF[$d]}")
        else
          SURFACE+=("$d")
        fi
      done
      while IFS= read -r f; do
        [ -n "$f" ] || continue
        [ -n "${IGNORED_OF[${f%/*}]:-}" ] && continue
        FILES+=("$f")
      done <<<"$FILES_LIST"
      [ "${#SURFACE[@]}" -gt 0 ] || die "sast go: every package of this module is git-ignored, so this \
surface is empty — a scan of nothing reports no findings, which is indistinguishable from a clean one."
      # The assertion is on the FILES, not on the package directories: it is
      # the files gosec reads (its own default leaves test files alone, so the
      # set above leaves them alone too). A directory git ignores is dropped
      # above and printed; a file git ignores inside a directory that stays is
      # exactly what this refuses.
      repo_paths_only sast-go "${FILES[@]}" || exit 1
      echo "     target: this module's own packages — ${#SURFACE[@]} of them, ${#FILES[@]} Go file(s), \
from 'go list ./...' (POST_SAST_GO_TARGET overrides)"
      if [ "${#DROPPED[@]}" -gt 0 ]; then
        if [ "${#DROPPED[@]}" -eq 1 ]; then DROPPED_NOUN="package directory"; else DROPPED_NOUN="package directories"; fi
        echo "     surface: NOT scanned — ${#DROPPED[@]} $DROPPED_NOUN of the module that git ignores:"
        echo "     installed or generated state rather than source of this repository, and a finding in a"
        echo "     dependency's file is not a finding of this tree:"
        printf '       %s\n' "${DROPPED[@]}"
      fi
    fi
    # The `/...` suffix, where it is used above, is not decoration: `gosec .`
    # loads the one package in the current directory (at this root: a package
    # with no Go files) and writes no report at all, which the row then reports
    # as "the scanner measured nothing". The default surface needs neither: it
    # is a list of package directories, and gosec scans each one and does not
    # walk below it — so the files it reads are the files checked above (for
    # the two packages of this module that hold only test files there is
    # nothing either of them reads, which is why they are not in that list).
    # No `/...` on the default surface is also what makes that check complete.
    gosec -fmt=json -out="$WORK/gosec.json" -quiet "${SURFACE[@]}" >"$WORK/gosec.log" 2>&1
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
