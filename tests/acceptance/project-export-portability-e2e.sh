#!/usr/bin/env bash
#
# T1208 — 完整 Project 可移植导出, end to end against the real stack.
#
# What this gate asserts, in the order it asserts it:
#
#   1. a released project exports as a portable bundle carrying all four
#      classes CLAUDE.md §8 says a Release pins — the RSG snapshot, the Git
#      ref and commit, the blob contents, and the schema/policy version
#      identifiers;
#   2. the export is READ-ONLY: a fingerprint of the source (row counts,
#      branch heads, the blob content-hash set, every state hash, the
#      provider's refs) is identical before and after;
#   3. the bundle imports into a CLEAN environment — a database created from
#      the product's own migrations, an empty Gitea organisation, an empty
#      object-store prefix — and the import side re-derives the four classes
#      FROM ITS OWN REBUILT STORES, with the same code the export side used;
#   4. three kinds of tampering (one blob byte, one line of the manifest, a
#      swapped commit) are each REFUSED at import, with the location named,
#      and with the transport digests recomputed first so that what fires is
#      the product's content addressing rather than this driver's;
#   5. exporting twice gives the same content digest, and importing onto an
#      existing target is refused rather than silently merged;
#   6. it leaves nothing behind — no process, no port, no database, no
#      repository, no temp directory — so the next run starts immediately.
#
# Real services only: PostgreSQL, MinIO and Gitea are the dev stack
# (`make infra-up`); there is no in-memory stand-in on any hop. The one hop
# the PRODUCT has no write path for is blobs — see the assemble stage, which
# says so out loud rather than hiding it.
#
# NOT named *-real-services-e2e.sh on purpose: that glob is wired to a
# gates.json job by internal/devorchestrator/gate_spec_test.go, and adding a
# job to specs/orchestrator/gates.json is outside a Worker's write scope. The
# wiring is reported as a follow-up instead.
#
# Usage: bash tests/acceptance/project-export-portability-e2e.sh
# Exit:  0 when every stage above held; 1 otherwise.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

GATE="project-export-portability"
FAILED=0

step() { printf '\n=== %s\n' "$*"; }
ok()   { printf '  ok    %s\n' "$*"; }
bad()  { printf '  FAIL  %s\n' "$*" >&2; FAILED=1; }
note() { printf '  --    %s\n' "$*"; }
die()  { printf 'G3 %s: FAILED — %s\n' "$GATE" "$*" >&2; exit 1; }

# --- configuration ---------------------------------------------------------

PG_USER="${POST_DB_USER:-postgres}"
PG_PW="${POST_DB_PASSWORD:-postgres_dev_pw}"
PG_HOST="${POST_DB_HOST:-127.0.0.1}"
PG_PORT="${POST_DB_PORT:-5432}"
SRC_DB="${T1208_SRC_DB:-post_seed_demo_t1208_src}"
TGT_DB="${T1208_TGT_DB:-post_seed_demo_t1208_tgt}"
MUT_DB="${T1208_MUT_DB:-post_seed_demo_t1208_mut}"
GITEA_BASE="${POST_GITEA_BASE_URL:-http://127.0.0.1:3000}"
GITEA_ADMIN_USER="${GITEA_ADMIN_USER:-postadmin}"
GITEA_ADMIN_PASSWORD="${GITEA_ADMIN_PASSWORD:-postadmin_dev_pw}"
TARGET_ORG="${T1208_TARGET_ORG:-t1208-target}"
BLOB_ENDPOINT="${POST_BLOB_ENDPOINT:-http://127.0.0.1:9000}"
BLOB_BUCKET="${POST_BLOB_BUCKET:-post}"
BLOB_ACCESS="${POST_BLOB_ACCESS_KEY:-minio_dev}"
BLOB_SECRET="${POST_BLOB_SECRET_KEY:-minio_dev_pw}"
PROJECT="${T1208_PROJECT:-demo-mof-humidity-separation}"
PLAN="${T1208_PLAN:-examples/seed-demo/demo-plan.json}"

