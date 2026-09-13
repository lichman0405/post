#!/usr/bin/env bash
#
# Unit tests for tests/acceptance/gitea-real-services-e2e.sh's own discipline.
#
# The G3 gate runs that script with the TREE UNDER TEST as its working
# directory, and it once graded trees it had already changed: an unguarded
# `git init … && cd …` fell through into the repository root and committed a
# Worker's entire diff onto its branch under the test's own identity, and its
# probe line ("should not land") reached main (L1-20260913-16). A gate that
# mutates what it grades cannot be believed about anything it reports.
#
# These tests never touch a real Gitea instance and never skip: a fake API
# answers the handful of endpoints the script calls, so what is exercised here
# is the script's own guards —
#
#   1. a probe that CANNOT ask its question (no scratch repo, no git backend)
#      must fail loudly rather than report "a direct push to protected main is
#      refused". The failure of the setup is exactly the moment the tree is at
#      risk, and "the push failed" is not the property under test;
#   2. a tree whose state cannot be read is not a tree that was left unchanged:
#      the non-interference check must refuse, not compare two sentinels;
#   3. nothing the script does may write in the tree it grades.
#
# Runnable on a host with bash + coreutils + git + python3. No network.
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
GATE="$ROOT/tests/acceptance/gitea-real-services-e2e.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILS=0
RC=0
OUT=""
fail() { printf 'FAIL %s\n' "$*"; FAILS=$((FAILS+1)); }
ok()   { printf 'ok   %s\n' "$*"; }

if [[ ! -f "$GATE" ]]; then
  echo "FAIL the gate script under test is missing: $GATE" >&2
  exit 1
fi

# --- a Gitea that answers, but cannot accept a push --------------------------
#
# Every endpoint the script calls is served, so the run reaches the probes; no
# git-http-backend is served, so every `git push` fails with a transport error
# — a failure that says nothing about branch protection. That is the case the
# script must NOT report as a pass.
cat > "$WORK/fake-gitea.py" <<'PY'
import json, re, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

MAIN_SHA = "a" * 40

class H(BaseHTTPRequestHandler):
    def _send(self, code, obj):
        body = b"" if obj is None else json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if body:
            self.wfile.write(body)

    def do_GET(self):
        p = self.path
        if p == "/api/v1/version":
            return self._send(200, {"version": "fake"})
        if p == "/api/v1/user":
            return self._send(200, {"login": "fakeowner"})
        if re.search(r"/branch_protections/[^/]+$", p):
            return self._send(200, {"branch_name": "main", "enable_push": False})
        if p.endswith("/git/refs/heads/main"):
            return self._send(200, [{"ref": "refs/heads/main",
                                     "object": {"type": "commit", "sha": MAIN_SHA}}])
        if p.endswith("/hooks"):
            return self._send(200, [{"id": 1}])
        if "/deliveries" in p:
            return self._send(200, [])
        return self._send(404, {"message": "not found"})

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        if n:
            self.rfile.read(n)
        p = self.path
        if p == "/api/v1/user/repos":
            return self._send(201, {"name": "probe"})
        if p.endswith("/branch_protections") or p.endswith("/hooks"):
            return self._send(201, {"id": 1})
        return self._send(404, {"message": "not found"})

    def do_DELETE(self):
        return self._send(204, None)

    def log_message(self, *a):
        pass

srv = HTTPServer(("127.0.0.1", 0), H)
print(srv.server_port, flush=True)
srv.serve_forever()
PY

python3 "$WORK/fake-gitea.py" > "$WORK/port" 2>"$WORK/fake.err" &
FAKE_PID=$!
for _ in $(seq 1 40); do [[ -s "$WORK/port" ]] && break; sleep 0.25; done
PORT="$(cat "$WORK/port" 2>/dev/null || true)"
if [[ -z "$PORT" ]]; then
  fail "could not start the fake Gitea API: $(cat "$WORK/fake.err" 2>/dev/null | tail -2)"
  printf '\n%d failure(s)\n' "$FAILS"; exit 1
fi
ok "fake Gitea API listening on 127.0.0.1:$PORT"

# --- the tree under test -----------------------------------------------------
#
# A git repo with an uncommitted change, which is what the script sees under
# G3 (a task worktree holding the Worker's diff). Every run below asserts the
# truth about this tree.
new_tree() { # -> a repo path with the script in place and a dirty file
  local d="$1"
  mkdir -p "$d/tests/acceptance" "$d/internal/config"
  git -C "$d" init -q
  printf 'x\n' > "$d/README.md"
  git -C "$d" add -A
  git -C "$d" -c user.name=t -c user.email=t@e commit -q -m base
  printf 'worker deliverable\n' > "$d/internal/config/deliverable.txt"
  python3 - "$GATE" "$d/tests/acceptance/gitea-real-services-e2e.sh" <<'PY'
import shutil, sys
shutil.copyfile(sys.argv[1], sys.argv[2])
PY
  chmod +x "$d/tests/acceptance/gitea-real-services-e2e.sh"
}

