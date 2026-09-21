#!/usr/bin/env bash
# mutation-check.sh — proves the demo's verifier can say NO.
#
# The acceptance criterion this script exists for: before the seed is trusted,
# the INSTRUMENT is tested. `seeddemo verify` passing every check proves
# nothing on its own — a verifier that cannot fail is decoration. So this
# drops one object from a COPY of the plan, builds that plan into its own
# database, and requires the verifier to
#
#   (a) exit non-zero, and
#   (b) name the class it was short of, with the shortfall it measured.
#
#   tests/acceptance/seeddemo/mutation-check.sh           # drop calc-gcmc-70
#   tests/acceptance/seeddemo/mutation-check.sh --keep    # leave the mutant database
#   SEED_DEMO_MUTATION_KEY=exp-70-02 .../mutation-check.sh   # mutate another object
#   SEED_DEMO_MUTATION_SUMMARY=/tmp/m.json .../mutation-check.sh  # keep its summary
#   SEED_DEMO_MUTATION_LOG=/tmp/m.log .../mutation-check.sh       # keep its build log
#   SEED_DEMO_API_LOG=/tmp/api.log .../mutation-check.sh          # keep the API's own log
#
# The default key is one nothing else in the plan names. That matters: an
# object its own plan still refers to (a relation's source, an assertion's
# evidence end, an asset manifest's member) cannot be dropped on its own — the
# builder would stop on the dangling reference and the verifier would never
# run, which proves nothing about the verifier. A key that IS referenced is
# refused here, by name, with the referring paths, unless --force is passed for
# someone deliberately studying that case.
#
# The mutant build runs with --external 0 by default, and that is deliberate.
# This check is an experiment about ONE thing: whether the verifier notices an
# object class coming up short. The external contribution is a different
# subsystem — a fork, a Gitea repository and an import — and on the dev machine
# it can lose a race described in ops/seed-demo.md §Known limitations (a stale
# queued job holds the single provisioning worker while it backs off, so the
# parent project's repository may not exist yet when the fork arrives). A build
# that stops there produces a red the verifier did not author, which this check
# refuses anyway; excluding the fork removes the noise rather than hiding it.
# Set SEED_DEMO_MUTATION_EXTERNAL=1 to run the mutant with the fork path too.
#
# What it must NOT do, and does not: touch examples/seed-demo/demo-plan.json
# (the mutant is a temp copy), or overwrite `.seed-demo-summary.json` (the
# mutant run writes its summary to a temp path through SEED_DEMO_SUMMARY).
#
# Exit code: 0 when the build completed, the verifier failed, and the failure
# names the mutated object's class — the three things that make the red a
# demonstration rather than an accident. Any other outcome is a failure of
# this check, including "the build crashed": a builder that dies before the
# verifier runs proves nothing about the verifier.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
PLAN="$ROOT/examples/seed-demo/demo-plan.json"
MUTANT_KEY="${SEED_DEMO_MUTATION_KEY:-calc-gcmc-70}"
MUTANT_DB="${SEED_DEMO_MUTATION_DB:-post_seed_demo_mutation}"
MUTANT_EXTERNAL="${SEED_DEMO_MUTATION_EXTERNAL:-0}"
KEEP=0
FORCE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --keep) KEEP=1 ;;
    --force) FORCE=1 ;;
    --key)  MUTANT_KEY="$2"; shift ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "mutation-check: unknown argument $1" >&2; exit 2 ;;
  esac
  shift
done

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
MUTANT_PLAN="$TMP/demo-plan-mutant.json"
# The mutant run's summary is kept for the caller to quote (the mutant plan and
# its log are not: one is a copy, the other is noise). Default is a temp path,
# so a plain `mutation-check.sh` leaves nothing behind.
MUTANT_SUMMARY="${SEED_DEMO_MUTATION_SUMMARY:-$TMP/summary.json}"

# --- 1. the mutation -------------------------------------------------------
# The plan must not refer to the object anywhere else, and finding that out
# means walking every string in the document: a relation names its endpoints
# plainly ("source": "calc-dft-water-cu"), an asset manifest names its members
# with an @ ("@mat-mof-y"), a blob fixture with a prefix ("@blob:ds-iso-40").
# Matching the bare key as a substring of every string value catches all three;
# matching the quoted key alone (the first version of this script) missed the
# @-prefixed one and let a material through that the asset manifest needed, so
# the build stopped and the red it produced was the builder's, not the
# verifier's. Each referring path is printed, so --force is an informed choice
# rather than a shrug.
MUTANT_TYPE="$(python3 - "$PLAN" "$MUTANT_PLAN" "$MUTANT_KEY" "$FORCE" <<'PY'
import json, sys, collections
src, dst, key, force = sys.argv[1:5]
force = force == "1"
with open(src) as fh:
    plan = json.loads(fh.read(), object_pairs_hook=collections.OrderedDict)

containers = [plan["main_objects"], plan["main_conclusions"]]
containers += [c.get("objects", []) for c in plan["branches_content"].values()]
containers.append(plan["external_contribution"]["objects"])