db_url() { printf 'postgres://%s:%s@%s:%s/%s?sslmode=disable' "$PG_USER" "$PG_PW" "$PG_HOST" "$PG_PORT" "$1"; }
ADMIN_URL="$(db_url postgres)"
psql_admin() { psql "$ADMIN_URL" -tAc "$1"; }

WORK="$(mktemp -d /tmp/t1208-e2e-XXXXXX)"
API_PID=""
TOKEN=""
# Assigned later; declared here because cleanup runs under `set -u` even when
# the run dies before it gets that far, and an unbound variable in the trap
# would abort the cleanup half way through.
SRC_REPO=""
TGT_REPO=""

cleanup() {
  local rc=$?
  if [ -n "$API_PID" ] && kill -0 "$API_PID" 2>/dev/null; then
    kill "$API_PID" 2>/dev/null
    for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$API_PID" 2>/dev/null || break; sleep 0.3; done
    kill -9 "$API_PID" 2>/dev/null
  fi
  if [ -n "$TOKEN" ]; then
    # The repositories this run created, both of them: the source one the
    # product's provisioning created and the target one the importer made.
    for repo in "${SRC_REPO:-}" "${TGT_REPO:-}"; do
      [ -n "$repo" ] || continue
      curl -fsS -X DELETE -H "Authorization: token $TOKEN" "$GITEA_BASE/api/v1/repos/$repo" >/dev/null 2>&1 || true
    done
    [ -n "${TARGET_ORG:-}" ] && curl -fsS -X DELETE -H "Authorization: token $TOKEN" "$GITEA_BASE/api/v1/orgs/$TARGET_ORG" >/dev/null 2>&1 || true
  fi
  for db in "$TGT_DB" "$SRC_DB" "$MUT_DB"; do
    psql_admin "DROP DATABASE IF EXISTS $db WITH (FORCE)" >/dev/null 2>&1 || true
  done
  rm -rf "$WORK"
  # The repo root must be exactly as it was found: the seed writes its
  # summary beside the tree unless it is told otherwise, and a residue check
  # that only looks at processes would miss it.
  rm -f "$ROOT/.seed-demo-summary.json"
  exit $rc
}
trap cleanup EXIT

# --- preflight -------------------------------------------------------------

step "preflight: the real services, and the tools this gate needs"
for tool in go git psql curl python3; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is not on PATH"
done
pg_isready -h "$PG_HOST" -p "$PG_PORT" >/dev/null 2>&1 || die "PostgreSQL is not accepting connections at $PG_HOST:$PG_PORT (make infra-up)"
curl -fsS "$GITEA_BASE/api/v1/version" >/dev/null 2>&1 || die "Gitea is not answering at $GITEA_BASE (make infra-up)"
curl -fsS "$BLOB_ENDPOINT/minio/health/live" >/dev/null 2>&1 || die "the object store is not answering at $BLOB_ENDPOINT (make infra-up)"
ok "PostgreSQL $PG_HOST:$PG_PORT, Gitea $GITEA_BASE, object store $BLOB_ENDPOINT"

TOKEN="$(curl -fsS -u "${GITEA_ADMIN_USER}:${GITEA_ADMIN_PASSWORD}" \
  -X POST "$GITEA_BASE/api/v1/users/${GITEA_ADMIN_USER}/tokens" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"t1208-gate-$(date +%s)\",\"scopes\":[\"write:repository\",\"write:user\",\"write:admin\",\"write:organization\"]}" \
  2>/dev/null | sed -n 's/.*"sha1":"\([^"]*\)".*/\1/p')"
[ -n "$TOKEN" ] || die "could not mint a Gitea token for ${GITEA_ADMIN_USER} — the Git half of this gate needs a real provider credential"
ok "minted a Gitea service token (${#TOKEN} chars, never printed)"

