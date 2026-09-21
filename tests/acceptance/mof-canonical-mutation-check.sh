#!/usr/bin/env bash
#
# Is mof-canonical-workflow.sh's chain load-bearing, or does it pass whatever
# happens?
#
# A gate that cannot fail certifies nothing, and the only way to know which one
# this is is to break the thing it grades and watch. This script breaks TWO
# hops of the browser driver, one at a time, and requires the gate to go red
# and to NAME each one:
#
#   A. the owner's merge request is removed. The chain's last transition never
#      happens, so the proposal stays merge_ready and main never gains a state.
#   B. the scientist's `git push` is removed. Everything the product does still
#      happens — the branch, the object, the proposal, both reviews, the merge
#      call — and the merge still commits its state transition, but the
#      provider has no commit to merge, so the Git half of the merge fails.
#      B is the interesting one: it is what an earlier version of this gate
#      actually did, and it is why the push is a step of the flow rather than
#      an assumption behind it.
#
# Each mutation is in the SUBJECT (the driver's own call), never in an
# assertion: weakening an assertion would prove only that a weakened assertion
# fails. Everything the mutation does not touch stays in, so what the gate
# reports is about the missing hop and nothing else.
#
# This is the same shape as tests/acceptance/seeddemo/mutation-check.sh, which
# proves the seed's verifier can fail by building a deliberately short plan.
#
# Usage: bash tests/acceptance/mof-canonical-mutation-check.sh
# Exit:  0 when the gate failed for the right reason in both cases.
#        1 when a mutation was not caught, or was caught for the wrong reason.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

GATE="$ROOT/tests/acceptance/mof-canonical-workflow.sh"
DRIVER="$ROOT/tests/e2e-pr-flows/mof-canonical-e2e.mjs"
# The run's own directory is NOT deleted at the end: when a mutation is not
# caught, what the gate actually said about it is the whole evidence, and this
# script prints those paths so they can be read in full.
WORK="$(mktemp -d)"

# The mutants live in this run's temp dir, NOT in the tree they grade: a file
# written beside the driver would be a file the gate's own tree guard then
# reports as a change the gate made. Node's ESM resolver walks up from the
# importing file, so the one thing a mutant needs — the playwright package the
# driver imports — is reached through a link to the real project's
# node_modules. A mutant that dies of ERR_MODULE_NOT_FOUND would go red
# without ever reaching an assertion, which is not a caught mutation.
ln -s "$ROOT/tests/e2e-pr-flows/node_modules" "$WORK/node_modules"

VERDICT=0
note()  { printf '%s\n' "$*"; }
bad()   { printf 'MUTATION CHECK FAILED — %s\n' "$*" >&2; VERDICT=1; }

# mutate NAME OUT PYTHON — applies one mutation, or exits 2 if the driver no
# longer has the shape that mutation knows how to break (a loud failure is the
# right one: silently proving nothing is not).
mutate() {
  local name="$1" out="$2" code="$3"
  python3 -c "$code" "$DRIVER" "$out"
  local rc=$?
  if [ "$rc" != 0 ]; then
    echo "MUTATION CHECK — could not build mutant $name (exit $rc); the gate's driver no longer has the shape this mutation breaks" >&2
    exit 2
  fi
}

# run_gate NAME MUTANT — runs the real gate against one mutant. Sets GATE_RC
# and leaves the gate's own output in $WORK/<name>.log.
run_gate() {
  local name="$1" mutant="$2"
  note "MUTATION CHECK — running the gate against the $name mutant"
  note "                (original: $DRIVER)"
  note "                (mutant:   $mutant)"
  note ""
  GATE_RC=0
  MOF_CANONICAL_DRIVER="$mutant" bash "$GATE" > "$WORK/$name.log" 2>&1 || GATE_RC=$?
  note "gate exit code with the mutation applied: $GATE_RC"
  note ""
  note "--- the gate's own steps that went red ---"
  grep -n '^FAIL' "$WORK/$name.log" | head -20 || true
  note ""
  [ "$GATE_RC" != 0 ] || bad "$name: the gate PASSED with the hop removed. Its assertions are not load-bearing."
}

# want NAME LOG PATTERN — one line the gate's own output has to carry.
want() {
  local name="$1" log="$2" pattern="$3"
  grep -q "$pattern" "$log" || bad "$name: the gate's red does not carry: $pattern"
}

# ---------------------------------------------------------------------------
# A. the merge hop is removed
# ---------------------------------------------------------------------------

MUTANT_A="$WORK/mof-canonical-e2e.merge-removed.mjs"
mutate "A" "$MUTANT_A" '
import re, sys
src, out = sys.argv[1], sys.argv[2]
text = open(src).read()
# The owner'"'"'s merge call — the request, its key and its body — becomes a
# value the step'"'"'s own assertion has to reject. The replacement is a single
# syntactically valid statement, so the mutant still PARSES: a driver that dies
# of a syntax error would prove nothing about the assertion.
pattern = re.compile(
    r"^[ \t]*const merged = await call\(owner\.page,[^\n]*\n"
    r"(?:[^\n]*\n)*?[^\n]*mof-canonical-merge-\$\{CTX\.tag\}[^\n]*\n",
    re.M)
