#!/usr/bin/env bash
#
# G3 — the Git infrastructure the P3 adapter will build on (docs/67, T0301+).
#
# P3's whole premise is that an internal Gitea (ADR-019) can host repository
# provisioning, two-layer main protection and push webhooks. Those are
# properties OF THE RUNNING INSTANCE, not of our code: a version that ignores
# `branch_protection` on this deployment, or a webhook payload shape that
# differs from the documented one, invalidates the adapter before a line of it
# is written. So this exercises the real instance, and fails loudly when the
# instance is not there rather than skipping — a G3 that quietly becomes a
# no-op reports green for a system nobody checked.
#
# Requires: the infra stack (make infra-up && make infra-init, with
# GITEA_SVC_MINT_TOKEN=1 once to mint the service token) and POST_GITEA_TOKEN.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

# Credentials come from the same place `make dev` reads them: .env.dev (local,
# gitignored). The gate step inherits the Supervisor's environment, and a token
# that exists only in one shell is exactly the kind of hidden prerequisite that
# makes a gate pass on the machine that configured it.
BASE="${POST_GITEA_BASE_URL:-http://127.0.0.1:3000}"
TOKEN="${POST_GITEA_TOKEN:-}"
if [[ -z "$TOKEN" && -f "$ROOT/.env.dev" ]]; then
  # shellcheck disable=SC1091
  TOKEN="$(set -a; . "$ROOT/.env.dev" >/dev/null 2>&1; printf '%s' "${POST_GITEA_TOKEN:-}")"
  BASE="${POST_GITEA_BASE_URL:-$BASE}"
fi

