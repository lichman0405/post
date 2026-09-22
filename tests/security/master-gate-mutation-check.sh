#!/usr/bin/env bash
#
# T1206 — the mutation check for the Master Security/Quality Gate.
#
# A check that cannot fail is not evidence, and a gate assembled out of
# checks nobody has ever seen fail is a green line with a story attached.
# So this script breaks six things ON PURPOSE and requires the named
# instrument — and only that instrument — to be what notices:
#
#   1. secret-scan          plant an *.env.example carrying a secret-shaped
#                           value; the repository sweep must fail and name
#                           the file, and must go green again when it is
#                           removed.
#   2. permission-negative-e2e
#                           flip the anonymous-read assertion so it expects
#                           a private project to be readable (200 instead of
#                           the existence-hiding 404); the test must fail.
#   3. owasp-smoke          delete a guard from the product — the HSTS header
#                           the edge stamps — and run the gate's owasp row
#                           against the mutated tree; it must fail naming the
#                           missing header.
#   4. master gate          register a decoy check that prints "skipping" and
#                           exits 0: the gate must call it UNSUBSTANTIATED
#                           and exit non-zero. This is the silent-skip rule
#                           measured on the real gate, from outside it
#                           (`--selftest` measures it from inside).
#   5. absence manifest     delete the SAST entry from a copy of
#                           ops/security/absent-checks.json; the checker must
#                           reject the manifest and say which capability went
#                           missing. A gap that can be deleted quietly is not
#                           written down.
#   6. vuln-go              swap the INPUT that decides the dependency
#                           audit's verdict: a synthetic vulnerability
#                           database naming a symbol this tree calls, reached
#                           through a shim on PATH. The row must go red. An
#                           audit row nobody has ever seen fail is a row that
#                           may be wired to nothing.
#
# Why every mutation happens in a COPY
# ------------------------------------
# Three of the six mutations are edits to files this task may not touch
# (internal/security/headers.go is product code) and all six would leave the
# tree dirty if a run were interrupted. The copy is made with tar, the
# mutations are applied there, and the run opens and closes by comparing a
# sha256 manifest of every file of the working tree against the copy: the
# trees are identical before the first mutation, and the working tree is
# byte-identical at the end. So the instruments being measured are this
# tree's instruments, and the reviewed tree is never the one left mutated.
#
# Runnable on a host with bash + coreutils + tar + python3 + go + govulncheck +
# the dev infrastructure (PostgreSQL + Redis) for mutation 3. It does not skip:
# a missing prerequisite is exit 2 with the reason.
#
# Exit codes
#   0  all six mutations were caught, and the mutating tree was left clean
#   1  an instrument did NOT catch its mutation (or the tree was left dirty)
#   2  a required tool or service is missing (never a silent pass)
#   3  usage error
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

PG_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"
REDIS_ADDR="${POST_G3_REDIS_ADDR:-127.0.0.1:6379}"

WORK="$(mktemp -d)"
SCRATCH="$WORK/tree"
mkdir -p "$SCRATCH"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }
step() { printf '\n== %s ==\n' "$*"; }

GOVULNCHECK_HINT="go install golang.org/x/vuln/cmd/govulncheck@latest"
for tool in tar python3 go sha256sum govulncheck; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "master-gate-mutation-check: $tool is not on PATH; the mutations cannot be run and a skip is not a pass." >&2
    [ "$tool" = govulncheck ] && echo "  install it with: $GOVULNCHECK_HINT" >&2
    exit 2
  }
done
if ! python3 "$ROOT/scripts/pg-ready.py" "$PG_URL" >/dev/null 2>&1; then
  echo "master-gate-mutation-check: no PostgreSQL accepting connections at $PG_URL — mutation 3 needs it (make infra-up)." >&2
  exit 2