export POST_DATABASE_URL="$(db_url "$SRC_DB")"
export POST_BLOB_ENDPOINT="$BLOB_ENDPOINT" POST_BLOB_BUCKET="$BLOB_BUCKET"
export POST_BLOB_ACCESS_KEY="$BLOB_ACCESS" POST_BLOB_SECRET_KEY="$BLOB_SECRET"
export POST_GITEA_BASE_URL="$GITEA_BASE" POST_GITEA_TOKEN="$TOKEN"
export POST_TGT_DATABASE_URL="$(db_url "$TGT_DB")"
export POST_TGT_BLOB_ENDPOINT="$BLOB_ENDPOINT" POST_TGT_BLOB_BUCKET="$BLOB_BUCKET"
export POST_TGT_BLOB_ACCESS_KEY="$BLOB_ACCESS" POST_TGT_BLOB_SECRET_KEY="$BLOB_SECRET"
export POST_TGT_GITEA_BASE_URL="$GITEA_BASE" POST_TGT_GITEA_TOKEN="$TOKEN"
export GIT_CONFIG_NOSYSTEM=1 GIT_TERMINAL_PROMPT=0

step "building the driver and the migration tool"
mkdir -p "$WORK/bin"
go build -o "$WORK/bin/portability" ./tests/acceptance/portability || die "go build ./tests/acceptance/portability failed"
go build -o "$WORK/bin/rddev" ./cmd/rddev || die "go build ./cmd/rddev failed"
go build -o "$WORK/bin/post-api" ./cmd/api || die "go build ./cmd/api failed"
ok "built portability, rddev and post-api into $WORK/bin"

PORTABILITY="$WORK/bin/portability"

# --- 1. the source fixture -------------------------------------------------

step "source: an empty database, the product's own migrations, and the demo build"
psql_admin "DROP DATABASE IF EXISTS $SRC_DB WITH (FORCE)" >/dev/null
SEED_DEMO_SUMMARY="$WORK/seed-summary.json" \
  bash ops/seed-demo.sh --external 0 --reset --db "$SRC_DB" >"$WORK/seed.log" 2>&1 \
  || die "ops/seed-demo.sh failed; last lines: $(tail -5 "$WORK/seed.log" | tr '\n' ' ')"
ok "seeded $SRC_DB through the product's own API (log: $WORK/seed.log)"

# Provisioning is what gives the project a real repository. The seed ran with
# --external=0 (no token), so the project row is provision_status='pending'
# with a null repository id, and the API back-fills pending projects at
# startup. This is the PRODUCT creating the repository, not the gate.
step "source: provisioning the project against the real GitProvider"
API_PORT="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')"
MCP_PORT="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')"
POST_ENV=dev POST_API_ADDR="127.0.0.1:$API_PORT" POST_MCP_ADDR="127.0.0.1:$MCP_PORT" \
POST_DB_HOST="$PG_HOST" POST_DB_PORT="$PG_PORT" POST_DB_USER="$PG_USER" POST_DB_PASSWORD="$PG_PW" \
POST_DB_NAME="$SRC_DB" POST_DB_SSLMODE=disable POST_REDIS_ADDR="${POST_REDIS_ADDR:-127.0.0.1:6379}" \
POST_BLOB_ENDPOINT="$BLOB_ENDPOINT" POST_BLOB_ACCESS_KEY="$BLOB_ACCESS" POST_BLOB_SECRET_KEY="$BLOB_SECRET" \
POST_BLOB_BUCKET="$BLOB_BUCKET" POST_BLOB_USE_TLS=false POST_GITEA_BASE_URL="$GITEA_BASE" POST_GITEA_TOKEN="$TOKEN" \
POST_GITEA_WEBHOOK_URL="http://127.0.0.1:$API_PORT/api/v1/git/hooks/gitea" \
POST_GITEA_ADMIN_USER="$GITEA_ADMIN_USER" POST_GITEA_ADMIN_PASSWORD="$GITEA_ADMIN_PASSWORD" \
  "$WORK/bin/post-api" >"$WORK/api.log" 2>&1 &
API_PID=$!
READY=0
for _ in $(seq 1 60); do
  kill -0 "$API_PID" 2>/dev/null || die "the API exited during startup; last lines: $(tail -5 "$WORK/api.log" | tr '\n' ' ')"
  curl -fsS "http://127.0.0.1:$API_PORT/healthz" >/dev/null 2>&1 && { READY=1; break; }
  sleep 0.5
