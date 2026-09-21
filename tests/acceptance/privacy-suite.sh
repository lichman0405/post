#!/usr/bin/env bash
# The required test "privacy suite" (T1107) — one command from the repo
# root, grouped by nature, with a case count per group.
#
# WHAT IT IS FOR. The repository already had dense permission negatives and
# no single place that ran them. This script is that place plus the two
# blocks T1107 adds, and it is deliberately a RUNNER: it names existing
# suites rather than restating them, so a rule that changes is caught by the
# suite that owns it and not by a copy here.
#
# THE GROUPS:
#
#   edge      tests/e2e — the composed HTTP surface over in-memory stores,
#             no database. The cheapest half of every negative, and the one
#             that runs on a laptop with no PostgreSQL.
#   postgres  tests/integration — the same negatives against REAL
#             PostgreSQL, through the real stores and the real SQL, plus the
#             four blocks T1107 adds. This is the group that can see a
#             filter that lives in the wrong layer.
#   browser   tests/e2e-<name>/run.sh — the five unwired browser suites, in
#             real Chromium against the built web app. Nothing in the
#             Makefile or in CI names any of them; this is the only target
#             that runs them, which is why it fails loudly rather than
#             quietly when one of them is red (see the note below).
#   controls  the mutation checks — the product code broken on purpose and
#             restored byte-for-byte, so that "the suite passes" cannot mean
#             "the suite agreed with the code".
#
# LOUD, NEVER SILENT. Every prerequisite is checked before the group that
# needs it, and a missing database, a missing node or a suite that reports
# zero cases is a failure here rather than a skip. A group that could not
# run is never reported as one that passed.
#
# Usage: bash tests/acceptance/privacy-suite.sh
# Env:   POSTGRES_TEST_ADMIN_URL overrides the database the postgres group uses.
#        PRIVACY_SUITE_GROUPS selects groups (default: all four), e.g.
#          PRIVACY_SUITE_GROUPS=edge,postgres bash tests/acceptance/privacy-suite.sh
set -uo pipefail

ACCEPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$ACCEPT_DIR/../.." && pwd)"
cd "$ROOT"

PG_TEST_URL="${POSTGRES_TEST_ADMIN_URL:-postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post}"
export POSTGRES_TEST_ADMIN_URL="$PG_TEST_URL"
SUITE_GROUPS="${PRIVACY_SUITE_GROUPS:-edge,postgres,browser,controls}"

want_group() { case ",$SUITE_GROUPS," in *",$1,"*) return 0 ;; *) return 1 ;; esac; }

FAILED_GROUPS=()
SUMMARY=()

group_failed() {
  FAILED_GROUPS+=("$1")
  SUMMARY+=("FAIL  $1 — $2")
  echo ""
  echo "privacy-suite: group $1 FAILED — $2" >&2
}

group_passed() {
  SUMMARY+=("ok    $1 — $2")
  echo "privacy-suite: group $1 ok — $2"
}

# --------------------------------------------------------------------------
# edge — tests/e2e, no database

# The acceptance criteria of this task name the existing negatives one by one
# (`tests/e2e/privacy_e2e_test.go:140`, `tests/e2e/asset_page_e2e_test.go:886`)
# and require that this runner covers them. Every name below is one of those,
# and the guard counts them so a name that stops matching is a red group and
# not a quietly smaller run.
EDGE_TESTS=(
  TestE2EPrivacyNegative
  TestE2EAssetPageAnswersOneIndistinguishable404
  TestE2EAnonymousReleaseReads
  TestE2EAnonymousReleaseHidesThePrivateProject
  TestE2EAssetPageAnonymous
  TestE2EExploreIsReadOnlyAndAnonymous
  TestE2EExploreNeverNamesAPrivateProject
)