run_gate() { # tree [extra env] -> sets RC/OUT
  local tree="$1"; shift
  OUT="$(cd "$tree" && env POST_GITEA_BASE_URL="http://127.0.0.1:$PORT" \
        POST_GITEA_TOKEN=fake-token POST_G3_HOOK_HOST=127.0.0.1 "$@" \
        bash tests/acceptance/gitea-real-services-e2e.sh 2>&1)"
  RC=$?
}

tree_state() { # HEAD + porcelain, as one string
  printf '%s|%s' "$(git -C "$1" rev-parse HEAD 2>/dev/null)" "$(git -C "$1" status --porcelain 2>/dev/null | tr '\n' ';')"
}

# --- 1. a push that fails for a reason other than protection -----------------
TREE="$WORK/tree-a"
new_tree "$TREE"
BEFORE="$(tree_state "$TREE")"
run_gate "$TREE"
if (( RC == 0 )); then
  fail "the script passed against an instance that cannot accept a push — its refusal probe never asked its question"
else
  ok "an instance that cannot accept a push fails the run (exit $RC)"
fi
case "$OUT" in
  *"failed for a reason that is not protection"*)
    ok "the unexplained push failure is reported as unexplained" ;;
  *) fail "the unexplained push failure is not reported as such: $(printf '%s' "$OUT" | grep -i 'protected main' | head -2)" ;;
esac
case "$OUT" in
  *"a direct push to protected main is refused"*)
    fail "a push that never reached the instance is reported as 'refused' — the gate would pass without checking protection" ;;
  *) ok "the refusal is not claimed on the strength of a failed push" ;;
esac
case "$OUT" in
  *"the tree under test is exactly as this script found it"*)
    ok "the non-interference check ran and reported the tree unchanged" ;;
  *) fail "the run did not report on the tree it graded" ;;
esac
AFTER="$(tree_state "$TREE")"
[[ "$BEFORE" == "$AFTER" ]] \
  && ok "the tree under test is byte-identical before and after" \
  || fail "the run changed the tree it graded: [$BEFORE] -> [$AFTER]"

# --- 2. the scratch repo cannot be created ----------------------------------
# The failure mode that started all of this: with the setup guarded by `&&` and
# no `set -e`, a failed `git init` left the shell in the tree under test, and
# the next lines wrote a README, staged everything and committed — there.
mkdir -p "$WORK/shim"
REAL_GIT="$(command -v git)"
cat > "$WORK/shim/git" <<SH
#!/usr/bin/env bash
for a in "\$@"; do
  if [[ "\$a" == "init" ]]; then
    echo "fatal: simulated git init failure" >&2
    exit 1
  fi
done
# The real git by absolute path: resolving it through PATH here would find
# this shim again and recurse.
exec "$REAL_GIT" "\$@"
SH
chmod +x "$WORK/shim/git"

TREE="$WORK/tree-b"
new_tree "$TREE"
BEFORE="$(tree_state "$TREE")"
run_gate "$TREE" "PATH=$WORK/shim:$PATH"
if (( RC == 0 )); then
  fail "the script passed without ever creating its scratch repository"
else
  ok "a scratch repo that cannot be created fails the run (exit $RC)"
fi
case "$OUT" in
  *"could not create the scratch repository"*)
    ok "the refusal names the scratch repository as the cause" ;;
  *) fail "the scratch-repo failure is not named: $(printf '%s' "$OUT" | head -3 | tr '\n' ' ')" ;;
esac
AFTER="$(tree_state "$TREE")"
[[ "$BEFORE" == "$AFTER" ]] \
  && ok "the failed setup wrote nothing in the tree under test" \
  || fail "a failed setup fell through into the tree under test: [$BEFORE] -> [$AFTER]"
if git -C "$TREE" log --format=%s -3 2>/dev/null | grep -q 'g3 probe\|g3 should be refused'; then
  fail "the gate committed in the tree it was grading: $(git -C "$TREE" log --format='%h %an %s' -1)"
else
  ok "no probe commit was made in the tree under test"
fi

# --- 3. a tree whose state cannot be read ------------------------------------
# `|| echo '<no git>'` made "could not ask" compare equal to "could not ask",
# and the check reported the tree unchanged having measured nothing.
TREE="$WORK/tree-c"
mkdir -p "$TREE/tests/acceptance"
python3 - "$GATE" "$TREE/tests/acceptance/gitea-real-services-e2e.sh" <<'PY'
import shutil, sys
shutil.copyfile(sys.argv[1], sys.argv[2])
PY
run_gate "$TREE"
if (( RC == 0 )); then
  fail "the script graded a directory that is not a git work tree"
