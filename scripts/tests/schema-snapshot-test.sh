#!/usr/bin/env bash
#
# Unit tests for scripts/gen_schema_snapshot.py (fixture-driven).
#
# The mechanism exists because the canonical artifact silently lagged five
# migrations behind the schema while everything still trusted it. A drift check
# that cannot report drift would repeat that, so the stale case is what this
# proves.
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
GEN="$ROOT/scripts/gen_schema_snapshot.py"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

FIX="$WORK/repo"
mkdir -p "$FIX/infra/migrations" "$FIX/specs/database"
cat > "$FIX/infra/migrations/00001_a.sql" <<'SQL'
-- +goose Up
CREATE TABLE a (id int);
-- +goose Down
DROP TABLE a;
SQL
cat > "$FIX/infra/migrations/00002_b.sql" <<'SQL'
-- +goose Up
ALTER TABLE a ADD COLUMN b text;
-- +goose Down
ALTER TABLE a DROP COLUMN b;
SQL

# 1. Generation includes every Up section and no Down section.
if ! python3 "$GEN" --root "$FIX" >/dev/null; then
  fail "generation failed"
else
  ok "generation succeeded"
fi
SNAP="$FIX/specs/database/postgres.sql"
grep -q 'CREATE TABLE a' "$SNAP" && grep -q 'ADD COLUMN b' "$SNAP" \
  && ok "the snapshot carries every migration's Up section in order" \
  || fail "the snapshot is missing a migration's content"
if grep -q 'DROP TABLE a' "$SNAP" || grep -q 'DROP COLUMN b' "$SNAP"; then
  fail "a Down section reached the snapshot — it describes the schema, not the reversals"
else
  ok "no Down section reached the snapshot"
fi

# 2. A current snapshot passes --check.
python3 "$GEN" --root "$FIX" --check >/dev/null 2>&1 \
  && ok "--check passes on a current snapshot" \
  || fail "--check failed on a snapshot it had just written"

# 3. Regeneration is byte-identical (deterministic).
before="$(sha256sum "$SNAP" | cut -d' ' -f1)"
python3 "$GEN" --root "$FIX" >/dev/null
after="$(sha256sum "$SNAP" | cut -d' ' -f1)"
[ "$before" = "$after" ] && ok "regeneration is byte-identical" \
  || fail "regeneration is not deterministic"

# 4. THE case the mechanism exists for: the schema moves, the snapshot does not.
cat >> "$FIX/infra/migrations/00003_c.sql" <<'SQL'
-- +goose Up
CREATE TABLE c (id int);
SQL
python3 "$GEN" --root "$FIX" --check >/dev/null 2>&1 \
  && fail "--check accepted a snapshot that lags a migration — this is the defect it exists to catch" \
  || ok "--check refuses a stale snapshot"

# 5. An unreadable tree is an error, not a silent pass.
python3 "$GEN" --root "$WORK/absent" --check >/dev/null 2>&1 \
  && fail "--check passed with no migrations to check" \
  || ok "--check fails when there are no migrations"

printf '\n'
if (( FAILS )); then
  printf 'schema-snapshot-test: %d failure(s)\n' "$FAILS"
  exit 1
fi
printf 'schema-snapshot-test: all cases passed\n'
