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
  if [[ -n "${SHADOW:-}" ]]; then
    # Best-effort: the shadow account (and its tokens) dies with the run.
    curl -sS -o /dev/null -u "${ADMIN_USER:-postadmin}:${ADMIN_PASS:-postadmin_dev_pw}" \
      -X DELETE "$BASE/api/v1/admin/users/$SHADOW" >/dev/null 2>&1
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

FAILS=0
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

# git_authed runs git with the service token supplied as a config override in
# the ENVIRONMENT, never argv. `-c http.extraHeader=...` put the credential in
# the argument list, which `ps aux` and /proc/<pid>/cmdline show to every
# account on the machine; GIT_CONFIG_VALUE_0 is in neither of them. Same shape
# as internal/gitprovider/gitea.go's gitEnv.
git_authed() {
  GIT_CONFIG_COUNT=1 \
  GIT_CONFIG_KEY_0=http.extraHeader \
  GIT_CONFIG_VALUE_0="Authorization: token $TOKEN" \
    git "$@"
}

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
# code_body is code() that KEEPS what the instance answered. Sets:
#   CB_CODE  the HTTP status, or "" when curl itself failed
#   CB_RC    curl's exit status (non-zero means CB_CODE says nothing)
#   CB_BODY  the response body, one line, truncated — Gitea's own reason
#
# `code()` throws the body away, and the bootstrap sequence below was written
# on top of it: three requests in one `&&` chain, one sentence for all of them.
# A 403 (rule still in effect), a 404 (no such rule) and a 422 (bad payload)
# therefore all arrived as "the bootstrap sequence (remove rule, seed,
# re-protect) did not complete" — same words, no code, no body, no way back to
# which request said no (issue #240). The body is where Gitea names the reason,
# and a rare red that cannot be attributed is indistinguishable from a test
# that is simply wrong.
CB_CODE=""; CB_RC=0; CB_BODY=""
code_body() { # code_body METHOD PATH [BODY]
  local method="$1" path="$2" body="${3:-}" bodyfile="$WORK/cb-body" errfile="$WORK/cb-err"
  : >"$bodyfile"; : >"$errfile"
  if [[ -n "$body" ]]; then
    CB_CODE="$(curl -sS -o "$bodyfile" -w '%{http_code}' -X "$method" \
      -H "Authorization: token $TOKEN" -H 'Content-Type: application/json' \
      -d "$body" "$BASE$path" 2>"$errfile")"
  else
    CB_CODE="$(curl -sS -o "$bodyfile" -w '%{http_code}' -X "$method" \
      -H "Authorization: token $TOKEN" "$BASE$path" 2>"$errfile")"
  fi
  CB_RC=$?
  # One line, bounded: the failure messages stay grep-able and a stray HTML
  # error page cannot bury the rest of the log.
  CB_BODY="$(tr -d '\r\n' <"$bodyfile" 2>/dev/null | head -c 400)"
  if [[ -z "$CB_BODY" ]]; then
    # An empty body is not the same as nothing to say: curl's own stderr is
    # the only explanation when there was no HTTP response at all.
    CB_BODY="$(tr -d '\r\n' <"$errfile" 2>/dev/null | head -c 400)"
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
      git_authed -C "$WORK/work2" push -q \
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
  if git_authed -C "$WORK/work2" push -q \
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
  #
  # The three requests are run UNCONDITIONALLY and asserted one by one. The
  # shape they replaced was a single `&&` chain — the first request that
  # strayed short-circuited the other two away — behind one sentence that
  # named no step and printed no code. So a run that went red was reportable
  # only as "the bootstrap sequence did not complete", which is the whole of
  # issue #240: nobody could say whether the delete had failed, whether the
  # delete had answered 204 and the WRITE was still refused (a stale rule
  # read), or whether the re-protect was the odd one out. Those are three
  # different defects and they were the same sentence.
  #
  # Now every step's code is kept, a stray code fails the step it belongs to
  # with the status and the body the instance sent back, and all three codes
  # are printed on every run — the green ones included. And there is NO retry
  # here, because a retry was measured and it does not work:
  #
  # T1216 ran this file 32 times under concurrent instance load (8-way
  # repository churn). Eight went red, all with the same signature:
  #
  #   1/3 remove-rule=204   2/3 seed=404   3/3 re-protect=201 (attempts 1/4/1)
  #   body: {"message":"branch does not exist [repo_id: N name: main]"}
  #
  # All eight exhausted four attempts one second apart: the same request, in
  # the same state, answers 404 for at least three seconds and then keeps
  # answering 404 — it is not a consistency window that closes. Re-issuing is
  # therefore NOT the answer and the retry was removed rather than tuned. (Two
  # further candidate cures were measured against the same state and rejected:
  # restoring the repository's default branch with PATCH does not clear the
  # 404, and the 404 is not the deleted rule still being in effect — step 3
  # re-applies the rule afterwards with 201, and the body names the branch,
  # not a refusal.)
  #
  # What is known about the trigger, and what is not. The 404 is on the seed
  # write only: the rule really was gone (204, then re-applied 201), so the
  # delete is not it. It reproduces when this repository has just been pushed
  # to, and it is the same request the platform itself sends
  # (internal/gitprovider/mainprotection.go, EnsureInitialMain), which answers
  # 201 in every other loaded and unloaded run. The same state is NOT
  # discriminated by default_branch: of six strays reproduced from the same
  # sequence, three had the repository pointing at main and three at the pushed
  # research branch, and forcing the field back to main on an unprotected
  # repository still seeded 201 — so the field is neither the cause nor a
  # reliable predictor, and this file does not get to guess further. The gate's
  # job ends at reporting the step, the code and the instance's own reason,
  # which it now does; locating the trigger is a decision about the platform's
  # bootstrap, and it belongs with the product rather than with this file's
  # sentences (T1216 records the diagnosis and hands the hole off).
  SEED_B64="$(python3 -c "import base64;print(base64.b64encode(('# ${REPO2##*/}\n').encode()).decode())")"
  boot_step() { # boot_step LABEL EXPECTED METHOD PATH [BODY] — sets BOOT_CODE/BOOT_BODY
    local label="$1" expected="$2" method="$3" path="$4" body="${5:-}" code extra
    code_body "$method" "$path" "$body"
    code="${CB_CODE:-<no response, curl rc=$CB_RC>}"
    extra=""
    [[ "$code" == "$expected" ]] || extra=" — body: ${CB_BODY:-<empty>}"
    printf '     %s: %s %s -> %s (expected %s) at %s%s\n' \
      "$label" "$method" "$path" "$code" "$expected" "$(date -u +%H:%M:%SZ)" "$extra"
    BOOT_CODE="$code"; BOOT_BODY="$CB_BODY"
    return 0
  }
  boot_verdict() { # boot_verdict LABEL EXPECTED CODE BODY
    # Every field is passed in. The first cut read the boot_step globals here,
    # and because the verdicts run after all three steps, each verdict reported
    # the LAST step's result — step 1's failure was printed with step 3's code
    # and step 3's body. One more way for a failing run to describe a step it
    # never measured (T1216).
    local label="$1" expected="$2" code="$3" body="$4"
    [[ "$code" == "$expected" ]] && return 0
    fail "$label answered ${code:-<no response>}, expected $expected at $(date -u +%H:%M:%SZ) — body: ${body:-<empty>}"
    return 1
  }
  boot_step "bootstrap step 1/3 (remove rule: DELETE /branch_protections/main)" 204 \
    DELETE "/api/v1/repos/$REPO2/branch_protections/main"
  BOOT_DEL_CODE="$BOOT_CODE"; BOOT_DEL_BODY="$BOOT_BODY"
  boot_step "bootstrap step 2/3 (seed main: POST /contents/README.md)" 201 \
    POST "/api/v1/repos/$REPO2/contents/README.md" \
    "{\"content\":\"$SEED_B64\",\"message\":\"POST repository bootstrap\",\"branch\":\"main\"}"
  BOOT_SEED_CODE="$BOOT_CODE"; BOOT_SEED_BODY="$BOOT_BODY"
  boot_step "bootstrap step 3/3 (re-protect: POST /branch_protections)" 201 \
    POST "/api/v1/repos/$REPO2/branch_protections" "$PROT2"
  BOOT_PROT_CODE="$BOOT_CODE"; BOOT_PROT_BODY="$BOOT_BODY"

  BOOT_BAD=0
  boot_verdict "bootstrap step 1/3 (remove rule: DELETE /branch_protections/main)" 204 \
    "$BOOT_DEL_CODE" "$BOOT_DEL_BODY" || BOOT_BAD=1
  boot_verdict "bootstrap step 2/3 (seed main: POST /contents/README.md)" 201 \
    "$BOOT_SEED_CODE" "$BOOT_SEED_BODY" || BOOT_BAD=1
  boot_verdict "bootstrap step 3/3 (re-protect: POST /branch_protections)" 201 \
    "$BOOT_PROT_CODE" "$BOOT_PROT_BODY" || BOOT_BAD=1
  printf '     bootstrap sequence codes: 1/3 remove-rule=%s 2/3 seed=%s 3/3 re-protect=%s\n' \
    "${BOOT_DEL_CODE:-<none>}" "${BOOT_SEED_CODE:-<none>}" "${BOOT_PROT_CODE:-<none>}"
  if (( BOOT_BAD )); then
    # A stray code on a step is only diagnosable against the state the instance
    # thinks the repository is in, and both halves of that state are cheap to
    # ask for here: which refs the repository has, and which of them Gitea has
    # taken as the repository's default branch. The seed's 404 is about main
    # not existing; whether the instance agrees it is the default branch is the
    # difference between "the branch is missing" and "the repository is not
    # pointed at that branch", and they need different fixes.
    BOOT_REFS="$(api GET "/api/v1/repos/$REPO2/git/refs" 2>/dev/null \
      | python3 -c 'import json,sys;refs=json.load(sys.stdin) or [];print(", ".join(r.get("ref","?") for r in refs) or "<no refs>")' 2>/dev/null)"
    BOOT_DEFAULT="$(api GET "/api/v1/repos/$REPO2" 2>/dev/null \
      | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d.get("default_branch") or "<none>")' 2>/dev/null)"
    printf '     %s has refs: %s\n' "$REPO2" "${BOOT_REFS:-<unreadable>}"
    printf '     %s default_branch: %s\n' "$REPO2" "${BOOT_DEFAULT:-<unreadable>}"
  else
    ok "the bootstrap seed lands before the rule is re-applied (production order)"
  fi

  # The first PR merge brings the first research commit onto main.
  push_rc=0
  git_authed -C "$WORK/work2" fetch -q \
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
    git_authed -C "$WORK/work2" push -q \
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

# --- scoped user tokens (T0304: the adapter's premises) -----------------------
# The platform mints per-user scoped tokens through the ADMIN's basic auth
# (Gitea 1.27 refuses API-token auth on the token surface) and grants repo
# reach through collaborator memberships made by the service account (the
# repo owner). The properties asserted, against the real instance:
#
#   * a collaborator's read-scoped token clones the private repository;
#   * the same token cannot push (the scope is enforced);
#   * a revoked token dies immediately;
#   * a valid token WITHOUT a grant cannot clone the private repository at
#     all — 无权限用户 clone private 失败, T0304's acceptance criterion.
#
# The admin pair is re-derived on purpose (ADMIN_USER/ADMIN_PASS were set
# above from GITEA_ADMIN_* for T0302's probes): this gate must check the
# pair the PLATFORM actually reads — internal/gitprovider/config.go reads
# POST_GITEA_ADMIN_* — not the bootstrap pair. The dev stack resolves both
# keys to the same account (postadmin), but they name different contracts;
# silently reusing the GITEA_ADMIN_* pair would report green for a system
# the platform could not actually administer. A wrong pair fails these
# checks loudly — never a skip (a G3 that skips reports green for a system
# nobody checked).
ADMIN_USER="${POST_GITEA_ADMIN_USER:-}"
ADMIN_PASS="${POST_GITEA_ADMIN_PASSWORD:-}"
if [[ -z "$ADMIN_USER" || -z "$ADMIN_PASS" ]] && [[ -f "$ROOT/.env.dev" ]]; then
  # shellcheck disable=SC1091
  ADMIN_USER="$(set -a; . "$ROOT/.env.dev" >/dev/null 2>&1; printf '%s' "${POST_GITEA_ADMIN_USER:-}")"
  # shellcheck disable=SC1091
  ADMIN_PASS="$(set -a; . "$ROOT/.env.dev" >/dev/null 2>&1; printf '%s' "${POST_GITEA_ADMIN_PASSWORD:-}")"
fi
[[ -z "$ADMIN_USER" ]] && ADMIN_USER=postadmin
[[ -z "$ADMIN_PASS" ]] && ADMIN_PASS=postadmin_dev_pw

# The clone/push probes below must FAIL, never hang: on a TTY, git would
# prompt for credentials after a 401 (the revoked-token clone) — kill the
# interactive prompt so a refused credential is a non-zero exit, like the
# integration test's gitRun (GIT_TERMINAL_PROMPT=0, credential.helper=
# per invocation — a machine credential helper must not substitute a
# working credential and flip a must-fail assertion).
export GIT_TERMINAL_PROMPT=0

SHADOW="u-g3-$$-$(date +%s)"
api_admin() { # api_admin METHOD PATH [BODY] — admin basic auth
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -sS -u "$ADMIN_USER:$ADMIN_PASS" -X "$method" \
      -H 'Content-Type: application/json' -d "$body" "$BASE$path"
  else
    curl -sS -u "$ADMIN_USER:$ADMIN_PASS" -X "$method" "$BASE$path"
  fi
}
code_admin() { # code_admin METHOD PATH [BODY]
  local method="$1" path="$2" body="${3:-}"
  if [[ -n "$body" ]]; then
    curl -sS -o /dev/null -w '%{http_code}' -u "$ADMIN_USER:$ADMIN_PASS" -X "$method" \
      -H 'Content-Type: application/json' -d "$body" "$BASE$path"
  else
    curl -sS -o /dev/null -w '%{http_code}' -u "$ADMIN_USER:$ADMIN_PASS" -X "$method" "$BASE$path"
  fi
}

SHADOW_PW="$(python3 -c 'import secrets;print(secrets.token_hex(24))')"
CREATE_BODY="{\"username\":\"$SHADOW\",\"email\":\"$SHADOW@users.invalid\",\"password\":\"$SHADOW_PW\",\"must_change_password\":false,\"send_notify\":false}"
if [[ "$(code_admin POST /api/v1/admin/users "$CREATE_BODY")" == "201" ]]; then
  ok "created a shadow account through the admin API ($SHADOW)"
else
  fail "could not create the shadow account $SHADOW — check POST_GITEA_ADMIN_USER/POST_GITEA_ADMIN_PASSWORD (token management is admin basic-auth-only)"
fi

# The collaborator grant rides the SERVICE token: only the repository
# owner may manage collaborators, and the platform's repositories live in
# the service account's namespace.
if [[ "$(code PUT "/api/v1/repos/$REPO/collaborators/$SHADOW" '{"permission":"read"}')" == "204" ]]; then
  ok "granted the shadow account read access on the private repository"
else
  fail "could not grant the shadow account collaborator access"
fi

# The scoped token: minted through the admin's basic auth, read-only.
MINT="$(api_admin POST "/api/v1/users/$SHADOW/tokens" "{\"name\":\"g3-t0304\",\"scopes\":[\"read:repository\"]}")"
SHADOW_TOKEN="$(printf '%s' "$MINT" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("sha1",""))' 2>/dev/null)"
SHADOW_TOKEN_ID="$(printf '%s' "$MINT" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))' 2>/dev/null)"
if [[ -n "$SHADOW_TOKEN" && -n "$SHADOW_TOKEN_ID" ]]; then
  ok "minted a read-scoped token for the shadow account (id $SHADOW_TOKEN_ID)"
else
  fail "could not mint the shadow token: $MINT"
fi

if [[ -n "$SHADOW_TOKEN" ]]; then
  clone_err=0
  git -c credential.helper= clone -q "http://$SHADOW:$SHADOW_TOKEN@${BASE#http://}/$REPO.git" \
    "$WORK/clone-read" 2>"$WORK/clone-read.err" || clone_err=$?
  if (( clone_err == 0 )); then
    ok "a collaborator's read token cloned the private repository"
  else
    fail "the read-token clone failed: $(tail -2 "$WORK/clone-read.err" | tr '\n' ' ')"
  fi

  if (( clone_err == 0 )); then
    echo "read tokens cannot push" > "$WORK/clone-read/proof.txt"
    git -C "$WORK/clone-read" -c user.name=g3 -c user.email=g3@test add -A
    git -C "$WORK/clone-read" -c user.name=g3 -c user.email=g3@test \
      commit -q -m "g3 read-token push must fail"
    push_rc=0
    git -C "$WORK/clone-read" -c credential.helper= push -q \
      "http://$SHADOW:$SHADOW_TOKEN@${BASE#http://}/$REPO.git" \
      HEAD:refs/heads/g3-read-probe >/dev/null 2>"$WORK/read-push.err" || push_rc=$?
    if (( push_rc != 0 )); then
      ok "a read-scoped token cannot push (the scope is enforced)"
    else
      fail "a read-scoped token pushed — the scope is NOT enforced"
    fi
  fi

  # Revocation: the token dies immediately (the DELETE is the enforcement).
  REVOKE_CODE="$(code_admin DELETE "/api/v1/users/$SHADOW/tokens/$SHADOW_TOKEN_ID")"
  if [[ "$REVOKE_CODE" == "204" ]]; then
    ok "revoked the token"
  else
    fail "could not revoke the token (admin DELETE answered $REVOKE_CODE)"
  fi
  clone_err=0
  git -c credential.helper= clone -q "http://$SHADOW:$SHADOW_TOKEN@${BASE#http://}/$REPO.git" \
    "$WORK/clone-revoked" 2>"$WORK/clone-revoked.err" || clone_err=$?
  if (( clone_err != 0 )); then
    ok "the revoked token can no longer clone (revocation is enforced)"
  else
    fail "the revoked token still cloned — revocation is NOT enforced"
  fi

  # 无权限用户 clone private 失败: a FRESH, still-valid token held by a user
  # WITHOUT a collaborator grant cannot reach the private repository — the
  # access mapping governs reach, not token existence.
  MINT2="$(api_admin POST "/api/v1/users/$SHADOW/tokens" "{\"name\":\"g3-t0304-b\",\"scopes\":[\"read:repository\"]}")"
  SHADOW_TOKEN2="$(printf '%s' "$MINT2" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("sha1",""))' 2>/dev/null)"
  if [[ "$(code DELETE "/api/v1/repos/$REPO/collaborators/$SHADOW")" == "204" ]]; then
    ok "removed the collaborator grant (access revocation)"
  else
    fail "could not remove the collaborator grant"
  fi
  if [[ -n "$SHADOW_TOKEN2" ]]; then
    clone_err=0
    git -c credential.helper= clone -q "http://$SHADOW:$SHADOW_TOKEN2@${BASE#http://}/$REPO.git" \
      "$WORK/clone-nogrant" 2>"$WORK/clone-nogrant.err" || clone_err=$?
    if (( clone_err != 0 )); then
      ok "a valid token without a grant cannot clone the private repository (无权限用户 clone 失败)"
    else
      fail "a user without access cloned the private repository — the access mapping is NOT enforced"
    fi
  else
    fail "could not mint the second shadow token: $MINT2"
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