else
  ok "a tree whose state cannot be read fails the run (exit $RC)"
fi
case "$OUT" in
  *"cannot read HEAD in"*)
    ok "the refusal says the tree's state could not be read" ;;
  *) fail "the unreadable tree is not named: $(printf '%s' "$OUT" | head -3 | tr '\n' ' ')" ;;
esac

# --- 4. no mutating git command runs in the tree under test ------------------
# The original defect was not a missing assertion but a command in the wrong
# directory: `git add -A`, `git commit` and `git push HEAD:main` executed with
# the tree under test as cwd. Case 1 catches that after the fact, because the
# tree is left changed — but a run that committed and then reset would leave it
# byte-identical, so where each mutating command ran is asserted here rather
# than inferred from the wreckage.
mkdir -p "$WORK/logshim"
REAL_GIT="$(command -v git)"
cat > "$WORK/logshim/git" <<SH
#!/usr/bin/env python3
import os, sys

MUTATING = {"add", "commit", "push", "tag", "reset", "checkout", "rebase",
            "stash", "rm", "mv", "merge", "cherry-pick", "clean", "apply", "init"}

args = sys.argv[1:]
sub, repo = None, None
i = 0
while i < len(args):
    a = args[i]
    if a == "-C" and i + 1 < len(args):
        i += 1
        repo = args[i]
    elif a.startswith("--git-dir="):
        repo = a.split("=", 1)[1]
    if sub is None and a in MUTATING:
        sub = a
    i += 1

# Where the command acts. -C/--git-dir wins; \`git init <path>\` names its
# target in its last argument (a bare \`git init\` re-initialises the cwd); for
# everything else the repository is the cwd.
where = repo or os.getcwd()
if sub == "init" and args and not args[-1].startswith("-"):
    where = args[-1]

with open(os.environ["GIT_LOG"], "a") as f:
    f.write("%s\t%s\t%s\n" % (os.getcwd(), where, sub or ""))
os.execv("$REAL_GIT", ["$REAL_GIT"] + args)
SH
chmod +x "$WORK/logshim/git"

TREE="$WORK/tree-d"
new_tree "$TREE"
rm -f "$WORK/gitlog"
BEFORE="$(tree_state "$TREE")"
run_gate "$TREE" "PATH=$WORK/logshim:$PATH" "GIT_LOG=$WORK/gitlog"
AFTER="$(tree_state "$TREE")"
[[ "$BEFORE" == "$AFTER" ]] \
  && ok "the logged run left the tree byte-identical" \
  || fail "the logged run changed the tree it graded: [$BEFORE] -> [$AFTER]"

JUDGE="$(python3 - "$WORK/gitlog" "$TREE" <<'PY'
import os, sys
log, tree = sys.argv[1], os.path.realpath(sys.argv[2])
mutating = {"add", "commit", "push", "tag", "reset", "checkout", "rebase",
            "stash", "rm", "mv", "merge", "cherry-pick", "clean", "apply", "init"}
if not os.path.exists(log):
    print("0\t0\tno log file was written")
    raise SystemExit(0)
bad, n = [], 0
for line in open(log):
    parts = (line.rstrip("\n").split("\t") + ["", "", ""])[:3]
    cwd, where, sub = parts
    if sub not in mutating:
        continue
    n += 1
    if os.path.realpath(where) == tree:
        bad.append("%s acting on %s (cwd %s)" % (sub, where, cwd))
print("%d\t%d\t%s" % (n, len(bad), "; ".join(bad)))
PY
)"
N_MUTATING="${JUDGE%%$'\t'*}"; REST="${JUDGE#*$'\t'}"
N_BAD="${REST%%$'\t'*}"; VIOLATIONS="${REST#*$'\t'}"
if (( N_MUTATING == 0 )); then
  fail "no mutating git command was logged at all — the gate cannot have done its work in the scratch repo, so this check would be vacuous"
else
  ok "logged $N_MUTATING mutating git command(s) and judged where each one ran"
fi
if (( N_BAD > 0 )); then
  fail "a mutating git command ran in the tree under test: $VIOLATIONS"
else
  ok "every mutating git command acted on somewhere other than the tree under test"
fi

kill "$FAKE_PID" 2>/dev/null

printf '\n'
if (( FAILS )); then
  printf 'gitea-real-services guard unit tests: %d failure(s)\n' "$FAILS"
  exit 1
fi
printf 'gitea-real-services guard unit tests: all checks passed\n'
