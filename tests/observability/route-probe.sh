#!/usr/bin/env bash
# Both routes of the composed root mux, from ONE running process.
#
# WHY THIS EXISTS (T1109, on the d21ad3c baseline)
#
# cmd/api/main.go has exactly one region that two changes touched: T0906's
# search wiring (retrievalStore ... searchAPI.Register(v1)) sits immediately
# above T1109's two lines (mux.Handle("/api/v1/", authAPI.Guard(
# observability.RecordRoute(v1))) and the registerAPIMetrics block). When main
# moved under this task the region was merged by hand, and a hand merge is
# exactly the edit that can silently drop one side: both sides still COMPILE
# and the binary still starts either way. So the check is request-level, on a
# running process, not a reading of the source.
#
# It is the PAIR that makes it evidence. GET is a READ, so the guard passes it
# through to the inner mux (auth_middleware.go: stateChangingMethods covers
# POST/PUT/PATCH/DELETE only), and net/http's ServeMux then answers
#
#   405 Method Not Allowed   a pattern matches the PATH but not the method
#   404 Not Found            nothing matches the path at all
#
# So GET /api/v1/search answering 405 with `Allow: POST` says "T0906's
# registration is reachable from the composed root mux", and the same request
# against a path nobody registers answering 404 is the control that says 405
# is not simply what this mux answers to everything. Remove searchAPI.Register
# from the composition and the first answer becomes a 404.
#
# The anonymous POST is here because it is the reason GET is the discriminating
# method: an anonymous write is refused 401 BEFORE routing (auth_middleware.go:
# "an unimplemented product endpoint answers 401, not 404, when the caller is
# anonymous"), so a POST answers the same thing whether or not the route
# exists and proves nothing about the composition. It is checked so that a
# reader cannot mistake it for evidence of routing.
#
# Usage: route-probe.sh BASE_URL      e.g.  route-probe.sh http://127.0.0.1:8080
# Exits non-zero on the first violated expectation, printing what it saw.
set -euo pipefail

BASE="${1:?usage: route-probe.sh BASE_URL (a running cmd/api, e.g. http://127.0.0.1:8080)}"
BASE="${BASE%/}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# T1109's route: the exposition is served, and it is this process serving it.
# ---------------------------------------------------------------------------
code=$(curl -sS -m 10 -o "$TMP/metrics.out" -w '%{http_code}' "$BASE/metrics") \
  || fail "GET $BASE/metrics could not be reached at all"
[ "$code" = "200" ] || fail "GET $BASE/metrics answered $code, want 200 — the endpoint this task delivers is not on the composed process"
families=$(grep -c '^post_' "$TMP/metrics.out" || true)
[ "$families" -gt 0 ] || fail "GET $BASE/metrics answered 200 with no post_* sample line: an empty exposition proves the route exists but not that the collectors are registered"
printf '   GET  /metrics                        -> %s, %s post_* sample lines\n' "$code" "$families"
grep -E '^post_(db_up|http_requests_total|permission_denials_total)' "$TMP/metrics.out" | head -3 | sed 's/^/        /'

# ---------------------------------------------------------------------------
# T0906's route, reached through the guard: the discriminating request.
# ---------------------------------------------------------------------------
code=$(curl -sS -m 10 -D "$TMP/search.headers" -o "$TMP/search.out" -w '%{http_code}' "$BASE/api/v1/search") \
  || fail "GET $BASE/api/v1/search could not be reached at all"
allow=$(tr -d '\r' <"$TMP/search.headers" | sed -n 's/^[Aa]llow: *//p')
printf '   GET  /api/v1/search                  -> %s  Allow: %s\n' "$code" "${allow:-<no Allow header>}"
[ "$code" = "405" ] || fail "GET $BASE/api/v1/search answered $code, want 405: a 404 means T0906's search route is not registered on the /api/v1 mux the composition root mounts, and 200 would mean the guard let an unauthenticated read run the search"
case "$allow" in
  *POST*) ;;
  *) fail "the 405 carries 'Allow: ${allow:-<none>}': the pattern that matched the path is not POST /api/v1/search" ;;
esac

code=$(curl -sS -m 10 -o "$TMP/post.out" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' -d '{}' "$BASE/api/v1/search") \
  || fail "POST $BASE/api/v1/search could not be reached at all"
printf '   POST /api/v1/search (anonymous)      -> %s\n' "$code"
[ "$code" = "401" ] || fail "an anonymous POST answered $code, want 401: the guard refuses anonymous writes before routing, so this answer is about the guard and not about the route (it is checked here to keep it from being read as routing evidence)"

# ---------------------------------------------------------------------------
# The control: a path nobody registers. Without it, "405" could be this mux's
# answer to anything, and the probe would pass on a tree with no search route.
# ---------------------------------------------------------------------------
NEG="/api/v1/route-probe-no-such-route"
code=$(curl -sS -m 10 -o "$TMP/neg.out" -w '%{http_code}' "$BASE$NEG") \
  || fail "GET $BASE$NEG could not be reached at all"
printf '   GET  %s -> %s  (control)\n' "$NEG" "$code"
[ "$code" = "404" ] || fail "GET $BASE$NEG answered $code, want 404: the control failed, so the 405 above is not evidence that a pattern matched /api/v1/search"

echo "   OK: one process serves /metrics and reaches T0906's POST /api/v1/search"
