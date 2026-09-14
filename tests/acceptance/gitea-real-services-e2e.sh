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

# Git's environment variables outrank the working directory, and they are
# inherited: rddev runs a gate with the ambient environment and cwd = the tree
# under test. With GIT_DIR set, `git init "$WORK/work"` exits 0 having created
# nothing, the next `git add -A` and `git commit` address the repository it
# names, and the probe commit lands in the tree this script is grading — the
# L1-20260913-16 harm, reached before any guard below can refuse it. A commit
# would already sit on the Worker's branch by the time the end-of-run check
# noticed. Every git command here addresses its repository by path (-C, a cwd,
# an explicit init target); none of them may be redirected from outside.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
      GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_NAMESPACE GIT_COMMON_DIR

# The tree this script runs in is under test-adjacent conditions, not a scratch
# space: the G3 gate runs it from inside a task worktree. Its state is therefore
# captured here and asserted unchanged at the end — a gate that quietly mutates
# the tree it grades makes every result it reports suspect, and this one did:
# a probe commit built in $ROOT took the Worker's whole working-tree diff, put
# it on the checked-out task branch under this test's identity and message, and
# appended this file's own probe text to the repository's README, which then
# reached main (L1-20260913-16).
#
# "Was the tree unchanged" is only an answer if the state could be read at all.
# `|| echo '<no git>'` made an unreadable tree compare equal to an unreadable
# tree, and the check reported green having measured nothing. An empty
# `status --porcelain` is a legal value (a clean tree), so the exit status is
# the only thing that distinguishes "clean" from "could not ask".
if ! ROOT_HEAD="$(git -C "$ROOT" rev-parse HEAD 2>/dev/null)"; then
  echo "G3 gitea-real-services: FAILED — cannot read HEAD in $ROOT." >&2
  echo "This script asserts the tree it runs in is unchanged, and will not grade one it cannot read." >&2
  exit 1
fi
if ! ROOT_TREE="$(git -C "$ROOT" status --porcelain 2>/dev/null)"; then
  echo "G3 gitea-real-services: FAILED — cannot read the working tree in $ROOT." >&2
  echo "This script asserts the tree it runs in is unchanged, and will not grade one it cannot read." >&2
  exit 1