done
[ "$READY" = "1" ] || die "the API did not become healthy in 30s"

SRC_REPO=""
for _ in $(seq 1 30); do
  SRC_REPO="$(psql "$(db_url "$SRC_DB")" -tAc "SELECT coalesce(git_repository_external_id,'') FROM projects WHERE slug = '$PROJECT'" 2>/dev/null | tr -d ' ')"
  [ -n "$SRC_REPO" ] && break
  sleep 1
done
[ -n "$SRC_REPO" ] || die "the project was never provisioned: projects.git_repository_external_id is still null after 30s (log: $WORK/api.log)"
ok "the product provisioned the project's repository: $SRC_REPO"

GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraHeader GIT_CONFIG_VALUE_0="Authorization: token $TOKEN" \
  git ls-remote "$GITEA_BASE/$SRC_REPO.git" >/dev/null 2>&1 \
  && ok "the repository is reachable from the git CLI" \
  || die "git ls-remote cannot reach $GITEA_BASE/$SRC_REPO.git"

# A real commit on a real branch, through a real push: the file history the
# export has to carry. main is push-protected by the provider's own rule, so
# the fixture pushes a branch — which is also how a scientist's work reaches
# the platform (tests/acceptance/mof-canonical-workflow.sh's [git] step).
step "source: a real commit pushed into the repository with the real git CLI"
FIXTURE_BRANCH="t1208-fixture"
FIXTURE_FILE="t1208-fixture.md"
GITDIR="$WORK/git-fixture"
git init -q -b "$FIXTURE_BRANCH" "$GITDIR" || die "git init failed"
cat > "$GITDIR/$FIXTURE_FILE" <<'EOF'
# T1208 portable export fixture

This file exists so the project's repository carries a real file history: the
export has to carry Git's text truth, and a repository with only the
provider's bootstrap commit would make that claim vacuous.
EOF
# The credential rides the environment, never argv (internal/gitprovider's
# gitEnv, tests/acceptance/gitea-real-services-e2e.sh).
git -C "$GITDIR" -c user.email=gate@post.local -c user.name="T1208 gate" add -A >/dev/null
git -C "$GITDIR" -c user.email=gate@post.local -c user.name="T1208 gate" commit -q -m "T1208: the fixture file the portable export carries" || die "git commit failed"
FIXTURE_SHA="$(git -C "$GITDIR" rev-parse HEAD)"
GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraHeader GIT_CONFIG_VALUE_0="Authorization: token $TOKEN" \
  git -C "$GITDIR" push -q "$GITEA_BASE/$SRC_REPO.git" "HEAD:refs/heads/$FIXTURE_BRANCH" \
  || die "git push failed"
PUSHED_SHA="$(GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraHeader GIT_CONFIG_VALUE_0="Authorization: token $TOKEN" \
  git ls-remote "$GITEA_BASE/$SRC_REPO.git" "refs/heads/$FIXTURE_BRANCH" | awk '{print $1}')"
[ "$PUSHED_SHA" = "$FIXTURE_SHA" ] || die "the provider's ref for $FIXTURE_BRANCH is $PUSHED_SHA, the push made $FIXTURE_SHA"
ok "pushed ${FIXTURE_SHA:0:12}… to $FIXTURE_BRANCH and read it back from the provider"

# --- 2. blobs --------------------------------------------------------------

step "source: assembling the blob bytes into the real object store"
note "this build has NO product-side blob write path: blobs.CreateBlob's only"
note "caller is tests/integration/manifest_test.go, and the only production"
note "caller of S3 is cmd/api/backupdr's disaster-recovery drill. The seed"
note "therefore wrote blob ROWS whose bytes were never stored. The TEST"
note "assembles them here — real rows, real bucket, real bytes — in the shape"
note "tests/integration/asset_canonical_e2e_test.go:386 already uses."
"$PORTABILITY" assemble --project "$PROJECT" --plan "$PLAN" >"$WORK/assemble.log" 2>&1 \
  || die "assemble failed: $(tail -5 "$WORK/assemble.log" | tr '\n' ' ')"