fi
# The probe runs in a subshell and the fd closes with it. Written with braces
# when it does not: `exec 3<&- 2>/dev/null` at the top level redirects THIS
# SHELL's stderr for the rest of the script, which is how a script reports
# every failure to /dev/null and exits 1 without saying why. (It was measured
# here: the failure summary below went missing until this line grew its braces.)
if ! ( exec 3<>"/dev/tcp/${REDIS_ADDR%:*}/${REDIS_ADDR#*:}" ) 2>/dev/null; then
  echo "master-gate-mutation-check: no Redis at $REDIS_ADDR — mutation 3 needs it (make infra-up)." >&2
  exit 2
fi

# ---------------------------------------------------------------------------
# The copy, and the two manifests that make "the same tree" checkable.
# ---------------------------------------------------------------------------
tree_manifest() { # tree_manifest <dir> > manifest
  ( cd "$1" && find . -type f \
      -not -path './.git' -not -path './.git/*' \
      -not -path './node_modules/*' -not -path '*/node_modules/*' \
      -not -path './.next/*' -not -path '*/.next/*' -not -path './out/*' -not -path './.venv/*' \
      -not -path '*/__pycache__/*' \
      -print0 | sort -z | xargs -0 sha256sum )
}

step "copy the tree under review into $SCRATCH"
tar -C "$ROOT" --exclude=.git --exclude=node_modules --exclude=.next --exclude=out --exclude=.venv \
  -cf - . | tar -C "$SCRATCH" -xf -
# The browser and Next toolchains live in node_modules, which is not copied
# (it is not source). The web-dependent halves of mutations 3's S8 checks get
# the same packages through a symlink, so the copy is not a weaker tree.
[ -d "$ROOT/apps/web/node_modules" ] && mkdir -p "$SCRATCH/apps/web" \
  && ln -sfn "$ROOT/apps/web/node_modules" "$SCRATCH/apps/web/node_modules"
ok "copied $(du -sh "$SCRATCH" | cut -f1) of the working tree"

tree_manifest "$ROOT" >"$WORK/root.before"
tree_manifest "$SCRATCH" >"$WORK/scratch.before"
if diff -q "$WORK/root.before" "$WORK/scratch.before" >/dev/null; then
  ok "the copy is byte-for-byte the tree under review ($(wc -l <"$WORK/root.before") file(s) hashed)"
else
  fail "the copy differs from the tree under review before any mutation:"
  diff "$WORK/root.before" "$WORK/scratch.before" | head -10 | sed 's/^/     /'
fi

pristine() { # pristine <relpath> — keep ROOT's copy to restore from
  mkdir -p "$WORK/pristine/$(dirname "$1")"
  cp "$ROOT/$1" "$WORK/pristine/$1"
}
restored_ok() { # restored_ok <relpath>
  if cmp -s "$ROOT/$1" "$SCRATCH/$1"; then
    ok "restored $1 to the tree's own content (byte-identical again)"
  else
    fail "restore of $1 does not match the tree under review"
  fi
}

# ---------------------------------------------------------------------------
# 1. secret scan — a planted example file must make the sweep fail, and
#    removing it must make the sweep green again. The planted value is not a
#    credential of any shape a secret scanner elsewhere would flag: it is a
#    secret-shaped KEY with a non-placeholder value, which is exactly what
#    internal/config.ScanExampleContent exists to catch.
# ---------------------------------------------------------------------------
step "1. secret scan: plant a secret-shaped value in a committed example file"
PLANT="ops/security/planted-mutation-check.env.example"
printf 'POST_GITEA_TOKEN=planted-value-not-a-documented-placeholder\n' >"$SCRATCH/$PLANT"
if ( cd "$SCRATCH" && go test ./internal/config -run TestRepoExampleFilesAreSecretFree -count=1 -v ) >"$WORK/planted.log" 2>&1; then
  fail "MUTATION 1 NOT CAUGHT: the secret scan passed with a planted secret in $PLANT — the sweep is not measuring the tree"