mutated, n = pattern.subn(
    "    const merged = { status: 0, body: null, error: \"MUTATION: the merge hop was removed\" };\n",
    text)
if n != 1:
    print(f"mutation did not apply: {n} merge call(s) matched, wanted exactly 1")
    raise SystemExit(2)
open(out, "w").write(mutated)
'

# The mutant has to be a mutant. (`${number}:merge` alone is not the test: the
# driver's NEGATIVE control merges the same proposal as the external user, and
# that request must stay in. The owner's own merge call and its idempotency key
# are what belong to the hop this mutation removes and to nothing else.)
if grep -q 'const merged = await call(owner.page' "$MUTANT_A"; then
  echo "MUTATION CHECK — mutant A still contains the original merge call" >&2; exit 2
fi
if grep -q 'mof-canonical-merge-' "$MUTANT_A"; then
  echo "MUTATION CHECK — mutant A still carries the owner's merge idempotency key; this run would prove nothing" >&2; exit 2
fi
grep -q 'MUTATION: the merge hop was removed' "$MUTANT_A" \
  || { echo "MUTATION CHECK — the mutation was not written into mutant A" >&2; exit 2; }

run_gate "A" "$MUTANT_A"
want "A" "$WORK/A.log" 'FAIL the merge committed the state AND advanced the provider'
want "A" "$WORK/A.log" 'FAIL the browser driver exited'
# The red has to NAME the hop, not merely be red: the driver's own step line is
# the evidence that it was the merge that broke.
want "A" "$WORK/A.log" '\[[0-9]*\] the owner merges the proposal \[fetch\]'
# And the hops the mutation did NOT touch must still be green: a gate that goes
# red everywhere proves nothing about any particular hop.
for hop in "the project owner signs in through the real login page" \
           "the scientist pushes the branch's work to the provider" \
           "the owner opens the proposal" \
           "review 1 (scientific, approved) is recorded through the app's own form" \
           "NEGATIVE: a non-member's merge of the same proposal is refused"; do
  want "A" "$WORK/A.log" "\[.*\] ${hop}"
done

# ---------------------------------------------------------------------------
# B. the scientist's push is removed
# ---------------------------------------------------------------------------

MUTANT_B="$WORK/mof-canonical-e2e.push-removed.mjs"
mutate "B" "$MUTANT_B" '
import re, sys
src, out = sys.argv[1], sys.argv[2]
text = open(src).read()
# The push — the clone, the commit, the push and the read-back of the provider
# ref — becomes a value that says nothing was pushed.
pattern = re.compile(
    r"^[ \t]*pushed = gitPush\(\n(?:.*\n)*?^[ \t]*\}\);\n",
    re.M)
mutated, n = pattern.subn(
    "      pushed = { sha: \"\", providerSHA: \"\" }; // MUTATION: the push was removed\n",
    text)
if n != 1:
    print(f"mutation did not apply: {n} push block(s) matched, wanted exactly 1")
    raise SystemExit(2)
open(out, "w").write(mutated)
'
if grep -q 'pushed = gitPush(' "$MUTANT_B"; then
  echo "MUTATION CHECK — mutant B still performs the push; this run would prove nothing" >&2; exit 2
fi
grep -q 'MUTATION: the push was removed' "$MUTANT_B" \
  || { echo "MUTATION CHECK — the mutation was not written into mutant B" >&2; exit 2; }

run_gate "B" "$MUTANT_B"
want "B" "$WORK/B.log" 'FAIL the provider.s ref for the branch carries the commit that was just made'
# The point of B: with no pushed commit the merge STILL commits its state
# transition (the product's database half is not what failed) and the Git half
# is what goes red. A gate that reported the merge as fine here would be
# certifying half a merge.
want "B" "$WORK/B.log" 'FAIL the merge committed the state AND advanced the provider'
want "B" "$WORK/B.log" '\[[0-9]*\] the scientist pushes the branch.s work to the provider \[git\]'
want "B" "$WORK/B.log" 'FAIL the browser driver exited'
# And main really did not move in Git: the gate reads the provider's own ref.
want "B" "$WORK/B.log" 'FAIL the sha Gitea.s refs/heads/main holds'

if [ "$VERDICT" != 0 ]; then
  # A mutation that was not caught is only reportable with the gate's own words
  # behind it — including the case where the mutant died before it reached an
  # assertion (a crash is not a caught mutation, and it looks nothing like one
  # in the grep above).
  for name in A B; do
    [ -f "$WORK/$name.log" ] || continue
    note ""
    note "--- what the gate said in the $name run (driver lines, errors, and the log itself) ---"
    grep -n -E 'mof-canonical-e2e|^FAIL|Error|ERR_|throw|at .*\.mjs' "$WORK/$name.log" | head -30 || true
    note "    the full log: $WORK/$name.log"
  done
fi

note ""
note "the mutants and the gate logs this run produced (kept): $WORK"
if [ "$VERDICT" = 0 ]; then
  note "MUTATION CHECK — both mutations were caught. The gate went red for each missing"
  note "hop, named the step that broke, and its own assertions are the lines that failed;"
  note "every hop the mutation did not touch was still driven, so each red is about the"
  note "hop that was removed. The gate can say no."
  exit 0
fi
echo "MUTATION CHECK — the gate did NOT catch a removed hop; see the reasons above." >&2
exit 1