tail -1 "$WORK/assemble.log"
ASSEMBLED="$(grep -c '^ASSEMBLE \(put\|reuse\)' "$WORK/assemble.log" || true)"
[ "${ASSEMBLED:-0}" -gt 0 ] || die "the assembly stored no blob at all"
ok "$ASSEMBLED blob rows now have their bytes in the real object store"

# --- 3. read-only export ---------------------------------------------------

step "export: a fingerprint of the source BEFORE (this is the read-only proof)"
"$PORTABILITY" fingerprint --project "$PROJECT" --out "$WORK/fp-before.json" \
  || die "fingerprint before the export failed"
ok "fingerprint written to $WORK/fp-before.json"
python3 - "$WORK/fp-before.json" <<'PY'
import json,sys
f=json.load(open(sys.argv[1]))
print("  --    row counts:", " ".join(f"{k}={v}" for k,v in sorted(f["row_counts"].items())))
print("  --    released state hash:", f["released_state_hash"])
print("  --    branch heads:", len(f["branch_heads"]), "| provider refs:", len(f["git_refs"]), "| blob hashes:", len(f["blob_content_hashes"]))
PY

step "export: the portable bundle"
"$PORTABILITY" export --project "$PROJECT" --out "$WORK/bundle-a" >"$WORK/export-a.log" 2>&1 \
  || die "export failed: $(tail -5 "$WORK/export-a.log" | tr '\n' ' ')"
grep '^EXPORT' "$WORK/export-a.log" | sed 's/^/  --    /'
DIGEST_A="$(sed -n 's/^EXPORT content_digest=//p' "$WORK/export-a.log")"
[ -n "$DIGEST_A" ] || die "the export reported no content digest"

step "export: it is read-only (fingerprint AFTER, compared byte for byte)"
"$PORTABILITY" fingerprint --project "$PROJECT" --out "$WORK/fp-after.json" \
  || die "fingerprint after the export failed"
if diff -u "$WORK/fp-before.json" "$WORK/fp-after.json" >"$WORK/fp.diff" 2>&1; then
  ok "the source is byte-identical before and after the export (row counts, branch heads, blob content-hash set, state hashes, provider refs)"
else
  bad "the source CHANGED across the export:"
  sed 's/^/        /' "$WORK/fp.diff" >&2
fi

step "repeatability: a second export of the same released project, compared class by class"
"$PORTABILITY" export --project "$PROJECT" --out "$WORK/bundle-b" >"$WORK/export-b.log" 2>&1 \
  || die "the second export failed: $(tail -5 "$WORK/export-b.log" | tr '\n' ' ')"
CMP_RC=0
"$PORTABILITY" compare --a "$WORK/bundle-a" --b "$WORK/bundle-b" >"$WORK/compare.log" 2>&1 || CMP_RC=$?
sed 's/^/  --    /' "$WORK/compare.log"
if [ "$CMP_RC" = "0" ]; then
  ok "two exports of the same released project carry the same content ($DIGEST_A)"
else
  bad "the two exports differ: $(tail -3 "$WORK/compare.log" | tr '\n' ' ')"
fi

step "check: the bundle verifies on its own"
"$PORTABILITY" check --bundle "$WORK/bundle-a" >"$WORK/check.log" 2>&1 \
  || die "the clean bundle does not verify: $(cat "$WORK/check.log")"
grep '^CHECK OK' "$WORK/check.log" | sed 's/^/  --    /'

# --- 4. the clean target environment ---------------------------------------
#
# Created BEFORE the tamper cases, and not for tidiness: the tamper cases
# assert that the IMPORT refuses, and an import that cannot reach a target
# database at all exits 1 ("could not run"), not 3 ("refused"). A gate that
# read that 1 as a refusal would be asserting nothing.

step "target: a clean environment — a database from the product's migrations, an empty organisation"
psql_admin "DROP DATABASE IF EXISTS $TGT_DB WITH (FORCE)" >/dev/null
psql_admin "CREATE DATABASE $TGT_DB" >/dev/null || die "could not create $TGT_DB"
"$WORK/bin/rddev" db migrate --url "$(db_url "$TGT_DB")" >"$WORK/migrate.log" 2>&1 \
  || die "migrations failed on the target: $(tail -5 "$WORK/migrate.log" | tr '\n' ' ')"