else
  if grep -q 'planted-mutation-check' "$WORK/planted.log"; then
    ok "MUTATION 1: the sweep failed and named the planted file"
    grep -E 'secret-shaped value|FAIL' "$WORK/planted.log" | head -3 | sed 's/^/     /'
  else
    fail "MUTATION 1 CAUGHT BUT SILENT: the sweep failed without naming $PLANT (the finding must locate the file)"
    tail -5 "$WORK/planted.log" | sed 's/^/     /'
  fi
fi
rm -f "$SCRATCH/$PLANT"
if ( cd "$SCRATCH" && go test ./internal/config -run TestRepoExampleFilesAreSecretFree -count=1 ) >"$WORK/unplanted.log" 2>&1; then
  ok "MUTATION 1 reverted: the sweep is green again with the planted file removed"
else
  fail "the sweep is still red after removing the planted file — the red above was not caused by the mutation"
  tail -5 "$WORK/unplanted.log" | sed 's/^/     /'
fi

# ---------------------------------------------------------------------------
# 2. permission negative e2e — flip the assertion so that a guessed private
#    id is expected to be readable anonymously. The negative path is the
#    point (privacy_e2e_test.go), so breaking it must be what the test
#    notices; the message is flipped with the condition so the failure reads
#    as the mutation it is.
# ---------------------------------------------------------------------------
step "2. permission negative e2e: flip the anonymous-read assertion to expect 200"
pristine tests/e2e/privacy_e2e_test.go
python3 - "$SCRATCH/tests/e2e/privacy_e2e_test.go" <<'PY'
import sys
p = sys.argv[1]
src = open(p, encoding="utf-8").read()
cond = "if resp.StatusCode != http.StatusNotFound {"
want = "want 404 (existence hiding)"
assert src.count(cond) == 2, f"expected 2 occurrences of the condition, found {src.count(cond)}"
assert src.count(want) == 2, f"expected 2 occurrences of the message, found {src.count(want)}"
src = src.replace(cond, "if resp.StatusCode != http.StatusOK {", 1)
src = src.replace(want, "want 200 (MUTATED: the negative assertion was flipped)", 1)
open(p, "w", encoding="utf-8").write(src)
print("mutation applied: the first anonymous-read assertion now expects 200")
PY
if ( cd "$SCRATCH" && go test ./tests/e2e -run TestE2EPrivacyNegative -count=1 ) >"$WORK/privacy.log" 2>&1; then
  fail "MUTATION 2 NOT CAUGHT: the privacy e2e passed with the anonymous-read assertion flipped — it is not asserting the negative path"
else
  if grep -q 'MUTATED: the negative assertion was flipped' "$WORK/privacy.log"; then
    ok "MUTATION 2: the privacy e2e failed on the flipped assertion"
    grep -E 'MUTATED|--- FAIL' "$WORK/privacy.log" | head -3 | sed 's/^/     /'
  else
    fail "MUTATION 2 CAUGHT BY SOMETHING ELSE: the test failed, but not on the assertion that was flipped:"
    tail -6 "$WORK/privacy.log" | sed 's/^/     /'
  fi
fi
cp "$WORK/pristine/tests/e2e/privacy_e2e_test.go" "$SCRATCH/tests/e2e/privacy_e2e_test.go"
restored_ok tests/e2e/privacy_e2e_test.go
if ( cd "$SCRATCH" && go test ./tests/e2e -run TestE2EPrivacyNegative -count=1 ) >"$WORK/privacy-restored.log" 2>&1; then
  ok "MUTATION 2 reverted: the privacy e2e is green again"
else
  fail "the privacy e2e is still red after the restore — the red above was not caused by the mutation"
  tail -6 "$WORK/privacy-restored.log" | sed 's/^/     /'
fi

