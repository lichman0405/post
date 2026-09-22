#!/usr/bin/env bash
#
# CONTAINER SCAN — the container-build face of the Master Security/Quality
# Gate (docs/25_CICD_DEVOPS.md §23 item 10 "container build/SBOM").
#
#   bash tests/security/container-scan.sh
#
# THE PROBLEM THIS ROW SOLVES
# ---------------------------
# Every other row of the gate scans something. This one has no object: there
# is no Dockerfile in this repository, so there is no image to scan, and a row
# that simply said "container scan: not applicable" would be a permanent green
# light that nobody would ever revisit — the exact failure docs/23 §11 is
# written against ("每个 Release 运行 … container scan").
#
# So the row is a GUARDED ABSENCE, and an absence that can fail:
#
#   * it is GREEN while the tree really has no container build file, and it
#     prints WHY — the tree claim it rests on, where that claim is registered
#     and where it is re-derived — plus how much of the tree it looked at, so
#     "clean" is a statement about a scan that happened;
#   * it also prints WHAT IT DID NOT LOOK AT. A walk with exclusions is only
#     honest if the exclusions are visible to the person reading the result:
#     "the tree has no container build file" is a claim about the directories
#     `PRUNED` below leaves out, so each of them is printed with its reason on
#     every green run, and a container build file sitting inside one of them is
#     printed too (separately walked for exactly that). It is not counted as
#     this tree's file — installed packages, other checkouts and generated
#     output are not this tree's content — and it is not silent either. The
#     same list, with the same reasons, is in tests/security/README.md;
#   * it turns RED the moment a Dockerfile or Containerfile appears anywhere
#     in the tree. That is not a failure of the repository; it is this row
#     reporting that it no longer applies and that a real image scan has to
#     take its place. A container scan with an object is either run or it is
#     not a scan.
#   * it also turns RED if the tree claim it cites disappears from
#     ops/runbook-steps.json. A guard that cites a claim nobody registers
#     anymore is a guard resting on nothing.
#
# WHAT A REAL REPLACEMENT WOULD BE
# --------------------------------
# When an image exists: build it, then scan the IMAGE (trivy/grype against the
# built digest, not the Dockerfile), and keep the SBOM rows pointed at the
# image's package set. That is a new task with a new tool pin; this row's
# failure message says so rather than leaving the next reader to work it out.
#
# Exit codes
#   0  no container build file in the tree, and the claim it rests on is registered
#   1  a container build file exists (the row no longer applies), or the claim is gone
#   2  usage error
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

# Mutation hook: point the same detector at a directory built for the purpose.
# A gate run never sets it, and the row prints the root it used either way.
SCAN_ROOT="${POST_CONTAINER_SCAN_ROOT:-$ROOT}"

