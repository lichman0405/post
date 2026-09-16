#!/usr/bin/env bash
# Mutation check for tests/e2e/anonymous_e2e_test.go (T0801, the required
# test "anonymous e2e"): prove BOTH halves of that test can fail, so that
# "it passes" means "it measured something".
#
# The two mutations are the two failures the task exists to prevent, each
# applied to the surface under test and each reverted immediately:
#
#   1. gate-bypass — releasehttp's visible() stops consulting the project
#      read gate, so a private project's release is served to whoever asks.
#      This is the "private 不索引" half: the test must refuse to pass, and
#      the refusal must be the existence-hiding-404 assertion.
#
#   2. login-wall — releasehttp's reads require a principal, so an
#      anonymous caller is answered 401. This is the "登录墙不挡 public
#      research" half: the test must refuse to pass, and the refusal must
#      be the anonymous-200 assertion.
#
# The mutations are applied to cmd/api/releasehttp/release_handlers.go —
# the product code, NOT the test — because a mutation inside the test would
# only prove the test can fail at failing things. The file is restored from
# a byte-exact backup on every exit path (trap), and the script verifies
# the restore with cmp before it draws any conclusion.
#
# Usage: bash tests/e2e/anonymous-mutation-check.sh
set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$E2E_DIR/../.." && pwd)"
HANDLERS="$ROOT/cmd/api/releasehttp/release_handlers.go"
TEST_PKG="./tests/e2e/"
RUN='TestE2EAnonymousReleaseReads|TestE2EAnonymousReleaseHidesThePrivateProject'

if [ ! -f "$HANDLERS" ]; then
  echo "mutation-check: $HANDLERS not found" >&2
  exit 1
fi

WORK="$(mktemp -d)"
BACKUP="$WORK/release_handlers.go.orig"
cp "$HANDLERS" "$BACKUP"

restore() {
  cp "$BACKUP" "$HANDLERS"
}
cleanup() {
  restore
  rm -rf "$WORK"
}
trap cleanup EXIT

# restore_and_verify puts the file back and refuses to continue if the
# restore is not byte-identical — a mutation check that leaves the product
# tree edited has measured nothing and broken something.
restore_and_verify() {
  restore
  if ! cmp -s "$BACKUP" "$HANDLERS"; then
    echo "mutation-check: FATAL — could not restore $HANDLERS byte-for-byte" >&2
    exit 1
  fi
}

# mutate replaces exactly one occurrence of $1 with $2 in the handlers file.
mutate() {
  python3 - "$HANDLERS" "$1" "$2" <<'PY'
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

# run_case runs the test under the current tree and returns the output;
# the caller decides what the output must contain.
run_case() {
  (cd "$ROOT" && go test "$TEST_PKG" -run "$RUN" -count=1 2>&1) || true
}

GATE_SITE='	_, err := h.projects.Get(r.Context(), reader(r), projectID)
	if err != nil {
		writeReleaseError(w, r, err)
		return false
	}
	return true'
GATE_MUTATION='	return true'

WALL_MUTATION='	if _, ok := authhttp.PrincipalFrom(r.Context()); !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated, "authentication required")
		return false
	}
	_, err := h.projects.Get(r.Context(), reader(r), projectID)
	if err != nil {
		writeReleaseError(w, r, err)
		return false
	}
	return true'

echo "== mutation-check: the UNMUTATED tree must pass =="
baseline="$(run_case)"
if ! grep -q "^ok" <<<"$baseline"; then
  echo "$baseline" >&2
  echo "mutation-check: the tests do not pass on the unmutated tree; fix that first" >&2
  exit 1
fi
echo "baseline: ok"

echo "== mutation 1/2: the release gate is bypassed (a private release would be served) =="
mutate "$GATE_SITE" "$GATE_MUTATION"
mutation1="$(run_case)"
restore_and_verify
if grep -q "^ok" <<<"$mutation1"; then
  echo "$mutation1" >&2
  echo 'mutation-check: BUG — the tests pass while the release gate is bypassed' >&2
  exit 1
fi
for expected in \
  'anonymous GET .*/releases = 200, want 404 \(existence hiding\)' \
  'private project.s release = \{status:200'
do
  if ! grep -qE "$expected" <<<"$mutation1"; then
    echo "$mutation1" >&2
    echo "mutation-check: BUG — the tests failed, but not on $expected" >&2
    exit 1
  fi
done
echo "mutation 1 caught: the gate-bypass is refused on the existence-hiding assertion"

echo "== mutation 2/2: the release reads demand a principal (the login wall) =="
mutate "$GATE_SITE" "$WALL_MUTATION"
mutation2="$(run_case)"
restore_and_verify
if grep -q "^ok" <<<"$mutation2"; then
  echo "$mutation2" >&2
  echo 'mutation-check: BUG — the tests pass while anonymous reads are answered 401' >&2
  exit 1
fi
if ! grep -qE 'anonymous GET .*/releases = 401, want 200' <<<"$mutation2"; then
  echo "$mutation2" >&2
  echo 'mutation-check: BUG — the tests failed, but not on the anonymous-200 assertion' >&2
  exit 1
fi
echo "mutation 2 caught: the login wall on a public release is refused on the anonymous-200 assertion"

echo "== the restored tree must pass again =="
restored="$(run_case)"
if ! grep -q "^ok" <<<"$restored"; then
  echo "$restored" >&2
  echo "mutation-check: BUG — the tests fail on the restored tree" >&2
  exit 1
fi
echo "restored: ok"
echo "mutation-check: both halves of the anonymous release test are real"