# ---------------------------------------------------------------------------
# 3. owasp smoke — delete a product guard and run the gate's own owasp row
#    against the mutated tree. The row is the real one: the same
#    tests/security/owasp-smoke.sh, pointed at the mutated copy through
#    G3_REPO_ROOT (the escape hatch the smoke documents for exactly this
#    case), building cmd/api from that copy and standing up real PostgreSQL
#    and real Redis behind it.
# ---------------------------------------------------------------------------
step "3. owasp smoke: delete the HSTS guard from the product and run the gate's owasp row"
pristine internal/security/headers.go
python3 - "$SCRATCH/internal/security/headers.go" <<'PY'
import sys
p = sys.argv[1]
src = open(p, encoding="utf-8").read()
line = "\t\t\th.Set(HeaderStrictTransport, HSTSValue)\n"
assert src.count(line) == 1, f"expected the HSTS stamp exactly once, found {src.count(line)} — this mutation has rotted"
open(p, "w", encoding="utf-8").write(src.replace(line, ""))
print("mutation applied: the edge no longer stamps Strict-Transport-Security")
PY
G3_REPO_ROOT="$SCRATCH" bash tests/security/owasp-smoke.sh >"$WORK/owasp.log" 2>&1
owasp_rc=$?
if [ "$owasp_rc" -eq 0 ]; then
  fail "MUTATION 3 NOT CAUGHT: the owasp smoke passed with the HSTS guard deleted"
else
  if grep -q 'Strict-Transport-Security' "$WORK/owasp.log"; then
    ok "MUTATION 3: the owasp smoke failed naming the missing header (exit $owasp_rc)"
    grep -E '^FAIL' "$WORK/owasp.log" | head -3 | sed 's/^/     /'
  else
    fail "MUTATION 3 CAUGHT BY SOMETHING ELSE: the smoke failed (exit $owasp_rc) without naming the guard that was deleted:"
    grep -E '^FAIL' "$WORK/owasp.log" | head -5 | sed 's/^/     /'
  fi
fi
cp "$WORK/pristine/internal/security/headers.go" "$SCRATCH/internal/security/headers.go"
restored_ok internal/security/headers.go
G3_REPO_ROOT="$SCRATCH" bash tests/security/owasp-smoke.sh >"$WORK/owasp-restored.log" 2>&1
if [ $? -eq 0 ] && grep -q '^G3 security-smoke: OK' "$WORK/owasp-restored.log"; then
  ok "MUTATION 3 reverted: the owasp smoke is green again against the restored tree"
else
  fail "the owasp smoke is still red after the restore — the red above was not caused by the mutation"
  tail -6 "$WORK/owasp-restored.log" | sed 's/^/     /'
fi

# ---------------------------------------------------------------------------
# 4. the gate's silent-skip rule, measured on the real gate from outside it.
#    A decoy check exits 0 having printed "skipping": exit 0 alone must not
#    be able to make the gate green.
# ---------------------------------------------------------------------------
step "4. master gate: a check that 'skips' and exits 0 must be red, not green"
DECOYS="$WORK/decoys.sh"
cat >"$DECOYS" <<'DECOY'
add_check decoy-silent-skip \
  "prints a skip announcement and exits 0" "mutation check" "" \
  "printf 'this check is skipping itself today\n'" \
  '^ok   '
add_check decoy-real-pass \
  "prints its declared evidence" "mutation check" "" \
  "printf 'ok   decoy measured something\n'" \
  '^ok   decoy measured something'
DECOY
bash tests/security/master-security-gate.sh --extra-checks "$DECOYS" --only decoy-silent-skip >"$WORK/decoy.log" 2>&1
decoy_rc=$?
if [ "$decoy_rc" -eq 0 ]; then
  fail "MUTATION 4 NOT CAUGHT: the gate exited 0 with a check that only announced it was skipping"
elif grep -q 'UNSUBSTANTIATED' "$WORK/decoy.log"; then
  ok "MUTATION 4: the gate called the silent skip UNSUBSTANTIATED and exited $decoy_rc"
  grep -E 'UNSUBSTANTIATED|never printed' "$WORK/decoy.log" | head -3 | sed 's/^/     /'
else
  fail "MUTATION 4 CAUGHT BY SOMETHING ELSE: exit $decoy_rc without UNSUBSTANTIATED:"
  tail -8 "$WORK/decoy.log" | sed 's/^/     /'
