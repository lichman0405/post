#!/usr/bin/env bash
#
# Regenerate internal/persistence/sqlc from its inputs (task T0005's convention,
# sqlc.yaml's documented command).
#
# Why a script rather than `sqlc generate` spelled out at each call site: the
# output is CHECKED IN, so the regeneration has to be the same operation
# everywhere it happens — a Worker keeping the checked-in code consistent with
# its own query edit, and `rddev rebaseline` recomputing it from a merged tree
# rather than merging two copies as text. A call site that forgets the version
# check below writes files the drift check will bless, because the drift check
# regenerates with whatever binary it finds on PATH too.
#
# Usage: scripts/gen_sqlc.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# The version sqlc.yaml pins. Generation with a DIFFERENT version does not
# fail — it writes its own formatting over the checked-in file, and every
# check downstream compares against a regeneration by the same binary, so the
# two agree and the mistake is invisible until a different machine runs CI.
WANT_VERSION="v1.31.1"

SQLC_BIN="${SQLC_BIN:-$(command -v sqlc || true)}"
if [[ -z "$SQLC_BIN" ]]; then
  echo "error: sqlc not found on PATH (install: go install github.com/sqlc-dev/sqlc/cmd/sqlc@${WANT_VERSION})" >&2
  exit 1
fi

have="$("$SQLC_BIN" version 2>&1 | tr -d '[:space:]')"
if [[ "$have" != "$WANT_VERSION" ]]; then
  echo "error: sqlc ${WANT_VERSION} is pinned in sqlc.yaml, found ${have} — generating with it would rewrite the checked-in code and every check would agree with the rewrite" >&2
  exit 1
fi

exec "$SQLC_BIN" generate
