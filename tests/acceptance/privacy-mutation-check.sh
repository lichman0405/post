#!/usr/bin/env bash
# Mutation check for the two blocks T1107 adds (the required test "privacy
# suite"): prove each can FAIL, so that "it passes" means "it measured
# something".
#
# The task's rule for any non-test change is "改不出红的证据，就不要改那段
# 代码". Mutation 1 is that evidence for the one product change this task
# makes; mutation 2 is the same evidence for the count-leak block, which
# would otherwise be a test that only ever agreed with the code.
#
#   1. explore-order — the WHERE clause T1107 added to knowledgeQuery is
#      removed, so the LIMIT is taken over the whole corpus again and a
#      private project's publications spend the window. The Explore test
#      must refuse to pass, and the refusal must be the section-bytes
#      assertion (not an unrelated failure).
#
#   2. anonymous-member — assetshttp's viewer() reports an ANONYMOUS caller
#      as a member of the asset's owning project, so the page renders the
#      private versions the fixture seeded. The count-leak differential
#      must refuse to pass, and the refusal must be the "three private
#      versions changed the answer" assertion.
#
# Both mutations are applied to the PRODUCT code — never to the tests —
# because a mutation inside a test only proves the test can fail at failing
# things. Each file is restored from a byte-exact backup on every exit path
# (trap), and the script verifies the restore with cmp before it draws any
# conclusion: a mutation check that leaves the product tree edited has
# measured nothing and broken something.
#
# Usage: bash tests/acceptance/privacy-mutation-check.sh
set -euo pipefail

ACCEPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$ACCEPT_DIR/../.." && pwd)"
STORE="$ROOT/cmd/api/explorehttp/store.go"
PAGE="$ROOT/cmd/api/assetshttp/page.go"

PG_TEST_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"
export POSTGRES_TEST_ADMIN_URL="$PG_TEST_URL"

for f in "$STORE" "$PAGE"; do
  if [ ! -f "$f" ]; then
    echo "mutation-check: $f not found" >&2
    exit 1
  fi
done

WORK="$(mktemp -d)"
STORE_BACKUP="$WORK/store.go.orig"
PAGE_BACKUP="$WORK/page.go.orig"
cp "$STORE" "$STORE_BACKUP"
cp "$PAGE" "$PAGE_BACKUP"

restore() {
  cp "$STORE_BACKUP" "$STORE"
  cp "$PAGE_BACKUP" "$PAGE"
}
cleanup() {
  restore
  rm -rf "$WORK"
}
trap cleanup EXIT

# restore_and_verify puts both files back and refuses to continue if either
# restore is not byte-identical.
restore_and_verify() {
  restore
  if ! cmp -s "$STORE_BACKUP" "$STORE" || ! cmp -s "$PAGE_BACKUP" "$PAGE"; then
    echo "mutation-check: FATAL — could not restore the product tree byte-for-byte" >&2
    exit 1
  fi
}

# mutate replaces exactly one occurrence of $2 with $3 in the file $1.
mutate() {
  python3 - "$1" "$2" "$3" <<'PY'
import sys

path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
with open(path, encoding="utf-8") as fh:
    src = fh.read()
if src.count(old) != 1:
    sys.exit(f"mutation-check: the mutation site matched {src.count(old)} times, want exactly 1:\n{old}")
with open(path, "w", encoding="utf-8") as fh:
    fh.write(src.replace(old, new))
PY
}

EXPLORE_TEST='TestPrivatePublicationsDoNotCrowdTheExploreIndex'
COUNT_TEST='TestPrivateUsageAndVersionsAreUnobservableOnEveryPublicSurface'

# run_case runs one test under the current tree and returns the output; the
# caller decides what the output must contain. The suite is loud about a
# database, so a missing one is a failure here rather than a quiet skip.
run_case() {
  (cd "$ROOT" && go test ./tests/integration -run "$1" -count=1 -timeout 10m 2>&1) || true
}

EXPLORE_SITE="WHERE p.visibility = 'public'
  AND sov.visibility_policy_id IS NULL
ORDER BY kp.published_at DESC, kp.id"
EXPLORE_MUTATION="ORDER BY kp.published_at DESC, kp.id"

VIEWER_SITE='	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return assets.PageViewer{}, nil
	}'
VIEWER_MUTATION='	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return assets.PageViewer{Member: true}, nil
	}'

echo "== mutation-check: the UNMUTATED tree must pass =="
baseline="$(run_case "$EXPLORE_TEST|$COUNT_TEST")"
if ! grep -q "^ok" <<<"$baseline"; then
  echo "$baseline" >&2
  echo "mutation-check: the tests do not pass on the unmutated tree; fix that first" >&2
  exit 1
fi
echo "baseline: ok"

echo "== mutation 1/2: the audience predicates leave knowledgeQuery (a private publication spends the window) =="
mutate "$STORE" "$EXPLORE_SITE" "$EXPLORE_MUTATION"
mutation1="$(run_case "$EXPLORE_TEST")"
restore_and_verify
if grep -q "^ok" <<<"$mutation1"; then
  echo "$mutation1" >&2
  echo 'mutation-check: BUG — the Explore test passes while the query reads the whole corpus' >&2
  exit 1
fi
if ! grep -qE 'CHANGED the knowledge section' <<<"$mutation1"; then
  echo "$mutation1" >&2
  echo 'mutation-check: BUG — the test failed, but not on the section-bytes assertion' >&2
  exit 1
fi
echo "mutation 1 caught: the post-LIMIT window is refused on the section-bytes assertion"

echo "== mutation 2/2: an anonymous caller is reported as a project member =="
mutate "$PAGE" "$VIEWER_SITE" "$VIEWER_MUTATION"
mutation2="$(run_case "$COUNT_TEST")"
restore_and_verify
if grep -q "^ok" <<<"$mutation2"; then
  echo "$mutation2" >&2
  echo 'mutation-check: BUG — the count differential passes while a stranger is treated as a member' >&2
  exit 1
fi
if ! grep -qE 'three private versions changed the answer' <<<"$mutation2"; then
  echo "$mutation2" >&2
  echo 'mutation-check: BUG — the test failed, but not on the differential assertion' >&2
  exit 1
fi
# The differential is the strong claim; this is the one that NAMES the values
# it refuses. It has to fire too, or it is decoration: the mutation makes the
# page render the private versions, so the needles that read "3.0" and
# "sha256:draft-ctl" (the two a "draft-" substring would miss) must be able
# to see them.
if ! grep -qE 'renders the private value' <<<"$mutation2"; then
  echo "$mutation2" >&2
  echo 'mutation-check: BUG — the differential failed but the needles did not fire; they are reading nothing' >&2
  exit 1
fi
echo "mutation 2 caught: the anonymous-member widening is refused on the differential assertion, and the needles see the values"
echo "  the needles that fired, verbatim from the failing run:"
grep -oE 'renders the private value [^(]*' <<<"$mutation2" | sed -E 's/^renders the private value //; s/[[:space:]]+$//' | sort -u | sed 's/^/    /'

echo "== the restored tree must pass again =="
restored="$(run_case "$EXPLORE_TEST|$COUNT_TEST")"
if ! grep -q "^ok" <<<"$restored"; then
  echo "$restored" >&2
  echo "mutation-check: BUG — the tests fail on the restored tree" >&2
  exit 1
fi
echo "restored: ok"
echo "mutation-check: both new blocks of the privacy suite are real"