fi
# The control: the same accounting must still say yes to a check that really
# printed its evidence, or "red" above would just mean this gate hates decoys.
bash tests/security/master-security-gate.sh --extra-checks "$DECOYS" --only decoy-real-pass >"$WORK/decoy-ok.log" 2>&1
if [ $? -eq 0 ] && grep -q '^  ok              decoy-real-pass' "$WORK/decoy-ok.log"; then
  ok "MUTATION 4 control: a decoy that prints its evidence is still green (the rule is not a blanket refusal)"
else
  fail "the control decoy was not green — the silent-skip rule cannot tell a skip from a measured pass"
  tail -8 "$WORK/decoy-ok.log" | sed 's/^/     /'
fi

# ---------------------------------------------------------------------------
# 5. absence manifest — delete a named gap and require the checker to
#    refuse. Deleting an absence is the quietest way to make a security
#    inventory look complete, so it has to be the loudest failure.
# ---------------------------------------------------------------------------
step "5. absence manifest: delete the SAST gap from a copy and require a refusal"
cp "$SCRATCH/ops/security/absent-checks.json" "$WORK/manifest-with-sast.json"
python3 - "$WORK/manifest-with-sast.json" <<'PY'
import json, sys
p = sys.argv[1]
m = json.load(open(p, encoding="utf-8"))
before = len(m["absent"])
m["absent"] = [a for a in m["absent"] if a.get("id") != "sast"]
for item in m["items"]:
    if item.get("id") == "sast":
        item["absent_id"] = "sast-which-is-no-longer-listed"