fi

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
  if [[ -n "$TOKEN" && -n "${REPO2:-}" ]]; then
    curl -sS -X DELETE -H "Authorization: token $TOKEN" "$BASE/api/v1/repos/$REPO2" >/dev/null 2>&1
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
inst_main_sha() { # the instance's main, or empty when it cannot be read
  # `/branches/main` answers 500 on this Gitea (checked against the running
  # instance, not read from a doc), so the refs API is the one that can answer.
  api GET "/api/v1/repos/$REPO/git/refs/heads/main" 2>/dev/null \
    | python3 -c 'import json,sys; refs=json.load(sys.stdin) or []; print(refs[0]["object"]["sha"] if refs else "")' 2>/dev/null
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
#
# The scratch repo is created ONCE, guarded, and the whole rest of the script
# runs inside it. It used to be `git init "$WORK/work" && cd "$WORK/work"` in a
# script without `set -e`: when the init failed, `&&` short-circuited the `cd`,
# the shell stayed in $ROOT, and the next lines wrote a README, staged the whole
# tree and committed — in the tree under test, as the "g3" identity. The failure
# of a setup step is precisely when the tree under test is at risk, so the setup
# now ends the run instead of falling through it.
if ! git init -q "$WORK/work"; then
  fail "could not create the scratch repository at $WORK/work"
  printf '\nG3 gitea-real-services: %d failure(s)\n' "$FAILS"; exit 1
fi
if ! cd "$WORK/work"; then
  fail "could not enter the scratch repository at $WORK/work"
  printf '\nG3 gitea-real-services: %d failure(s)\n' "$FAILS"; exit 1
fi
# Every push below names its destination ref (HEAD:main, HEAD:refs/heads/g3-probe),
# so which branch this scratch repo starts on does not matter; the second
# `git init` that used to "force" a name only re-initialised the directory it
# was already in.
echo "g3" > README.md && git add -A
git -c user.name=g3 -c user.email=g3@test commit -q -m "g3 probe"
if git -c user.name=g3 -c user.email=g3@test push -q \
     "http://post-git-svc:$TOKEN@${BASE#http://}/$REPO.git" HEAD:main 2>"$WORK/push.err"; then
  ok "pushed to main over HTTP with the service token"
else
  fail "push failed: $(tail -2 "$WORK/push.err")"
fi

# --- two-layer main protection (T0302's premise) ------------------------------
# The canonical rule shape the platform enforces (internal/gitprovider
# mainprotection.go): direct and force pushes blocked for everyone, PR
# merges restricted to the merge service, no bypass identities.
PROT='{"branch_name":"main","rule_name":"main","enable_push":false,"enable_force_push":false,"enable_merge_whitelist":true,"merge_whitelist_usernames":["'$OWNER'"],"enable_bypass_allowlist":false,"required_approvals":1,"enable_status_check":false}'
if [[ "$(code POST "/api/v1/repos/$REPO/branch_protections" "$PROT")" == "201" ]]; then
  ok "branch protection accepted on main"
else
  fail "the instance refused to create branch protection on main"
fi
GOT="$(api GET "/api/v1/repos/$REPO/branch_protections/main")"
if python3 - "$GOT" "$OWNER" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
owner = sys.argv[2]
assert d.get("branch_name") == "main", d
# enable_push=false is the property the adapter depends on: main is not
# writable through the branch, which is what makes "frozen main" enforceable.
assert d.get("enable_push") is False, f"enable_push is {d.get('enable_push')!r}, not False"
# The rest of the canonical shape: force pushes off, merges whitelisted to
# the merge service, no bypass identity that could skip the rule.
assert d.get("enable_force_push") is False, f"enable_force_push is {d.get('enable_force_push')!r}, not False"
assert d.get("enable_merge_whitelist") is True, f"enable_merge_whitelist is {d.get('enable_merge_whitelist')!r}, not True"
assert d.get("merge_whitelist_usernames") == [owner], d.get("merge_whitelist_usernames")
assert d.get("enable_bypass_allowlist") is False, f"enable_bypass_allowlist is {d.get('enable_bypass_allowlist')!r}, not False"
PY
then ok "protection reads back canonical (enable_push=false, force off, merge whitelist [$OWNER], no bypass)"
else fail "protection did not read back canonical: $GOT"; fi

# A direct push to a protected main must be refused — the property, not the setting.
#
# Two things here are the difference between an assertion and a green light:
#
#   * the probe commit is built in the SCRATCH repo, never in $ROOT. This script
#     is run by the G3 gate with the tree under test as the working directory (a
#     task worktree, per gates.json), so a commit in $ROOT takes the Worker's
#     whole diff onto its branch under this test's identity. The property being
#     checked is the INSTANCE's protection of $REPO; the local tree has nothing
#     to do with it.
#   * "the push failed" is not the property. Every way this can go wrong — no
#     scratch repo, a commit that cannot be made, a dead network, a rejected
#     token, an actual refusal — used to reach the same `ok` line, so a probe
#     that never got to ask the question reported that the answer was no. The
#     question is asked of the INSTANCE: did main move? And when it did not, the
#     refusal has to name protection, or the failure is unexplained and this
#     gate refuses to call it a pass.
BEFORE_MAIN="$(inst_main_sha)"
if [[ -z "$BEFORE_MAIN" ]]; then
  fail "cannot read main on the instance before the refusal probe — the probe cannot be judged"
elif ! ( echo "should not land" >> README.md \
         && git -c user.name=g3 -c user.email=g3@test commit -q -am "g3 should be refused" ); then
  fail "could not build the refusal probe commit in $WORK/work — see the commit error above"
else
  push_rc=0
  git -c user.name=g3 -c user.email=g3@test push -q \
    "http://post-git-svc:$TOKEN@${BASE#http://}/$REPO.git" HEAD:main \
    >/dev/null 2>"$WORK/refuse.err" || push_rc=$?
  AFTER_MAIN="$(inst_main_sha)"
  if [[ -z "$AFTER_MAIN" ]]; then
    fail "cannot read main on the instance after the refusal probe — an unreadable state is not a refusal"
  elif [[ "$AFTER_MAIN" != "$BEFORE_MAIN" ]]; then
    fail "a direct push to a protected main SUCCEEDED — protection is configured but not enforced (main moved $BEFORE_MAIN -> $AFTER_MAIN)"
  elif (( push_rc == 0 )); then
    fail "the probe push reported success but main did not move — this probe is not measuring what it claims to"
  elif ! grep -qiE 'protected|pre-receive hook declined' "$WORK/refuse.err"; then
    fail "the push to protected main failed for a reason that is not protection: $(tail -3 "$WORK/refuse.err" | tr '\n' ' ')"
  else
    ok "a direct push to protected main is refused (main is still $BEFORE_MAIN)"
  fi
fi

# The refusal must hold for the strongest identity on the instance: the
# admin. A rule the admin could bypass would protect main from nobody.
ADMIN_USER="${GITEA_ADMIN_USER:-postadmin}"
ADMIN_PASS="${GITEA_ADMIN_PASSWORD:-postadmin_dev_pw}"
push_rc=0
git -c user.name=g3 -c user.email=g3@test push -q \
  "http://$ADMIN_USER:$ADMIN_PASS@${BASE#http://}/$REPO.git" HEAD:main \
  >/dev/null 2>"$WORK/admin-refuse.err" || push_rc=$?
ADMIN_AFTER="$(inst_main_sha)"
if [[ -z "$ADMIN_AFTER" ]]; then
  fail "cannot read main after the admin push probe"
elif [[ "$ADMIN_AFTER" != "$BEFORE_MAIN" ]]; then
  fail "an ADMIN direct push to protected main SUCCEEDED (main moved $BEFORE_MAIN -> $ADMIN_AFTER) — admins must not bypass the rule"
elif (( push_rc == 0 )); then
  fail "the admin probe push reported success but main did not move"
elif ! grep -qiE 'protected|pre-receive hook declined' "$WORK/admin-refuse.err"; then
  fail "the admin push failed for a reason that is not protection: $(tail -3 "$WORK/admin-refuse.err" | tr '\n' ' ')"
else
  ok "an admin direct push to protected main is refused too (no admin bypass)"
fi

# A force push of a rewritten history must be refused the same way.
if ! git -c user.name=g3 -c user.email=g3@test commit -q --amend -m "g3 force probe"; then
  fail "could not rewrite the probe commit for the force-push probe"
else
  push_rc=0
  git -c user.name=g3 -c user.email=g3@test push -q --force \
    "http://post-git-svc:$TOKEN@${BASE#http://}/$REPO.git" HEAD:main \
    >/dev/null 2>"$WORK/force-refuse.err" || push_rc=$?
  FORCE_AFTER="$(inst_main_sha)"
  if [[ -z "$FORCE_AFTER" ]]; then
    fail "cannot read main after the force-push probe"
  elif [[ "$FORCE_AFTER" != "$BEFORE_MAIN" ]]; then
    fail "a force push to protected main SUCCEEDED (main moved $BEFORE_MAIN -> $FORCE_AFTER)"
  elif (( push_rc == 0 )); then
    fail "the force probe push reported success but main did not move"
  elif ! grep -qiE 'protected|pre-receive hook declined' "$WORK/force-refuse.err"; then
    fail "the force push failed for a reason that is not protection: $(tail -3 "$WORK/force-refuse.err" | tr '\n' ' ')"
  else
    ok "a force push to protected main is refused (main is still $BEFORE_MAIN)"
  fi
fi

# Deleting main is refused; the default branch cannot be removed from under
# the protection.
push_rc=0
git -c user.name=g3 -c user.email=g3@test push -q \
  "http://post-git-svc:$TOKEN@${BASE#http://}/$REPO.git" :main \
  >/dev/null 2>"$WORK/delete-refuse.err" || push_rc=$?
DELETE_AFTER="$(inst_main_sha)"
if [[ -z "$DELETE_AFTER" ]]; then
  fail "main is unreadable after the delete probe — it may have been deleted"
elif [[ "$DELETE_AFTER" != "$BEFORE_MAIN" ]]; then
  fail "deleting main changed it ($BEFORE_MAIN -> $DELETE_AFTER)"
elif (( push_rc == 0 )); then
  fail "the delete probe reported success — main must be undeletable"
elif ! grep -qiE 'denied|protected|pre-receive|delete' "$WORK/delete-refuse.err"; then
  fail "the delete-main refusal is unexplained: $(tail -3 "$WORK/delete-refuse.err" | tr '\n' ' ')"
else
  ok "deleting protected main is refused (main is still $BEFORE_MAIN)"
fi

# --- push webhook (T0305's premise, T0301's secret) --------------------------
python3 - "$WORK/deliveries.jsonl" <<'PY' &
import http.server, json, sys
out = open(sys.argv[1], "w")
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n)
        # The signature is verified over the RAW body bytes, so the recorder
        # keeps them hex-exact alongside the parsed payload.
        out.write(json.dumps({
            "event": self.headers.get("X-Gitea-Event"),
            "signature": self.headers.get("X-Gitea-Signature"),
            "body_hex": body.hex(),
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

# The hook is registered WITH an HMAC secret (T0301's webhook-secret
# requirement). Gitea never returns the secret — the platform-side stored
# value stays the only authority — and every delivery must arrive signed
# with X-Gitea-Signature = hex(hmac-sha256(secret, raw body)).
HOOK_SECRET="$(python3 -c 'import secrets;print(secrets.token_hex(32))')"
HOOK="{\"type\":\"gitea\",\"active\":true,\"events\":[\"push\"],\"config\":{\"url\":\"http://$HOOK_HOST:18099/hook\",\"content_type\":\"json\",\"secret\":\"$HOOK_SECRET\"}}"
if [[ "$(code POST "/api/v1/repos/$REPO/hooks" "$HOOK")" == "201" ]]; then
  ok "registered a push webhook with an HMAC secret"
else
  fail "could not register a push webhook"
fi

# Trigger a delivery with a push to a NON-protected branch — from the scratch
# repo, whose HEAD is the probe commit above. This push used to run after a
# `cd "$ROOT"`, so the delivery was triggered by sending the TREE UNDER TEST's
# HEAD to the instance: the content of the repository being graded, published to
# a service, as a side effect of grading it.
push_rc=0
git -c user.name=g3 -c user.email=g3@test push -q \
  "http://post-git-svc:$TOKEN@${BASE#http://}/$REPO.git" HEAD:refs/heads/g3-probe \
  >/dev/null 2>"$WORK/hook-push.err" || push_rc=$?
delivered=0
if (( push_rc != 0 )); then
  fail "the webhook trigger push failed ($(tail -2 "$WORK/hook-push.err" | tr '\n' ' ')) — no delivery can arrive, and the cause is here"
else
  ok "pushed a real commit to a non-protected branch (the webhook trigger)"
  for _ in $(seq 1 40); do
    if [[ -s "$WORK/deliveries.jsonl" ]]; then delivered=1; break; fi
    sleep 0.5
  done
fi
if (( delivered )); then
  HOOK_SECRET="$HOOK_SECRET" python3 - "$WORK/deliveries.jsonl" <<'PY'
import hashlib, hmac, json, os, sys
d = json.loads(open(sys.argv[1]).readline())
assert d["event"] == "push", d
b = d["body"]
for key in ("ref", "repository", "commits", "pusher"):
    assert key in b, f"a real push payload has no {key!r}: {sorted(b)}"
# X-Gitea-Signature = hex(hmac-sha256(secret, raw body)): verified over the
# RAW bytes the provider signed, against the secret the platform registered
# — the exact check T0305's receiver performs per delivery.
assert d["signature"], "delivery carries no X-Gitea-Signature"
raw = bytes.fromhex(d["body_hex"])
want = hmac.new(os.environ["HOOK_SECRET"].encode(), raw, hashlib.sha256).hexdigest()
assert hmac.compare_digest(d["signature"], want), f"signature {d['signature']!r} != {want!r}"
PY
  ok "a real push delivered a webhook whose X-Gitea-Signature verifies against the registered secret"
elif (( push_rc == 0 )); then
  # Ask the instance why, rather than reporting only that nothing arrived.
  HOOK_ID="$(api GET "/api/v1/repos/$REPO/hooks" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d[0]["id"] if d else "")' 2>/dev/null)"
  DETAIL="$(api GET "/api/v1/repos/$REPO/hooks/$HOOK_ID/deliveries" 2>/dev/null | head -c 400)"
  fail "no webhook delivery arrived — the payload shape the ingestion path depends on is unverified. Gitea reports: ${DETAIL:-<no deliveries recorded>}"
fi

# --- the merge service's controlled write path (T0302) ------------------------
# The PR merge API must be the SINGLE way main moves while protected: the
# admin (not in the merge whitelist) cannot merge, and the whitelisted
# merge service can. The refused merge must leave main exactly where it
# was; the accepted one must move it.
MAIN_BEFORE_MERGE="$(inst_main_sha)"
PRNUM="$(api POST "/api/v1/repos/$REPO/pulls" '{"title":"g3 merge probe","head":"g3-probe","base":"main"}' \
  | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("number",""))')"
if [[ -z "$PRNUM" ]]; then
  fail "could not create the merge-probe PR — the controlled write path cannot be probed"
else
  # required_approvals=1: the admin approves (a different identity than the
  # PR author, so the review gate is really exercised).
  review_code="$(curl -s -o /dev/null -w '%{http_code}' -u "$ADMIN_USER:$ADMIN_PASS" \
    -X POST -H 'Content-Type: application/json' -d '{"event":"APPROVED","body":"g3"}' \
    "$BASE/api/v1/repos/$REPO/pulls/$PRNUM/reviews")"
  if [[ "$review_code" != "200" && "$review_code" != "201" ]]; then
    fail "the admin review was refused ($review_code) — the merge probes would not measure the whitelist"
  fi

  admin_merge_code="$(curl -s -o /dev/null -w '%{http_code}' -u "$ADMIN_USER:$ADMIN_PASS" \
    -X POST -H 'Content-Type: application/json' -d '{"Do":"merge"}' \
    "$BASE/api/v1/repos/$REPO/pulls/$PRNUM/merge")"
  ADMIN_MERGE_AFTER="$(inst_main_sha)"
  if [[ "$admin_merge_code" != "403" && "$admin_merge_code" != "405" ]]; then
    fail "the admin merged a PR into main ($admin_merge_code) — the merge whitelist must exclude every human identity"
  elif [[ "$ADMIN_MERGE_AFTER" != "$MAIN_BEFORE_MERGE" ]]; then
    fail "the refused admin merge moved main ($MAIN_BEFORE_MERGE -> $ADMIN_MERGE_AFTER)"
  else
    ok "an admin PR merge is refused ($admin_merge_code) and main stays $MAIN_BEFORE_MERGE"
  fi

  svc_merge_code="$(code POST "/api/v1/repos/$REPO/pulls/$PRNUM/merge" '{"Do":"merge"}')"
  SVC_MERGE_AFTER="$(inst_main_sha)"
  if [[ "$svc_merge_code" != "200" ]]; then
    fail "the merge service could not merge ($svc_merge_code) — the controlled write path must work"
  elif [[ "$SVC_MERGE_AFTER" == "$MAIN_BEFORE_MERGE" ]]; then
    fail "the service merge reported success but main did not move"
  elif ! api GET "/api/v1/repos/$REPO/contents/README.md?ref=main" \
      | python3 -c 'import json,sys,base64;d=json.load(sys.stdin);print(base64.b64decode(d["content"]).decode())' 2>/dev/null \
      | grep -q "should not land"; then
    fail "main after the service merge does not contain the merged commit's content"
  else
    ok "the merge service's PR merge moved main ($MAIN_BEFORE_MERGE -> $SVC_MERGE_AFTER) — the single controlled write path"
  fi
fi

# --- protect-before-first-push (T0302's bootstrap premise) --------------------
# On a fresh repository the rule must hold before main exists: the first
# push is refused, a PR cannot even target the absent main, and even the
# contents API cannot create it under the rule — three walls that would
# deadlock an unseeded repository. The platform's exit is the bootstrap
# seed (before the rule is re-applied), after which the first PR merge
# brings the first commit onto main.
REPO2="$OWNER/g3-boot-$$-$(date +%s)"
if [[ "$(code POST /api/v1/user/repos "{\"name\":\"${REPO2##*/}\",\"auto_init\":false,\"private\":true}")" == "201" ]]; then
  ok "created a second repository for the bootstrap premise ($REPO2)"
else
  fail "could not create the bootstrap-premise repository"
  REPO2=""
fi
if [[ -n "$REPO2" ]]; then
  PROT2='{"branch_name":"main","rule_name":"main","enable_push":false,"enable_force_push":false,"enable_merge_whitelist":true,"merge_whitelist_usernames":["'$OWNER'"],"enable_bypass_allowlist":false}'
  if [[ "$(code POST "/api/v1/repos/$REPO2/branch_protections" "$PROT2")" == "201" ]]; then
    ok "protection applied on $REPO2 before any ref exists"
  else
    fail "the instance refused to protect a branch that has no refs — the first-push block cannot hold"
  fi

  if ! git init -q "$WORK/work2"; then
    fail "could not create the second scratch repository at $WORK/work2"
  else
    echo "boot" > "$WORK/work2/README.md"
    if ! git -C "$WORK/work2" add -A \
       || ! git -C "$WORK/work2" -c user.name=g3 -c user.email=g3@test commit -q -m "g3 boot probe"; then
      fail "could not build the bootstrap probe commit in $WORK/work2"
    else
      push_rc=0
      git -C "$WORK/work2" -c "http.extraHeader=Authorization: token $TOKEN" push -q \
        "http://${BASE#http://}/$REPO2.git" HEAD:main \
        >/dev/null 2>"$WORK/boot-refuse.err" || push_rc=$?
      BOOT_MAIN="$(api GET "/api/v1/repos/$REPO2/git/refs/heads/main" 2>/dev/null \
        | python3 -c 'import json,sys;refs=json.load(sys.stdin) or [];print(refs[0]["object"]["sha"] if refs else "")' 2>/dev/null)"
      if [[ -n "$BOOT_MAIN" ]]; then
        fail "the very first push to a protected main created it ($BOOT_MAIN) — main must be uncreatable through Git"
      elif (( push_rc == 0 )); then
        fail "the first push reported success but main has no ref — this probe is not measuring what it claims to"
      elif ! grep -qiE 'protected|pre-receive hook declined' "$WORK/boot-refuse.err"; then
        fail "the first-push refusal does not name protection: $(tail -3 "$WORK/boot-refuse.err" | tr '\n' ' ')"
      else
        ok "the very first push to a protected main is refused (main never comes into existence)"
      fi
    fi
  fi

  # Research branches stay pushable, and the deadlock shows: no PR can
  # target the absent main, and even the contents API write is refused.
  if git -C "$WORK/work2" -c "http.extraHeader=Authorization: token $TOKEN" push -q \
       "http://${BASE#http://}/$REPO2.git" HEAD:refs/heads/g3-boot-r >/dev/null 2>&1; then
    ok "a research branch push is accepted on the protected-but-empty repository"
  else
    fail "the research branch push on $REPO2 failed — non-main branches must stay pushable"
  fi
  if [[ "$(code POST "/api/v1/repos/$REPO2/pulls" '{"title":"boot","head":"g3-boot-r","base":"main"}')" == "404" ]]; then
    ok "a PR targeting a nonexistent main is refused (404) — the deadlock the bootstrap seed exists for"
  else
    fail "a PR against a nonexistent main was not refused — the deadlock claim is unverified"
  fi
  if [[ "$(code POST "/api/v1/repos/$REPO2/contents/README.md" '{"content":"c2VlZA==","message":"seed attempt","branch":"main"}')" == "403" ]]; then
    ok "even the contents API cannot create main under the rule (403)"
  else
    fail "the contents API wrote to the protected main — the seed window would not need to precede the rule"
  fi

  # The platform's exit, in the production order: seed first, protect after.
  SEED_B64="$(python3 -c "import base64;print(base64.b64encode(('# ${REPO2##*/}\n').encode()).decode())")"
  if [[ "$(code DELETE "/api/v1/repos/$REPO2/branch_protections/main")" == "204" \
     && "$(code POST "/api/v1/repos/$REPO2/contents/README.md" "{\"content\":\"$SEED_B64\",\"message\":\"POST repository bootstrap\",\"branch\":\"main\"}")" == "201" \
     && "$(code POST "/api/v1/repos/$REPO2/branch_protections" "$PROT2")" == "201" ]]; then
    ok "the bootstrap seed lands before the rule is re-applied (production order)"
  else
    fail "the bootstrap sequence (remove rule, seed, re-protect) did not complete"
  fi

  # The first PR merge brings the first research commit onto main.
  push_rc=0
  git -C "$WORK/work2" -c "http.extraHeader=Authorization: token $TOKEN" fetch -q \
    "http://${BASE#http://}/$REPO2.git" main 2>/dev/null || push_rc=$?
  if (( push_rc != 0 )); then
    fail "could not fetch the seeded main into the bootstrap scratch repository"
  elif ! git -C "$WORK/work2" checkout -q -b g3-boot-r2 FETCH_HEAD \
       || ! (echo "first research line" >> "$WORK/work2/README.md" \
             && git -C "$WORK/work2" add -A \
             && git -C "$WORK/work2" -c user.name=g3 -c user.email=g3@test commit -q -m "g3 first research"); then
    fail "could not build the first research branch off the seeded main"
  else
    push_rc=0
    git -C "$WORK/work2" -c "http.extraHeader=Authorization: token $TOKEN" push -q \
      "http://${BASE#http://}/$REPO2.git" HEAD:refs/heads/g3-boot-r2 \
      >/dev/null 2>"$WORK/boot-branch.err" || push_rc=$?
    if (( push_rc != 0 )); then
      fail "the first research branch push on $REPO2 failed: $(tail -2 "$WORK/boot-branch.err" | tr '\n' ' ')"
    else
      BOOT_PR="$(api POST "/api/v1/repos/$REPO2/pulls" '{"title":"first","head":"g3-boot-r2","base":"main"}' \
        | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("number",""))')"
      if [[ -z "$BOOT_PR" ]]; then
        fail "could not create the first PR against the seeded main"
      elif [[ "$(code POST "/api/v1/repos/$REPO2/pulls/$BOOT_PR/merge" '{"Do":"merge"}')" != "200" ]]; then
        fail "the first PR merge on the seeded repository was refused — the controlled write path must work from the first merge on"
      elif ! api GET "/api/v1/repos/$REPO2/contents/README.md?ref=main" \
          | python3 -c 'import json,sys,base64;d=json.load(sys.stdin);print(base64.b64decode(d["content"]).decode())' 2>/dev/null \
          | grep -q "first research line"; then
        fail "main on $REPO2 does not contain the first merged commit's content"
      else
        ok "the first PR merge moves main on the freshly bootstrapped repository"
      fi
    fi
  fi
fi

# Non-interference, asserted rather than assumed. Everything above pushes to the
# INSTANCE and talks to its API; nothing needs to write in the tree under test.
NOW_HEAD="$(git -C "$ROOT" rev-parse HEAD 2>/dev/null)"
NOW_TREE="$(git -C "$ROOT" status --porcelain 2>/dev/null)"
if [[ "$NOW_HEAD" != "$ROOT_HEAD" ]]; then
  fail "this script moved HEAD in $ROOT ($ROOT_HEAD -> $NOW_HEAD) — the gate graded a tree it had already changed"
elif [[ "$NOW_TREE" != "$ROOT_TREE" ]]; then
  fail "this script changed the working tree in $ROOT — the gate graded a tree it had already changed: $(printf '%s' "$NOW_TREE" | head -3 | tr '\n' ' ')"
else
  ok "the tree under test is exactly as this script found it (HEAD and working tree unchanged)"
fi

printf '\n'
if (( FAILS )); then
  printf 'G3 gitea-real-services: %d failure(s)\n' "$FAILS"
  exit 1
fi
printf 'G3 gitea-real-services: all checks passed against a real Gitea\n'