WORK="$(mktemp -d)"
HOOK_PID=""
cleanup() {
  [[ -n "$HOOK_PID" ]] && kill "$HOOK_PID" 2>/dev/null
  if [[ -n "$TOKEN" && -n "${REPO:-}" ]]; then
    curl -sS -X DELETE -H "Authorization: token $TOKEN" "$BASE/api/v1/repos/$REPO" >/dev/null 2>&1
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

api() { # api METHOD PATH [BODY]
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -sS -X "$method" -H "Authorization: token $TOKEN" \
      -H 'Content-Type: application/json' -d "$body" "$BASE$path"
  else
    curl -sS -X "$method" -H "Authorization: token $TOKEN" "$BASE$path"
  fi
}
code() { # code METHOD PATH [BODY]
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -sS -o /dev/null -w '%{http_code}' -X "$method" -H "Authorization: token $TOKEN" \
      -H 'Content-Type: application/json' -d "$body" "$BASE$path"
  else
    curl -sS -o /dev/null -w '%{http_code}' -X "$method" -H "Authorization: token $TOKEN" "$BASE$path"
  fi
}

if [[ -z "$TOKEN" ]]; then
  echo "G3 gitea-real-services: FAILED — POST_GITEA_TOKEN is not set." >&2
  echo "G3 requires the real instance and does NOT skip. Start it and mint a token:" >&2
  echo "  make infra-up && GITEA_SVC_MINT_TOKEN=1 make infra-init" >&2
  echo "then put the minted token in .env.dev as POST_GITEA_TOKEN=<token>" >&2
  exit 1
fi
if [[ "$(code GET /api/v1/version)" != "200" ]]; then
  echo "G3 gitea-real-services: FAILED — no Gitea API at $BASE (make infra-up)" >&2
  exit 1
fi
ok "real Gitea reachable at $BASE and the token authenticates"

# --- repository provisioning -------------------------------------------------
OWNER="$(api GET /api/v1/user | python3 -c 'import json,sys;print(json.load(sys.stdin)["login"])')"
REPO="$OWNER/g3-$$-$(date +%s)"
if [[ "$(code POST /api/v1/user/repos "{\"name\":\"${REPO##*/}\",\"auto_init\":false,\"private\":true}")" == "201" ]]; then
  ok "created a private repository ($REPO)"
else
  fail "could not create a repository through the API"
  printf '\nG3 gitea-real-services: %d failure(s)\n' "$FAILS"; exit 1
fi

# A real push, with the token as the credential — the path the adapter uses.
git init -q "$WORK/work" && cd "$WORK/work"
git -c init.defaultBranch=main init -q >/dev/null 2>&1 || true
echo "g3" > README.md && git add -A
git -c user.name=g3 -c user.email=g3@test commit -q -m "g3 probe"
if git -c user.name=g3 -c user.email=g3@test push -q \
     "http://post-git-svc:$TOKEN@${BASE#http://}/$REPO.git" HEAD:main 2>"$WORK/push.err"; then
  ok "pushed to main over HTTP with the service token"
else
  fail "push failed: $(tail -2 "$WORK/push.err")"
fi
cd "$ROOT"

# --- two-layer main protection (T0302's premise) ------------------------------
PROT='{"branch_name":"main","enable_push":false,"required_approvals":1,"enable_status_check":false}'
if [[ "$(code POST "/api/v1/repos/$REPO/branch_protections" "$PROT")" == "201" ]]; then
  ok "branch protection accepted on main"
else
  fail "the instance refused to create branch protection on main"
fi
GOT="$(api GET "/api/v1/repos/$REPO/branch_protections/main")"
if python3 - "$GOT" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
assert d.get("branch_name") == "main", d
# enable_push=false is the property the adapter depends on: main is not
# writable through the branch, which is what makes "frozen main" enforceable.
assert d.get("enable_push") is False, f"enable_push is {d.get('enable_push')!r}, not False"
PY
then ok "protection reads back with enable_push=false (frozen main is enforceable)"
else fail "protection did not read back as written: $GOT"; fi

# A direct push to a protected main must be refused — the property, not the setting.
echo "should not land" >> README.md
git -c user.name=g3 -c user.email=g3@test commit -q -am "g3 should be refused"
if git -c user.name=g3 -c user.email=g3@test push -q \
     "http://post-git-svc:$TOKEN@${BASE#http://}/$REPO.git" HEAD:main >/dev/null 2>&1; then
  fail "a direct push to a protected main SUCCEEDED — protection is configured but not enforced"
else
  ok "a direct push to protected main is refused"
fi

# --- push webhook (T0305's premise) ------------------------------------------
python3 - "$WORK/deliveries.jsonl" <<'PY' &
import http.server, json, sys
out = open(sys.argv[1], "w")
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n)
        out.write(json.dumps({
            "event": self.headers.get("X-Gitea-Event"),
            "body": json.loads(body.decode() or "{}"),
        }) + "\n"); out.flush()
        self.send_response(204); self.end_headers()
    def log_message(self, *a): pass
http.server.HTTPServer(("0.0.0.0", 18099), H).serve_forever()
PY
HOOK_PID=$!
for _ in $(seq 1 40); do (exec 3<>"/dev/tcp/127.0.0.1/18099") 2>/dev/null && break; sleep 0.25; done

# Gitea runs in a container: 127.0.0.1 there is the container's loopback, not
# this listener's. Address the host the way the container sees it.
# The gateway of the network the Gitea container is actually on: the compose
# network is not `bridge`, so guessing the default bridge reaches nothing and
# Gitea reports "connection refused" for a URL that looks perfectly fine.
GITEA_CONTAINER="${POST_G3_GITEA_CONTAINER:-post-gitea-1}"
gitea_network() {
  docker inspect "$GITEA_CONTAINER" \
    --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>/dev/null | awk '{print $1}'
}
HOOK_HOST="${POST_G3_HOOK_HOST:-}"
if [[ -z "$HOOK_HOST" ]]; then
  NET="$(gitea_network)"
  [[ -n "$NET" ]] && HOOK_HOST="$(docker network inspect "$NET" \
    --format '{{(index .IPAM.Config 0).Gateway}}' 2>/dev/null)"
fi
if [[ -z "$HOOK_HOST" ]]; then
  fail "could not determine an address the Gitea container can reach this listener on"
fi

HOOK="{\"type\":\"gitea\",\"active\":true,\"events\":[\"push\"],\"config\":{\"url\":\"http://$HOOK_HOST:18099/hook\",\"content_type\":\"json\"}}"
if [[ "$(code POST "/api/v1/repos/$REPO/hooks" "$HOOK")" == "201" ]]; then
  ok "registered a push webhook"
else
  fail "could not register a push webhook"
fi

# Trigger a delivery with a push to a NON-protected branch.
git -c user.name=g3 -c user.email=g3@test push -q \
  "http://post-git-svc:$TOKEN@${BASE#http://}/$REPO.git" HEAD:refs/heads/g3-probe >/dev/null 2>&1 || true
delivered=0
for _ in $(seq 1 40); do
  if [[ -s "$WORK/deliveries.jsonl" ]]; then delivered=1; break; fi
  sleep 0.5
done
if (( delivered )); then
  python3 - "$WORK/deliveries.jsonl" <<'PY'
import json, sys
d = json.loads(open(sys.argv[1]).readline())
assert d["event"] == "push", d
b = d["body"]
for key in ("ref", "repository", "commits", "pusher"):
    assert key in b, f"a real push payload has no {key!r}: {sorted(b)}"
PY
  ok "a real push delivered a webhook with ref/repository/commits/pusher"
else
  # Ask the instance why, rather than reporting only that nothing arrived.
  HOOK_ID="$(api GET "/api/v1/repos/$REPO/hooks" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d[0]["id"] if d else "")' 2>/dev/null)"
  DETAIL="$(api GET "/api/v1/repos/$REPO/hooks/$HOOK_ID/deliveries" 2>/dev/null | head -c 400)"
  fail "no webhook delivery arrived — the payload shape the ingestion path depends on is unverified. Gitea reports: ${DETAIL:-<no deliveries recorded>}"
fi

cd "$ROOT"
printf '\n'
if (( FAILS )); then
  printf 'G3 gitea-real-services: %d failure(s)\n' "$FAILS"
  exit 1
fi
printf 'G3 gitea-real-services: all checks passed against a real Gitea\n'