run_edge() {
  echo ""
  echo "=================================================================="
  echo "== privacy-suite: group edge (tests/e2e, no database)"
  echo "=================================================================="
  local re
  re="$(printf '%s|' "${EDGE_TESTS[@]}")"
  re="${re%|}"
  local out status cases
  out="$(go test ./tests/e2e -run "$re" -count=1 -v -timeout 10m 2>&1)"
  status=$?
  echo "$out"
  cases="$(grep -c '^--- PASS:' <<<"$out")"
  if [ "$status" -ne 0 ]; then
    group_failed edge "go test ./tests/e2e exited $status"
    return
  fi
  if [ "$cases" -lt "${#EDGE_TESTS[@]}" ]; then
    group_failed edge "only $cases of ${#EDGE_TESTS[@]} cases ran — a group that measured less than it names is a shell, not a gate"
    return
  fi
  group_passed edge "$cases cases (${#EDGE_TESTS[@]} named)"
}

# --------------------------------------------------------------------------
# postgres — tests/integration against a real database

# The privacy and permission negatives this repository already had, by name,
# plus the four blocks T1107 adds. Every entry is load-bearing: this list is
# the manifest of what the group covers, and the check below refuses to let
# it shrink silently.
POSTGRES_TESTS=(
  # the composed surfaces and the store-level filters
  TestPrivacyReadIsolation
  TestProjectAuthzMatrix
  TestOrgPermissionIntegration
  TestAssetPageOfAPrivateProjectIsTheSameNotFound
  TestAssetPreviewDoesNotDiscloseAForeignPrivateEntity
  TestAssetGovernanceAnonymousClassIsDeniedByTheMatrix
  # the feeds: what a private project's rows do to a public document
  TestPrivateRowsAreNeverRenderedFromThePublicFeed
  TestPrivateVersionsDoNotSuppressPublicOnes
  TestMembersOnlyPublicationsAreNotInAnyAnonymousFeed
  TestPrivateTargetsHaveNoFeedForAnyone
  # publishing is not publishing-to-the-network
  TestKnowledgePublishDoesNotWidenVisibility
  TestKnowledgePublishPermissionIsFailClosed
  TestKnowledgeReadAnswersAnUnreadablePublicationExactlyAsAnUnknownPID
  TestAssetPublishDenialPrecedesTheTargetLookup
  # search, retrieval and ranking: the scope predicate
  TestSearchScopeIsResolvedFromMembershipAndEnforcedByTheQuery
  TestSearchProjectionKeepsPrivateContentPrivate
  TestSearchDocumentsEnforcesAccessControl
  TestRetrievalRecallsSeedsAndExpandsWithoutPrivateLeak
  TestRetrievalQueriesFailClosedOnTheirOwn
  # The counter-proof the task singles out: it runs the query WITHOUT the
  # project predicate and shows the other project's version id comes back, so
  # a filter that is removed whole cannot pass this file silently.
  TestTraversalQueriesAreWhatRefuseTheOtherProject
  TestRetrieverRefusesASeedFromAnotherProject
  TestRankingFactsAreScopedAndFailClosed
  # the rest of the read paths a private entity can leak through
  TestProvenanceVisibility
  TestRSGQueryAuthorizationAndNoLeak
  TestInboxIsOwnerScopedAndHidesWhatItMayNotName
  TestActivityFeedAudienceFailsClosedWithoutAReader
  TestFanOutFailClosedOnUnresolvableAudience
  TestDependencyImpactAssetLandingPointAndPrivacy
  TestExternalEvidenceNetworkIsClassifiedAndDoesNotLeak
  TestMergeWithholdsPrivateSourceFromPublicTarget
  TestAttestationPrivacyE2E
  TestEvidenceReadAudienceKeepsTheGateAnswer
  TestOrganizationAudienceIgnoresTheSessionTimezone
  TestValidateEndpointVisibilityOverRealServices
  TestWebhookFanOutVisibilityAndFilters
  TestResponsibilityNeverWidensAuthz
  TestEmailDigestCarriesNoUnauthorizedPrivateContent
  # T1107 — the two blanks this task fills, and the two surfaces it pins
  TestPrivateUsageAndVersionsAreUnobservableOnEveryPublicSurface
  TestProjectAndAssetRefusalsAreIndistinguishable
  TestRetrievalCannotTellAnUnreadableDocumentFromNonexistent
  TestPrivatePublicationsDoNotCrowdTheExploreIndex
)