usage() { sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' | sed '/^$/q'; exit 2; }
die() { echo "container-scan: $*" >&2; exit 1; }

[ -d "$SCAN_ROOT" ] || die "no such directory to scan: $SCAN_ROOT"

CLAIMS="ops/runbook-steps.json"
grep -q '"id": "no-dockerfile"' "$CLAIMS" \
  || die "the tree claim 'no-dockerfile' is no longer registered in $CLAIMS. This row is a guarded \
absence: it is green only because the tree has no container build file, and the claim it cites is the \
one that says so. With the claim gone, the row has nothing to rest on — restore the claim or replace \
this row with a real image scan."

# The same walk ops/runbook-verify.py performs for the claim (empty-find over
# Dockerfile*), widened to podman's Containerfile and to any Dockerfile.<stage>
# name, and excluding the same runtime/build state that script excludes — plus
# the directories this repository only ever fills with installed packages or
# generated artifacts. `-name ... -prune` rather than a glob because `find` has
# to look INSIDE a directory to decide, and looking inside node_modules is how
# a scan reports a dependency's own Dockerfile as ours.
#
# ONE LIST, and it is printed (see the report below) — a name without a reason
# beside it is how a walk with exclusions becomes a claim nobody can check.
PRUNED=(
  ".git|the repository's own object store: its entries are compressed objects and refs, not files of this tree, and its file names say nothing about the checkout"
  ".rddev|the orchestrator's runtime state — and other Workers' worktrees, each of them a whole checkout of this repository. A Dockerfile in another Worker's tree is not this tree's file, and walking it would let one branch decide another task's gate"
  "node_modules|installed npm packages: a Dockerfile inside a dependency's published tarball is the dependency's, not ours. ops/runbook-verify.py's walk leaves installed trees out for the same reason"
  ".next|the web app's generated build output"
  ".venv|the scientific adapter's installed Python packages (the same reason as node_modules)"
  ".sbom|the CycloneDX documents this gate writes and reads back (generated, gitignored: tests/security/sbom.sh)"
  ".backup-dr|the backup/DR drill's artifacts — database dumps, blob copies and git mirrors — tens of megabytes that are a copy of an environment rather than a source change (docs/37_BACKUP_DR.md)"
  ".dev|dev-stack runtime scratch: POST_MAIL_SINK_DIR defaults to .dev/mail and the sink writes whole notification bodies there, so it holds runtime state, not source"
  "post-wt|the Supervisor's own worktrees (rddev branch create --worktree post-wt/...): separate checkouts of this repository, same reason as .rddev"
)

# What makes a file a container build file here. One array, used by both walks.
CONTAINER_FILE_NAMES=(
  -name 'Dockerfile' -o -name 'Dockerfile.*' -o -name '*.Dockerfile'
  -o -name 'Containerfile' -o -name 'Containerfile.*'
)

# walk <root> <find tests...> — the walk above, built from PRUNED so the
# exclusion list cannot drift from the one this row prints.
walk() {
  local root="$1"; shift
  local args=() entry
  for entry in "${PRUNED[@]}"; do args+=( -name "${entry%%|*}" -o ); done
  unset 'args[${#args[@]}-1]'
  find "$root" \( "${args[@]}" \) -prune -o "$@"
}

found="$(walk "$SCAN_ROOT" -type f \( "${CONTAINER_FILE_NAMES[@]}" \) -print 2>/dev/null \
  | sed "s|^$SCAN_ROOT/||" | sort)"
scanned="$(walk "$SCAN_ROOT" -type f -print 2>/dev/null | wc -l | tr -d ' ')"

# And what the exclusions would have hidden. Each pruned directory is walked on
# its own for container build files: not to judge them (they are not this
# tree's content — that is what the reason beside each name says), but so that
# somebody who puts a Dockerfile in post-wt/ finds out from this row's output
# rather than from a green line that quietly walked around it.
pruned_found=""
for entry in "${PRUNED[@]}"; do
  dir="${entry%%|*}"
  [ -d "$SCAN_ROOT/$dir" ] || continue
  hits="$(find "$SCAN_ROOT/$dir" -type f \( "${CONTAINER_FILE_NAMES[@]}" \) -print 2>/dev/null \
    | sed "s|^$SCAN_ROOT/||" | sort)"
  [ -n "$hits" ] && pruned_found="${pruned_found}${hits}"$'\n'
done
pruned_found="$(printf '%s' "$pruned_found" | sed '/^$/d')"

if [ -n "$found" ]; then
  {
    echo "container-scan: the tree now has a container build file:"
    printf '  %s\n' $found
    echo ""
    echo "This row is a guarded absence, not a scan: it was green because there was no image to scan."
    echo "That is no longer true, so the row has to be replaced rather than kept green —"
    echo "docs/25 §23 item 10 asks for a container build AND a container scan, and docs/23 §11 asks"
    echo "for the scan at every release. Build the image and scan it (trivy or grype against the built"
    echo "digest, with its own pinned version in ops/security/tool-versions.sh), then update"
    echo "ops/security/absent-checks.json, which still records container-scan as absent."
    echo "The tree claim 'no-dockerfile' ($CLAIMS, ops/deploy/README.md) and the runbook steps that"
    echo "rest on it (blocked_by: no-dockerfile) become stale in the same moment."
  } >&2
  exit 1
fi

echo "ok   container-scan: no container build file in the tree ($scanned file(s) walked, tree claim 'no-dockerfile' holds)"
echo "     the claim this rests on: $CLAIMS (id no-dockerfile, re-derived by ops/runbook-verify.py T1);"
echo "     ops/deploy/README.md 'There is no \`Dockerfile\` anywhere in this repository'; ops/DEV_COMMANDS.md:154."
echo "     it is not a container scan and does not pretend to be one: with no image there is nothing to"
echo "     scan, and this row exists so that stays visible — and so that it stops being true loudly."
echo "     the directories this row did NOT walk ($scanned file(s) above are the rest of the tree):"
for entry in "${PRUNED[@]}"; do
  printf '       %-14s %s\n' "${entry%%|*}" "${entry#*|}"
done
if [ -n "$pruned_found" ]; then
  {
    echo ""
    echo "     CONTAINER BUILD FILE(S) INSIDE THE DIRECTORIES LISTED ABOVE — the walk did not count these"
    echo "     as this tree's, and this row did not judge them. They are in one of the directories printed"
    echo "     above, for the reason printed beside it (an installed tree, another checkout, generated output):"
    printf '       %s\n' $pruned_found
    echo "     if one of them is meant to be THIS tree's container build file, it belongs outside those"
    echo "     directories — and the moment it is there, the walk above finds it and this row goes red."
  } >&2
else
  echo "     nothing inside them either: each was walked separately for container build files and came back empty"
fi
