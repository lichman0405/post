#!/usr/bin/env bash
#
# vuln-node: the Node dependency audit row, with one owner-approved exception.
#
# The tree depends on deepmerge 4.3.1 through @primer/react. GHSA-gcqx-w48m-w2h4
# is a low-severity prototype-poisoning advisory with NO patched version; the
# owner approved a scoped ignore on 2026-10-28. This wrapper keeps the gate's
# evidence line canonical while refusing any *other* ignored advisory:
#
#   - a clean audit -> "No known vulnerabilities found"
#   - exactly "1 ignored: 1 low" and pnpm's reviewed-ignored message -> pass
#   - anything else (a new vulnerability, a second ignored advisory, a
#     different severity) -> red
set -uo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

out="$(pnpm audit --audit-level=low 2>&1)"
rc=$?
if printf '%s\n' "$out" | grep -q '^No known vulnerabilities found'; then
  printf '%s\n' "$out"
  echo 'No known vulnerabilities found'
  exit 0
fi
if [[ $rc -eq 0 ]] \
   && printf '%s\n' "$out" | grep -qx 'All found vulnerabilities were already reviewed and decided to be ignored' \
   && printf '%s\n' "$out" | grep -qx '1 ignored: 1 low'; then
  printf '%s\n' "$out"
  echo 'No known vulnerabilities found (1 owner-approved ignored advisory: GHSA-gcqx-w48m-w2h4)'
  exit 0
fi
printf '%s\n' "$out" >&2
echo 'vuln-node: the audit is neither clean nor exactly the approved one-low-ignored case' >&2
exit 1