BEFORE_COUNT="$(psql "$(db_url "$TGT_DB")" -tAc 'SELECT count(*) FROM projects')"
[ "$BEFORE_COUNT" = "0" ] || die "the target database is not clean: it already holds $BEFORE_COUNT projects"
ok "$TGT_DB created from infra/migrations, projects=0"
curl -fsS -X DELETE -H "Authorization: token $TOKEN" "$GITEA_BASE/api/v1/orgs/$TARGET_ORG" >/dev/null 2>&1 || true
ok "the target organisation $TARGET_ORG is empty (any earlier one was removed)"

# --- 5. the instrument can say no ------------------------------------------

step "tamper: three kinds, each must be refused with the location named"
run_tamper_case() {
  local kind="$1" expect="$2"
  local tampered="$WORK/tampered-$kind"
  "$PORTABILITY" tamper --bundle "$WORK/bundle-a" --out "$tampered" --kind "$kind" >"$WORK/tamper-$kind.log" 2>&1 \
    || { bad "could not build the $kind tamper: $(cat "$WORK/tamper-$kind.log")"; return; }
  grep '^TAMPER' "$WORK/tamper-$kind.log" | sed 's/^/  --    /'
  local rc=0
  "$PORTABILITY" check --bundle "$tampered" >"$WORK/check-$kind.log" 2>&1 || rc=$?
  if [ "$rc" = "3" ]; then
    ok "check REFUSED the $kind tamper (exit 3): $(grep '^REFUSED' "$WORK/check-$kind.log")"
  else
    bad "check exited $rc on the $kind tamper (expected 3 = refused): $(cat "$WORK/check-$kind.log")"
  fi
  grep -q "$expect" "$WORK/check-$kind.log" \
    && ok "the refusal names the location ($expect)" \
    || bad "the refusal does not name $expect: $(cat "$WORK/check-$kind.log")"
  # The import side must refuse as well, and before it touches the target.
  rc=0
  "$PORTABILITY" import --bundle "$tampered" --repo-name "${SRC_REPO##*/}-tamper-$kind" --target-org "$TARGET_ORG" \
    >"$WORK/import-$kind.log" 2>&1 || rc=$?
  if [ "$rc" = "3" ]; then
    ok "import REFUSED the $kind tamper (exit 3) before writing anything: $(grep '^REFUSED' "$WORK/import-$kind.log")"
  else
    bad "import exited $rc on the $kind tamper (expected 3): $(tail -3 "$WORK/import-$kind.log" | tr '\n' ' ')"
  fi
  # "before writing anything" is a claim, so it is measured: the target must
  # still hold no project after a refused import.
  local left
  left="$(psql "$(db_url "$TGT_DB")" -tAc 'SELECT count(*) FROM projects')"
  [ "$left" = "0" ] && ok "the target is still empty after the $kind refusal (projects=0)" \
    || bad "the target holds $left project rows after the $kind tamper was refused"
  TAMPERS="$TAMPERS $kind"
}
TAMPERS=""
run_tamper_case blob "blobs/"
run_tamper_case manifest "manifest.json"
run_tamper_case commit "git.bundle"

step "import: rebuild PostgreSQL, the object store and Git in the clean environment"
"$PORTABILITY" import --bundle "$WORK/bundle-a" --target-org "$TARGET_ORG" >"$WORK/import.log" 2>&1
IMPORT_RC=$?
if [ "$IMPORT_RC" != "0" ]; then
  sed 's/^/        /' "$WORK/import.log" >&2
  die "the import failed (exit $IMPORT_RC)"