run_postgres() {
  echo ""
  echo "=================================================================="
  echo "== privacy-suite: group postgres (tests/integration, real database)"
  echo "== $PG_TEST_URL"
  echo "=================================================================="
  if ! python3 scripts/pg-ready.py "$PG_TEST_URL" 2>&1 | tail -2; then
    group_failed postgres "no PostgreSQL reachable at $PG_TEST_URL — this group has no in-memory substitute, and a skipped group is not a passed group"
    return
  fi
  local re
  re="$(printf '%s|' "${POSTGRES_TESTS[@]}")"
  re="${re%|}"
  local out status cases
  out="$(go test ./tests/integration -run "$re" -count=1 -v -timeout 20m 2>&1)"
  status=$?
  echo "$out"
  cases="$(grep -c '^--- PASS:' <<<"$out")"
  if [ "$status" -ne 0 ]; then
    group_failed postgres "go test ./tests/integration exited $status"
    return
  fi
  if [ "$cases" -lt "${#POSTGRES_TESTS[@]}" ]; then
    group_failed postgres "only $cases of ${#POSTGRES_TESTS[@]} named cases ran — a name that matches nothing is a hole in the manifest"
    return
  fi
  group_passed postgres "$cases cases (${#POSTGRES_TESTS[@]} named)"
}

# --------------------------------------------------------------------------
# browser — the five unwired Chromium suites

BROWSER_SUITES=(anonymous assets explore settings shell)

run_browser() {
  echo ""
  echo "=================================================================="
  echo "== privacy-suite: group browser (real Chromium, built web app)"
  echo "=================================================================="
  if ! command -v node >/dev/null 2>&1; then
    group_failed browser "node is not on PATH; the browser suites cannot run and a skip is not a pass"
    return
  fi
  if [ ! -d apps/web/node_modules ]; then
    group_failed browser "apps/web/node_modules is missing — run pnpm install first"
    return
  fi
  local ok_count=0 failed=()
  for suite in "${BROWSER_SUITES[@]}"; do
    local script="tests/e2e-$suite/run.sh"
    if [ ! -f "$script" ]; then
      failed+=("$suite (no $script)")
      continue
    fi
    echo ""
    echo "---- e2e-$suite ----"
    if bash "$script" 2>&1 | tail -40; then
      ok_count=$((ok_count + 1))
    else
      failed+=("$suite")
    fi
  done
  if [ "${#failed[@]}" -ne 0 ]; then
    group_failed browser "suites failed: ${failed[*]}"
    return
  fi
  group_passed browser "$ok_count of ${#BROWSER_SUITES[@]} suites"
}

# --------------------------------------------------------------------------
# controls — the mutation checks

CONTROL_SCRIPTS=(
  tests/e2e/anonymous-mutation-check.sh
  tests/acceptance/privacy-mutation-check.sh
)

run_controls() {
  echo ""
  echo "=================================================================="
  echo "== privacy-suite: group controls (product code broken on purpose, restored byte-for-byte)"
  echo "=================================================================="
  local ok_count=0 failed=()
  for script in "${CONTROL_SCRIPTS[@]}"; do
    if [ ! -f "$script" ]; then
      failed+=("$script (missing)")
      continue
    fi
    echo ""
    echo "---- $script ----"
    if bash "$script" 2>&1 | tail -20; then
      ok_count=$((ok_count + 1))
    else
      failed+=("$script")
    fi
  done
  if [ "${#failed[@]}" -ne 0 ]; then
    group_failed controls "checks failed: ${failed[*]}"
    return
  fi
  group_passed controls "$ok_count of ${#CONTROL_SCRIPTS[@]} mutation checks"
}

# --------------------------------------------------------------------------

echo "privacy-suite: groups = $SUITE_GROUPS"

want_group edge && run_edge
want_group postgres && run_postgres
want_group browser && run_browser
want_group controls && run_controls

echo ""
echo "=================================================================="
echo "== privacy-suite: summary"
echo "=================================================================="
for line in "${SUMMARY[@]}"; do
  echo "  $line"
done

if [ "${#FAILED_GROUPS[@]}" -ne 0 ]; then
  echo ""
  echo "privacy-suite: FAILED — groups: ${FAILED_GROUPS[*]}" >&2
  exit 1
fi
echo ""
echo "privacy-suite: all groups passed"