assert len(m["absent"]) == before - 1, "the mutation did not remove an entry"
json.dump(m, open(p, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
print(f"mutation applied: {before} absent entries -> {len(m['absent'])}")
PY
python3 tests/security/check-absent-manifest.py --manifest "$WORK/manifest-with-sast.json" --repo "$ROOT" >"$WORK/manifest.log" 2>&1
manifest_rc=$?
if [ "$manifest_rc" -eq 0 ]; then
  fail "MUTATION 5 NOT CAUGHT: the checker accepted a manifest with the SAST gap deleted"
elif grep -q 'sast' "$WORK/manifest.log"; then
  ok "MUTATION 5: the checker refused the manifest (exit $manifest_rc) and named the missing capability"
  grep -E '^FAIL' "$WORK/manifest.log" | head -3 | sed 's/^/     /'
else
  fail "MUTATION 5 CAUGHT BY SOMETHING ELSE: exit $manifest_rc without naming SAST:"
  tail -6 "$WORK/manifest.log" | sed 's/^/     /'
fi

# ---------------------------------------------------------------------------
# 6. dependency audit — the tree is not touched here; the INPUT that decides
#    the verdict is. The real govulncheck binary is put behind a shim that
#    points -db at one synthetic advisory asserting a vulnerability in a
#    symbol this tree calls (pgxpool.New and friends). A scanner that really
#    reads its database has to report it and exit non-zero, and the gate's
#    vuln-go row has to go red naming the advisory. An audit row that has
#    never been seen to fail is a row that might be wired to nothing — this
#    is the one mutation the dependency audits were missing.
# ---------------------------------------------------------------------------
step "6. dependency audit: a database naming a called symbol must make the vuln-go row red"
FAKEDB="$WORK/vulndb"
SHIMBIN="$WORK/shim-bin"
REAL_GOVULNCHECK="$(command -v govulncheck)"
mkdir -p "$FAKEDB" "$SHIMBIN"
python3 - "$FAKEDB/GO-2026-9001.json" <<'PY'
import json, sys

entry = {
    "schema_version": "1.3.1",
    "id": "GO-2026-9001",
    "modified": "2026-09-01T00:00:00Z",
    "published": "2026-09-01T00:00:00Z",
    "aliases": ["CVE-2026-9001"],
    "details": (
        "SYNTHETIC ENTRY created by tests/security/master-gate-mutation-check.sh to prove the "
        "vuln-go row can fail. It claims a vulnerability in a symbol this tree calls, so a "
        "scanner really reading its database must report it and exit non-zero. The gate's own "
        "run never sees it: it reads the real database at https://vuln.go.dev."
    ),
    "affected": [{
        "package": {"name": "github.com/jackc/pgx/v5", "ecosystem": "Go"},
        "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}, {"fixed": "99.99.99"}]}],
        "ecosystem_specific": {"imports": [{
            "path": "github.com/jackc/pgx/v5/pgxpool",
            "symbols": ["New", "NewWithConfig", "ParseConfig"],
        }]},
    }],
    "references": [{"type": "REPORT", "url": "https://pkg.go.dev/vuln/GO-2026-9001"}],
    "database_specific": {"url": "https://pkg.go.dev/vuln/GO-2026-9001"},
}
with open(sys.argv[1], "w", encoding="utf-8") as fh:
    json.dump(entry, fh, indent=2)
print("mutation applied: one synthetic advisory for github.com/jackc/pgx/v5")
PY
cat >"$SHIMBIN/govulncheck" <<SHIM
#!/usr/bin/env bash
# the real scanner, reading the synthetic database in this run's scratch dir
exec "$REAL_GOVULNCHECK" -db=file://$FAKEDB "\$@"
SHIM
chmod +x "$SHIMBIN/govulncheck"
PATH="$SHIMBIN:$PATH" bash tests/security/master-security-gate.sh --only vuln-go >"$WORK/vuln.log" 2>&1
vuln_rc=$?
if [ "$vuln_rc" -eq 0 ]; then
  fail "MUTATION 6 NOT CAUGHT: the vuln-go row was green while the database named a symbol this tree calls"
elif grep -q 'GO-2026-9001' "$WORK/vuln.log"; then
  ok "MUTATION 6: the vuln-go row went red (exit $vuln_rc) naming the advisory"
  grep -E '^FAIL|GO-2026-9001|affected by [0-9]+ vulnerabilit' "$WORK/vuln.log" | head -4 | sed 's/^/     /'
else
  fail "MUTATION 6 CAUGHT BY SOMETHING ELSE: exit $vuln_rc without naming the advisory:"
  tail -8 "$WORK/vuln.log" | sed 's/^/     /'
fi
# Revert: with the shim gone the row reads the real database at
# https://vuln.go.dev and has to be green again, or the red above was not
# caused by the mutation.
bash tests/security/master-security-gate.sh --only vuln-go >"$WORK/vuln-restored.log" 2>&1
if [ $? -eq 0 ] && grep -q 'No vulnerabilities found' "$WORK/vuln-restored.log"; then
  ok "MUTATION 6 reverted: the row is green again against the real database"
else
  fail "the vuln-go row is still red without the shim — the red above was not caused by the mutation"
  tail -6 "$WORK/vuln-restored.log" | sed 's/^/     /'
fi

# ---------------------------------------------------------------------------
# The tree that gets reviewed is the tree that was reviewed.
# ---------------------------------------------------------------------------
step "the working tree is byte-identical to what it was at the start"
tree_manifest "$ROOT" >"$WORK/root.after"
if diff -q "$WORK/root.before" "$WORK/root.after" >/dev/null; then
  ok "no mutation touched the working tree ($(wc -l <"$WORK/root.after") file(s) hashed before and after)"
else
  fail "the working tree changed during this run:"
  diff "$WORK/root.before" "$WORK/root.after" | head -10 | sed 's/^/     /'
fi

printf '\n== master-gate-mutation-check: summary ==\n'
if [ "$FAILS" -ne 0 ]; then
  printf 'master-gate-mutation-check: FAILED — %d finding(s)\n' "$FAILS" >&2
  exit 1
fi
printf 'master-gate-mutation-check: OK — six mutations, six instruments that said no, and the tree left as it was found\n'
exit 0