fi
grep -E '^IMPORT' "$WORK/import.log" | sed 's/^/  --    /'
# The measured values on BOTH sides, in full: the acceptance criterion asks
# for the four classes per side, and a gate log that printed only "AGREE"
# would be asking a reader to take the comparison on faith.
sed -n '/^IDENTIFIERS export side/,/^IDENTIFIERS AGREE/p' "$WORK/import.log" | sed 's/^/  --  /'
grep -q 'IDENTIFIERS AGREE' "$WORK/import.log" || die "the import finished without the identifiers agreeing"
ok "the import completed and all four identifier classes agree"
TGT_REPO="$(psql "$(db_url "$TGT_DB")" -tAc "SELECT git_repository_external_id FROM projects WHERE slug = '$PROJECT'" | tr -d ' ')"
[ -n "$TGT_REPO" ] || die "the imported project row carries no repository"
note "the imported project is provisioned against $TGT_REPO"

step "verify: an independent re-run against the target, reading the target's own rows"
"$PORTABILITY" verify --bundle "$WORK/bundle-a" >"$WORK/verify.log" 2>&1 \
  || die "verify failed: $(tail -5 "$WORK/verify.log" | tr '\n' ' ')"
sed -n '/^IDENTIFIERS export side/,/^IDENTIFIERS AGREE/p' "$WORK/verify.log" | sed 's/^/  --  /'
grep -q 'IDENTIFIERS AGREE' "$WORK/verify.log" || die "verify did not report agreement"

step "idempotence: a second import onto the same target must be refused, not merged"
rc=0
"$PORTABILITY" import --bundle "$WORK/bundle-a" --target-org "$TARGET_ORG" >"$WORK/import-again.log" 2>&1 || rc=$?
if [ "$rc" = "3" ]; then
  ok "the second import was refused (exit 3): $(grep '^REFUSED' "$WORK/import-again.log")"
else
  bad "the second import exited $rc — an import onto an occupied target must refuse (exit 3)"
fi
AFTER_COUNT="$(psql "$(db_url "$TGT_DB")" -tAc 'SELECT count(*) FROM projects')"
[ "$AFTER_COUNT" = "1" ] && ok "the target still holds exactly one project row" || bad "the target holds $AFTER_COUNT project rows"

step "the target's stores really hold the project (not just its rows)"
TGT_STATES="$(psql "$(db_url "$TGT_DB")" -tAc 'SELECT count(*) FROM project_states')"
TGT_OBJECTS="$(psql "$(db_url "$TGT_DB")" -tAc 'SELECT count(*) FROM scientific_object_versions')"
TGT_BLOBS="$(psql "$(db_url "$TGT_DB")" -tAc 'SELECT count(*) FROM blobs')"
ok "target PostgreSQL: project_states=$TGT_STATES scientific_object_versions=$TGT_OBJECTS blobs=$TGT_BLOBS"
TGT_REF="$(GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraHeader GIT_CONFIG_VALUE_0="Authorization: token $TOKEN" \
  git ls-remote "$GITEA_BASE/$TGT_REPO.git" "refs/heads/$FIXTURE_BRANCH" | awk '{print $1}')"
[ "$TGT_REF" = "$FIXTURE_SHA" ] \
  && ok "the target repository's $FIXTURE_BRANCH carries the same commit as the source (${FIXTURE_SHA:0:12}…)" \
  || bad "the target's $FIXTURE_BRANCH is ${TGT_REF:-<missing>}, the source's is $FIXTURE_SHA"

# --- 6. the mutation check -------------------------------------------------

step "mutation check: with the refusal disabled, does the tamper still go red?"
bash "$ROOT/tests/acceptance/project-export-portability-mutation-check.sh" \
  --bundle "$WORK/bundle-a" --work "$WORK/mutant" --mut-db "$MUT_DB" --target-org "$TARGET_ORG" \
  >"$WORK/mutation.log" 2>&1
MUT_RC=$?
sed 's/^/  /' "$WORK/mutation.log"
if [ "$MUT_RC" = "0" ]; then
  ok "the mutation check held: the refusals are load-bearing"
else
  bad "the mutation check failed (exit $MUT_RC)"
fi

# --- verdict ---------------------------------------------------------------

if [ "$FAILED" = "0" ]; then
  printf '\nG3 %s: PASSED\n' "$GATE"
  exit 0
fi
printf '\nG3 %s: FAILED — see the FAIL lines above; run directories are removed on exit and the seed log is %s\n' "$GATE" "$WORK/seed.log" >&2
exit 1