where, found = None, None
for c in containers:
    for o in c:
        if o.get("key") == key:
            where, found = c, o
if found is None:
    sys.exit("mutation-check: the plan declares no object %r" % key)

def refs(node, path=""):
    """Every string in the plan that names this key, ignoring the declaration."""
    if node is found:
        return []
    out = []
    if isinstance(node, dict):
        for k, v in node.items():
            out += refs(v, "%s.%s" % (path, k))
    elif isinstance(node, list):
        for i, v in enumerate(node):
            out += refs(v, "%s[%d]" % (path, i))
    elif isinstance(node, str) and key in node:
        out.append((path, node))
    return out

hits = refs(plan)
if hits and not force:
    sys.exit("mutation-check: the plan names %r in %d more place(s) than the declaration, so "
             "dropping it leaves a dangling reference and the BUILDER would stop before the verifier "
             "runs — which would prove nothing about the verifier:\n  %s\n"
             "Pick a key the plan does not refer to, or pass --force to study that case."
             % (key, len(hits), "\n  ".join("%s = %r" % h for h in hits)))
where.remove(found)
with open(dst, "w") as fh:
    fh.write(json.dumps(plan, indent=2, ensure_ascii=False) + "\n")
print(found["type"])
PY
)"
RC=$?
if [ "$RC" != "0" ]; then exit 1; fi
if [ -z "$MUTANT_TYPE" ]; then echo "mutation-check: could not read the mutated object's type" >&2; exit 1; fi
echo "mutation-check: dropped $MUTANT_TYPE $MUTANT_KEY from a copy of the plan ($MUTANT_PLAN)" >&2

# --- 2. build and verify the mutant plan ----------------------------------
# The mutant run's own output. Kept on request (SEED_DEMO_MUTATION_LOG) and
# always shown, trimmed, when the check fails — a red whose cause is invisible
# is not evidence of anything.
BUILD_LOG="${SEED_DEMO_MUTATION_LOG:-$TMP/build.log}"
SEED_DEMO_SUMMARY="$MUTANT_SUMMARY" \
  "$ROOT/ops/seed-demo.sh" --reset --db "$MUTANT_DB" --plan "$MUTANT_PLAN" \
  --external "$MUTANT_EXTERNAL" >"$BUILD_LOG" 2>&1
RC=$?

# --- 3. the three things that make the red a demonstration -----------------
python3 - "$MUTANT_SUMMARY" "$MUTANT_TYPE" "$RC" "$MUTANT_KEY" <<'PY'
import json, sys
summary_path, want_type, rc, key = sys.argv[1:5]
rc = int(rc)
try:
    with open(summary_path) as fh:
        s = json.load(fh)
except Exception as exc:
    print("mutation-check: the mutant run wrote no summary (%s) — the red we got is not the verifier's: %s" % (summary_path, exc))
    sys.exit(1)

problems = []
if s.get("build_exit_code") != 0:
    problems.append("the BUILD did not complete (exit %s), so the verifier proved nothing about the "
                    "missing %s: %s" % (s.get("build_exit_code"), want_type, "; ".join(s.get("notes", []))))
if s.get("verify_exit_code") == 0:
    problems.append("the verifier PASSED a plan that is missing one %s — the instrument cannot say no" % want_type)
short = [c for c in s.get("checks", [])
         if c["status"] == "fail" and c["item"].startswith(want_type + " (") and c["actual"] < c["required"]]
if not short:
    problems.append("the verifier failed but named no shortfall in the %s class" % want_type)

print("MUTATION CHECK — dropped %s %s" % (want_type, key))
print("  ops/seed-demo.sh --reset --db <mutant db> --plan <plan minus that object> --external %s"
      % (1 if s.get("external_contribution") else 0))
print("  build exit code:  %s" % s.get("build_exit_code"))
print("  verify exit code: %s" % s.get("verify_exit_code"))
if s.get("failed"):
    print("  the verifier named it: %s" % "; ".join(s["failed"]))
for c in short:
    print("  %s: required %s, measured %s — %s" % (c["item"], c["required"], c["actual"], c.get("detail", "")))
for p in problems:
    print("  PROBLEM: %s" % p)
sys.exit(1 if problems else 0)
PY
OUTCOME=$?

if [ "$KEEP" = "0" ]; then
  PGPASSWORD="${POST_DB_PASSWORD:-postgres_dev_pw}" psql -h "${POST_DB_HOST:-127.0.0.1}" \
    -U "${POST_DB_USER:-postgres}" -d postgres \
    -c "drop database if exists $MUTANT_DB" >/dev/null 2>&1 \
    && echo "mutation-check: dropped the mutant database $MUTANT_DB (--keep would have left it)" >&2
fi

if [ "$OUTCOME" != "0" ]; then
  echo "mutation-check: FAILED — the verifier did not go red the way this check requires" >&2
  echo "--- the mutant run's own output ---" >&2
  tail -25 "$BUILD_LOG" >&2
  exit 1
fi
echo "mutation-check: OK — the verifier refused a demo that is one $MUTANT_TYPE short" >&2
exit 0
