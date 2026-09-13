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
#   3. nothing the script does may write in the tree it grades. This is asserted
#      by TWO independent nets, because either one alone has a blind spot:
#      a NAMING net reads the shim's log of where each command acted — it can
#      say which command did it, but it reasons about git's command line, and
#      a spelling it mis-parses walks past it (six of them did) — and a
#      MEASURING net fingerprints the tree's own state (HEAD, its refs, its
#      config, every entry in the worktree and every entry in .git, by kind)
#      before and after the run — it needs no grammar, so nothing can be
#      spelled past it, but it cannot say who did it. The run fails if either
#      net fires; the self-check below requires the naming net to name every
#      spelling AND the measuring net to see the write on its own;
#   4. an environment that redirects git (GIT_DIR) is not a way around any of
#      it.
#
# Two properties are deliberately OUT of scope here, because a fake that cannot
# accept a push cannot witness them: that the gate re-reads the instance's main
# after the probe (a fake serves a constant sha, so "read it twice" and "read it
# once" look the same), and that the probe actually reached the instance at all
# (contacting it and failing unexplained is indistinguishable, offline, from not
# contacting it). The real-instance run in the G3 gate covers both.
#
# Runnable on a host with bash + coreutils + git + python3 + curl. No network.
set -u
export LC_ALL=C

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
GATE="$ROOT/tests/acceptance/gitea-real-services-e2e.sh"
WORK="$(mktemp -d)"
# The fake server is killed on every exit path, not just the happy one: an
# `exit 1` in the middle of a failing run used to leave a python process
# holding a loopback listener, reparented to init, for the life of the host.
# Same for a signal, which bash does not turn into an EXIT trap on its own.
cleanup() {
  [[ -n "${FAKE_PID:-}" ]] && kill "$FAKE_PID" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

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

# Everything about a tree that a write can change: HEAD, its refs (a tag leaves
# HEAD and every file alone, so byte-identity of the worktree cannot see it),
# its config, its files, and — added after a review walked through them — the
# rest of what is inside .git. Refs and config were the first two things anyone
# thought of; a blob written straight into the object store, or a hook file
# dropped into .git/hooks by a shell redirection, changes neither, and both are
# writes into the tree being graded.
#
# `.git/index` is excluded because reading can rewrite it: `git status`
# refreshes stat information, so fingerprinting it would make this net fire on
# its own bookkeeping. Its content is covered from the other side — a staged
# change is exactly what `status --porcelain` reports.
dot_git_state() { # tree -> one record per entry under .git, whatever its type
  local t="$1"
  ( cd "$t/.git" 2>/dev/null || return 0
    # EVERY entry, not every file, and nothing excluded by name. The first
    # version of this digested `-type f`, skipped `*.lock`, and a review walked
    # three writes straight through it: a hook installed as a SYMLINK (which git
    # will run), an empty directory, and a leftover lock file. None of the three
    # is a regular file and one of them was excluded by its name. The same
    # question applies to the ONE exclusion that is still here, which is why it
    # is written as a path and not as a name.
    #
    # The lock exclusion had no reason to exist. This fingerprint is taken
    # before and after a COMPLETED run, so a transient lock is already gone;
    # what survives to be fingerprinted is a lock somebody left behind.
    #
    # `.git/index` is still excluded, and only that exact path: reading can
    # rewrite it (`git status` refreshes stat information), so digesting it would
    # make this net fire on its own bookkeeping. Its content is covered from the
    # other side — a staged change is exactly what `status --porcelain` reports.
    find . ! -path ./index -printf '%y %p\0' 2>/dev/null | sort -z | \
      while IFS= read -r -d '' rec; do
        local kind="${rec%% *}" path="${rec#* }"
        case "$kind" in
          # A regular file: its bytes. A symlink: where it points, because the
          # target's content is not what was written into the tree.
          f) printf '%s %s %s;' "$kind" "$path" \
               "$(sha256sum -- "$path" 2>/dev/null | cut -d' ' -f1)" ;;
          l) printf '%s %s ->%s;' "$kind" "$path" "$(readlink -- "$path" 2>/dev/null)" ;;
          *) printf '%s %s %s;' "$kind" "$path" "$(stat -c '%a:%s' -- "$path" 2>/dev/null)" ;;
        esac
      done )
}
# Everything in the worktree OUTSIDE .git, every entry, by kind. `git status
# --porcelain` is not this and cannot be: it collapses a wholly untracked
# directory to a single `?? dir/` line and never looks inside, and git cannot
# represent an empty directory at all. That is the region a gate would land in
# — the Worker's deliverable is untracked by construction, since the task
# branch's work is uncommitted, and the gate copy itself sits in an untracked
# `tests/` — so a gate that overwrote the deliverable, or left scratch files
# beside it, changed nothing porcelain reports. Same shape as dot_git_state,
# for the same reason: the question "what kind of thing is this" is not
# answered by the fact that it is under a directory git happens to summarize.
worktree_state() { # tree -> one record per entry outside .git, whatever its type
  local t="$1"
  ( cd "$t" 2>/dev/null || return 0
    # -prune, not a filter on the print: a path test alone would still walk
    # into .git and print every entry under it (./.git/config does not match
    # ./.git), which is dot_git_state's job and not this one's.
    find . -path ./.git -prune -o -printf '%y %p\0' 2>/dev/null | sort -z | \
      while IFS= read -r -d '' rec; do
        local kind="${rec%% *}" path="${rec#* }"
        case "$kind" in
          f) printf '%s %s %s;' "$kind" "$path" \
               "$(sha256sum -- "$path" 2>/dev/null | cut -d' ' -f1)" ;;
          l) printf '%s %s ->%s;' "$kind" "$path" "$(readlink -- "$path" 2>/dev/null)" ;;
          *) printf '%s %s %s;' "$kind" "$path" "$(stat -c '%a:%s' -- "$path" 2>/dev/null)" ;;
        esac
      done )
}
tree_state() { # tree -> state string
  local t="$1"
  printf '%s|%s|%s|%s|%s|%s' \
    "$(git -C "$t" rev-parse HEAD 2>/dev/null)" \
    "$(git -C "$t" for-each-ref --format='%(refname)=%(objectname)' 2>/dev/null | sort | tr '\n' ';')" \
    "$(sha256sum "$t/.git/config" 2>/dev/null | cut -d' ' -f1)" \
    "$(git -C "$t" status --porcelain 2>/dev/null | tr '\n' ';')" \
    "$(worktree_state "$t")" \
    "$(dot_git_state "$t")"
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
#
# The rule is containment, not equality, and the classification is a read-only
# ALLOWLIST rather than a list of destructive verbs. The first version did the
# opposite on both counts, and a reviewer walked straight through it: `git -C
# "$ROOT/tests" tag …`, `git --git-dir="$ROOT/.git" tag …`, `git -C "$ROOT"
# config --local …` and `git -C "$ROOT" update-ref …` each wrote into the tree
# under test while the judge printed "ok … acted on somewhere other than the
# tree". A list of verbs can only catch what someone thought of; the allowlist
# catches what nobody did.
#
# An allowlist still reads git's command line, though, and a second review
# showed six more spellings it read wrongly — including `--git-dir=.git`, whose
# relative path resolved against the judge's cwd instead of the command's. So
# the naming net is backed by the measuring net in judge_tree: whatever the
# command line looks like, a tree that changed is a tree that was written to.
# The naming net is what says WHO; the measuring net is what says WHAT HAPPENED.
mkdir -p "$WORK/logshim"
REAL_GIT="$(command -v git)"
cat > "$WORK/logshim/git" <<SH
#!/usr/bin/env python3
# A git that records what it was asked to do, then does it. The record is raw —
# the cwd, the subcommand, the paths the command could act on, and the
# arguments after the subcommand — because the judge, not the shim, owns the
# question of what that adds up to.
#
# One JSON object per line, not tab-joined fields: an argument containing a tab
# would otherwise move the boundary between two fields and let a command
# describe itself as something it is not. The judge treats a line it cannot
# parse as a violation for the same reason.
import json, os, sys

# Global options that take a separate value. The value is not a subcommand:
# reading \`git --work-tree X reset --hard\` as "subcommand X" is how a write
# walks past a judge that stops at the first non-dash token.
VALUE_OPTS = {"-C", "-c", "--git-dir", "--work-tree", "--namespace",
              "--config-env", "--exec-path", "--super-prefix", "--attr-source"}

args = sys.argv[1:]
cwd = os.getcwd()
git_dir = os.environ.get("GIT_DIR") or None
work_tree = os.environ.get("GIT_WORK_TREE") or None
sub, rest, i = None, [], 0
while i < len(args):
    a = args[i]
    key, eq, val = a.partition("=")
    if eq and key in VALUE_OPTS:
        if key == "--git-dir":
            git_dir = val
        elif key == "--work-tree":
            work_tree = val
        i += 1
        continue
    if a in VALUE_OPTS and i + 1 < len(args):
        v = args[i + 1]
        if a == "-C":
            # Relative to where git was already told to go, not to the cwd it
            # was started in: \`git -C a -C b\` acts on a/b.
            cwd = os.path.join(cwd, v)
        elif a == "--git-dir":
            git_dir = v
        elif a == "--work-tree":
            work_tree = v
        i += 2
        continue
    if a.startswith("-") and a != "-":
        i += 1
        continue
    sub, rest = a, args[i + 1:]
    break

# Every path the command could write to: its cwd (where a repo-scoped command
# lands), --git-dir and --work-tree when they move it, and for \`init\` the
# directory it names (a bare \`git init\` initialises the cwd). Relative paths
# are kept relative — the judge resolves them against the recorded cwd, so it
# cannot mistake "where the shim happened to be" for "where the command went".
targets = [cwd]
if sub == "init":
    named = [x for x in rest if not x.startswith("-")]
    targets = [named[-1] if named else cwd]
else:
    if git_dir:
        targets.append(git_dir)
    if work_tree:
        targets.append(work_tree)
    # The environment can name where a write lands just as --git-dir does, and
    # a command whose cwd is outside the tree has no other trace of it: the
    # third review wrote objects into the tree with GIT_OBJECT_DIRECTORY from
    # the scratch directory. Recorded, not resolved — the judge decides.
    for var in ("GIT_OBJECT_DIRECTORY", "GIT_COMMON_DIR", "GIT_INDEX_FILE"):
        v = os.environ.get(var)
        if v:
            targets.append(v)

with open(os.environ["GIT_LOG"], "a") as f:
    f.write(json.dumps({"cwd": cwd, "sub": sub or "", "targets": targets, "argv": rest}) + "\n")
os.execv("$REAL_GIT", ["$REAL_GIT"] + args)
SH
chmod +x "$WORK/logshim/git"

# The judge. Anything that is not known to read is treated as writing, and a
# write anywhere in the tree — the root, a subdirectory, its .git — is a
# violation. Deliberately conservative: a git command the gate grows later
# starts out distrusted, and the fix is to name it here, in the open.
cat > "$WORK/judge.py" <<'PY'
import json, os, sys

# Subcommands that only read, in every form.
READ_ONLY = {
    "rev-parse", "status", "ls-files", "ls-remote", "diff", "diff-tree",
    "log", "show", "show-ref", "cat-file", "for-each-ref", "rev-list",
    "merge-base", "describe", "check-ignore", "check-ref-format",
    "blame", "shortlog", "whatchanged", "name-rev", "verify-commit", "verify-tag",
    "is-inside-work-tree", "var", "version", "help",
    # Added after a third review reported them as false alarms: each is a read
    # in the form that reaches here. A net that accuses a correct gate is a net
    # someone weakens, which costs more than the accusations are worth.
    "diff-index", "ls-tree", "grep", "count-objects", "fsck", "verify-pack",
    "check-attr", "cherry", "merge-tree",
    # Not listed, though they are usually reads: `symbolic-ref` writes when it is
    # given a target, `config` when it is given a value, `branch`/`remote` when
    # they are given arguments, `stash` unless it is asked to list, and
    # `hash-object` unless it is given -w. A command that is not always a read is
    # not allowlisted wholesale — the FORM that writes has to be named to pass.
}
# `git config` reads with a key alone or with an explicit read flag, and writes
# with a value, --add, --unset, --edit and friends. --file is NOT a read flag:
# `config --file X key value` writes into X.
CONFIG_WRITE_FLAGS = {"--add", "--unset", "--unset-all", "--replace-all",
                      "--rename-section", "--remove-section", "--edit"}
CONFIG_READ_FLAGS = {"--get", "--get-all", "--get-regexp", "--get-urlmatch",
                     "--list", "-l"}
# Flags that take a separate value, so the value is not a positional argument.
VALUE_FLAGS = {"-f", "--file"}


def positionals(argv):
    out, i = [], 0
    while i < len(argv):
        a = argv[i]
        if a in VALUE_FLAGS and i + 1 < len(argv):
            i += 2
            continue
        if a.startswith("-") and a != "-":
            i += 1
            continue
        out.append(a)
        i += 1
    return out


def writes(sub, argv):
    if not sub:
        return False  # no subcommand: `git --version` and friends carry no repo
    pos = positionals(argv)
    if sub == "config":
        if any(f in argv for f in CONFIG_WRITE_FLAGS):
            return True
        if any(f in argv for f in CONFIG_READ_FLAGS):
            return False
        return len(pos) >= 2  # `config key` reads, `config key value` writes
    # Reads that are only reads in one form. Judging the FORM is what keeps a
    # correct gate from being accused: `branch --show-current`, `remote -v`,
    # `symbolic-ref HEAD`, `worktree list` and `tag -l` touch nothing, and a
    # judge that calls them writes is a judge that gets weakened the first time
    # a gate legitimately runs one.
    if sub == "branch":
        return len(pos) >= 1
    if sub == "symbolic-ref":
        return len(pos) >= 2
    if sub == "remote":
        return len(pos) >= 1 and pos[0] not in ("show", "get-url")
    if sub == "worktree":
        return len(pos) >= 1 and pos[0] != "list"
    if sub == "tag":
        return not (any(f in ("-l", "--list") for f in argv) or not pos)
    if sub == "stash":
        return not (pos and pos[0] in ("list", "show"))
    if sub == "hash-object":
        return "-w" in argv  # without -w it prints the hash and writes nothing
    # Two more that are reads in the form a gate uses and writes in the form it
    # does not. Both landed in .git, so the measuring net caught them anyway —
    # but "the other net will get it" is not a reason to let this one name a
    # write a read, and here it costs nothing: same shape as hash-object.
    if sub == "merge-tree":
        return "--write-tree" in argv  # writes the merged tree as an object
    if sub == "fsck":
        return "--lost-found" in argv  # writes .git/lost-found/
    return sub not in READ_ONLY


def inside(raw, cwd, tree):
    # Against the cwd the command ran in, never the judge's own: a relative
    # --git-dir=.git means the tree only if git was told to go there first.
    p = os.path.realpath(raw if os.path.isabs(raw) else os.path.join(cwd, raw))
    return p == tree or p.startswith(tree + os.sep)


log, tree = sys.argv[1], os.path.realpath(sys.argv[2])
if not os.path.exists(log):
    print("0\t0\tno log file was written")
    raise SystemExit(0)
bad, n = [], 0
for lineno, line in enumerate(open(log), 1):
    if not line.strip():
        continue
    try:
        rec = json.loads(line)
    except ValueError as e:
        # Not skipped: a record the judge cannot read is a record whose command
        # went unjudged, and "unjudged" must never be reported as "clean".
        bad.append("a log line could not be read (%s): %s" % (e, line[:80]))
        continue
    cwd = rec.get("cwd") or ""
    sub = rec.get("sub") or ""
    argv = rec.get("argv") or []
    if isinstance(argv, str):
        argv = argv.split()
    if not writes(sub, argv):
        continue
    n += 1
    for t in [x for x in (rec.get("targets") or []) if x]:
        if inside(t, cwd, tree):
            bad.append("%s acting on %s (cwd %s)" % (sub or "?", t, cwd))
            break
print("%d\t%d\t%s" % (n, len(bad), "; ".join(bad)))
PY

judge_tree() { # tree -> "n_mutating \t n_parse_bad \t measured \t violations"
  local tree="$1" before after parsed rest n_mut n_parse measured violations delta
  rm -f "$WORK/gitlog"
  before="$(tree_state "$tree")"
  run_gate "$tree" "PATH=$WORK/logshim:$PATH" "GIT_LOG=$WORK/gitlog" >/dev/null 2>&1
  after="$(tree_state "$tree")"
  parsed="$(python3 "$WORK/judge.py" "$WORK/gitlog" "$tree")"
  n_mut="${parsed%%$'\t'*}"; rest="${parsed#*$'\t'}"
  n_parse="${rest%%$'\t'*}"; violations="${rest#*$'\t'}"
  # The measuring net. The judge above reasons about what a command WAS; this
  # asks what happened to the tree, so a spelling nobody has thought of yet
  # still fails the run. Both are reported: one names the offender, the other
  # is the guarantee, and the self-check below requires each to stand alone.
  measured=0
  if [[ "$before" != "$after" ]]; then
    measured=1
    # Split on both separators: one field per ref, one per file inside .git, so
    # the report names what changed rather than printing two whole states.
    delta="$(diff <(printf '%s' "$before" | tr '|;' '\n\n') \
                  <(printf '%s' "$after" | tr '|;' '\n\n') | grep '^[<>]' | head -12 | tr '\n' ' ')"
    violations="${violations:+$violations; }the tree itself changed during the run: $delta"
  fi
  printf '%s\t%s\t%s\t%s\n' "$n_mut" "$n_parse" "$measured" "$violations"
}

TREE="$WORK/tree-d"
new_tree "$TREE"
BEFORE="$(tree_state "$TREE")"
JUDGE="$(judge_tree "$TREE")"
AFTER="$(tree_state "$TREE")"
[[ "$BEFORE" == "$AFTER" ]] \
  && ok "the logged run left the tree byte-identical" \
  || fail "the logged run changed the tree it graded: [$BEFORE] -> [$AFTER]"

N_MUTATING="${JUDGE%%$'\t'*}"; REST="${JUDGE#*$'\t'}"
N_PARSE="${REST%%$'\t'*}"; REST="${REST#*$'\t'}"
MEASURED="${REST%%$'\t'*}"; VIOLATIONS="${REST#*$'\t'}"
N_BAD=$(( N_PARSE + MEASURED ))
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

# Each net is only worth as much as its reach. Every case below is a way of
# writing into the tree under test that an earlier version called clean — the
# spellings two reviewers used to walk past it. They are run against a
# deliberately broken copy of the gate, because the nets are being tested, not
# the gate.
#
# In $WORK, not a temp directory of its own: `mktemp -d` here meant a directory
# outside the one cleanup() removes, so every run of this test leaked a copy of
# the gate for the life of the host.
BROKEN="$WORK/broken"
mkdir -p "$BROKEN"
python3 - "$GATE" "$BROKEN/gate.sh" "$BROKEN/gate-shell.sh" "$BROKEN/gate-worktree.sh" <<'PY'
import sys
src = open(sys.argv[1]).read()
out, shell_out, worktree_out = sys.argv[2], sys.argv[3], sys.argv[4]
probe = '# a probe the nets must not miss\n'
cases = [
    # The first seven: a `-C` into a subdirectory, an absolute --git-dir, a
    # ref write that leaves HEAD and every file alone, a config write, a
    # symbolic-ref WITH a target, an index write, a commit.
    'git -C "$ROOT/tests" tag g3-escape-tag\n',
    'git --git-dir="$ROOT/.git" tag g3-escape-tag\n',
    'git -C "$ROOT" update-ref refs/g3-escape HEAD\n',
    'git -C "$ROOT" config --local g3.escape 1\n',
    'git -C "$ROOT" symbolic-ref HEAD refs/heads/g3-escape\n',
    'git -C "$ROOT" add -A\n',
    'git --git-dir="$ROOT/.git" commit -q -m g3-escape\n',
    # The six an adversarial review added, every one of which the naming net
    # used to call clean: a RELATIVE --git-dir (resolved against the wrong
    # directory), a second -C that is a no-op the parse never reached, a
    # --work-tree that rewrites the files themselves, a global option whose
    # VALUE was read as the subcommand, a config write through --file, and a
    # GIT_DIR that never appears in the argument vector at all.
    'git -C "$ROOT" --git-dir=.git tag g3-escape-tag\n',
    'git -C "$ROOT" -C . tag g3-escape-tag\n',
    'git --work-tree "$ROOT" reset --hard\n',
    'git --namespace g3 -C "$ROOT" tag g3-escape-tag\n',
    'git -C "$ROOT" config --file "$ROOT/.git/config" g3.escape 1\n',
    'GIT_DIR="$ROOT/.git" git tag g3-escape-tag\n',
    # And the other half of the form question: commands that read in one form
    # and write in another. Each of these is the WRITE form, and the judge has
    # to tell it apart from `branch --show-current`, `remote -v` and `worktree
    # list` — which the read-only check below requires it NOT to accuse.
    'git -C "$ROOT" branch g3-escape-branch\n',
    'git -C "$ROOT" remote add probe "$ROOT"\n',
    'git -C "$ROOT" worktree add "$WORK/g3-escape-wt"\n',
    # A relative path is only relative to somewhere: here the cwd is OUTSIDE
    # the tree and the path back into it is relative, so nothing but resolving
    # it against the cwd the command ran in can see where it lands. The other
    # relative spelling above is caught by its cwd alone, which is why this one
    # exists — a defence with no probe that isolates it is a defence nobody
    # would notice losing.
    'git -C "$(dirname "$ROOT")" --git-dir="$(basename "$ROOT")/.git" tag g3-escape-tag\n',
    # The third review's: a write that names its destination through the
    # ENVIRONMENT rather than the argument vector. The gate is in the scratch
    # repo when this runs, so the cwd says nothing; only the recorded
    # GIT_OBJECT_DIRECTORY names the tree, which is why the shim records it.
    # (The companion case — a write that runs no command at all — is the
    # shell-only gate below, and only the fingerprint can see that one.)
    'GIT_OBJECT_DIRECTORY="$ROOT/.git/objects" git hash-object -w README.md\n',
    # The fourth review's: two more commands that read in the form a gate uses
    # and write in another, and that the judge called reads in BOTH forms. The
    # tree is fingerprinted now, so the measuring net would have reported the
    # object either way — but "the other net will get it" is not a reason to
    # let this one name a write a read, and the reads case below pins the
    # opposite direction for the same two names.
    #
    # `merge-tree --write-tree` writes the merged tree as an object. The plain
    # form prints it.
    'git -C "$ROOT" merge-tree --write-tree HEAD HEAD\n',
    # `fsck --lost-found` writes .git/lost-found/. It writes only when there is
    # something to lose — and here there is, because the hash-object probe
    # above has just left a dangling blob behind; the reported delta names
    # `d ./lost-found`. Before this probe existed the judge called the form a
    # read, which is the finding it answers.
    'git -C "$ROOT" fsck --lost-found\n',
]
# Far enough in that ROOT is set and the environment has been unset (the probes
# must be run through the shim, not neutralised by the gate's own hygiene), and
# after the tree's state has been captured, so the gate goes on to report the
# change rather than exiting at the setup.
anchor = "# --- two-layer main protection"
assert src.count(anchor) == 1, "the insertion point moved: %d matches" % src.count(anchor)
body = "".join(
    p + c for p, c in zip(
        [probe + "# case %d\n" % i for i in range(len(cases))], cases))
open(out, "w").write(src.replace(anchor, body + anchor, 1))

# The same broken gate with FIVE probes, none of them a git command: writes that
# run no command at all, so the naming net has nothing to read — if the
# fingerprint does not see them, they are invisible to the guard.
#
# One is a shell redirection into the tree's own .git. Three are the shapes a
# review found after the fingerprint digested only regular files: a hook
# installed as a SYMLINK (git runs it — this is a functional change to the
# gate's environment, not litter), a leftover `.lock`, and an empty directory.
# They are here because "which kind of thing did the write create" is the same
# class of question as "how was the command spelled", and a net that answers it
# for one kind of file is a net with a spelling problem. The fifth closes the
# same question one level down, about the one exclusion the net keeps.
shell_probes = [
    'printf "#!/bin/sh\\n" > "$ROOT/.git/hooks/commit-msg"\n',
    'ln -sf /bin/true "$ROOT/.git/hooks/pre-commit"\n',
    ': > "$ROOT/.git/g3-leftover.lock"\n',
    'mkdir -p "$ROOT/.git/g3-empty-dir"\n',
    # And the exclusion that survives: `.git/index` is skipped because READING
    # can rewrite it. It is skipped by PATH — this write is a file that merely
    # shares the name, and a rule that skipped `-name index` would have let it
    # through. The one exclusion left in the net is the one this probe pins.
    ': > "$ROOT/.git/refs/index"\n',
]
open(shell_out, "w").write(src.replace(anchor, probe + "".join(shell_probes) + anchor, 1))

# And three more of the same kind OUTSIDE .git, where the fingerprint looked
# only through `git status --porcelain`. Porcelain collapses a wholly untracked
# directory to one `?? dir/` line and never looks inside it, and git cannot
# represent an empty directory at all — so the region the gate actually lands
# in, the one holding the Worker's uncommitted deliverable and the gate's own
# copy, was the region nothing digested. A gate that overwrote the deliverable
# or littered its directory changed nothing this net could see, and the suite
# said the tree was untouched.
worktree_probes = [
    # The deliverable itself: an untracked file, so overwriting it is invisible
    # to porcelain.
    'printf "the gate overwrote the deliverable\\n" > "$ROOT/internal/config/deliverable.txt"\n',
    # A new file inside an untracked directory: visible to `-uall` as an entry,
    # invisible to plain porcelain, and its CONTENT invisible either way.
    'printf "x\\n" > "$ROOT/tests/acceptance/g3-extra.sh"\n',
    # A directory git cannot represent at all, in the tree's root.
    'mkdir -p "$ROOT/g3-empty-root"\n',
]
open(worktree_out, "w").write(src.replace(anchor, probe + "".join(worktree_probes) + anchor, 1))
PY
chmod +x "$BROKEN/gate.sh" "$BROKEN/gate-shell.sh" "$BROKEN/gate-worktree.sh"
# The mutations have to be in the tree under test for the nets to have
# something to find, so the gate is copied into a tree of its own.
TREE="$WORK/tree-e"
new_tree "$TREE"
cp "$BROKEN/gate.sh" "$TREE/tests/acceptance/gitea-real-services-e2e.sh"
ESCAPED="$(judge_tree "$TREE")"
E_MUT="${ESCAPED%%$'\t'}"; E_REST="${ESCAPED#*$'\t'}"
E_PARSE="${E_REST%%$'\t'*}"; E_REST="${E_REST#*$'\t'}"
E_MEASURED="${E_REST%%$'\t'*}"
if (( E_PARSE == 20 )); then
  ok "the naming net names all 20 spellings of a write into the tree under test, and accuses nothing else"
else
  fail "the naming net named $E_PARSE writes into the tree under test, not 20 — it judges the spelling, not the target: $ESCAPED"
fi
# The measuring net has to stand on its own: a spelling the naming net cannot
# parse must still be visible as a changed tree, or the naming net's blind
# spots are the guard's blind spots.
if (( E_MEASURED == 1 )); then
  ok "the measuring net saw the tree change without needing to parse the command"
else
  fail "20 writes into the tree under test left the tree's own state unchanged — the net that cannot be spelled past is not connected: $ESCAPED"
fi

# And it has to stand alone in the other direction too: here is a write that
# runs no git command, so there is no log line for any parse to read. If the
# fingerprint does not see it, the guard does not see it — the two nets are
# only independent if one of them can catch what the other provably cannot.
TREE="$WORK/tree-g"
new_tree "$TREE"
cp "$BROKEN/gate-shell.sh" "$TREE/tests/acceptance/gitea-real-services-e2e.sh"
SHELLONLY="$(judge_tree "$TREE")"
S_MUT="${SHELLONLY%%$'\t'}"; S_REST="${SHELLONLY#*$'\t'}"
S_PARSE="${S_REST%%$'\t'*}"; S_REST="${S_REST#*$'\t'}"
S_MEASURED="${S_REST%%$'\t'*}"
if (( S_MEASURED == 1 )); then
  ok "the measuring net saw the tree change with no command logged to parse"
else
  fail "5 shell-level writes went into the tree's own .git and the measuring net saw nothing: $SHELLONLY"
fi
# One write being visible is not the claim, and "the tree differs" does not say
# what was seen. The fingerprint digested regular files only until a review put
# a SYMLINKED HOOK, an empty directory and a leftover `.lock` past it — and the
# third of those is a regular file, which is why the exclusion that skipped it
# is part of the same finding rather than a separate one. So the assertion is
# that the report names all five, by kind and by path. A net that sees one kind
# of file is a net with a spelling problem, and so is one that skips by name
# what it meant to skip by path.
S_DETAIL="${S_REST#*$'\t'}"
unseen=""
for shape in "hooks/commit-msg" "hooks/pre-commit ->/bin/true" "g3-leftover.lock" \
             "g3-empty-dir" "refs/index"; do
  case "$S_DETAIL" in
    *"$shape"*) ;;
    *) unseen="$unseen [$shape]" ;;
  esac
done
if [[ -z "$unseen" ]]; then
  ok "the measuring net named all five: two that are not regular files, the one it skipped for its name, and the one that only shares a name with the index"
else
  fail "the measuring net did not report:$unseen — what it digests is a spelling too, and this one excluded symlinks, directories and anything named *.lock: $SHELLONLY"
fi
if (( S_PARSE == 0 )); then
  ok "no command was logged for any of them, so the naming net was blind to them by construction"
else
  fail "the naming net claims to have judged a write that ran no command at all: $SHELLONLY"
fi

# The same question one region out. The five above are all inside .git; these
# three are in the worktree, which the net reached only through porcelain — and
# porcelain is not a fingerprint: it summarizes a wholly untracked directory in
# one line, and sees no directory git cannot represent. This is the region the
# gate lands in, since the Worker's deliverable and the gate's own copy are both
# untracked, so a write here was the most likely one and the least visible.
TREE="$WORK/tree-h"
new_tree "$TREE"
cp "$BROKEN/gate-worktree.sh" "$TREE/tests/acceptance/gitea-real-services-e2e.sh"
WORKTREEONLY="$(judge_tree "$TREE")"
W_MUT="${WORKTREEONLY%%$'\t'}"; W_REST="${WORKTREEONLY#*$'\t'}"
W_PARSE="${W_REST%%$'\t'*}"; W_REST="${W_REST#*$'\t'}"
W_MEASURED="${W_REST%%$'\t'*}"; W_DETAIL="${W_REST#*$'\t'}"
if (( W_MEASURED == 1 )); then
  ok "the measuring net saw writes in the worktree, not only inside .git"
else
  fail "3 shell-level writes went into the worktree outside .git and the measuring net saw nothing: $WORKTREEONLY"
fi
w_unseen=""
for shape in "internal/config/deliverable.txt" "tests/acceptance/g3-extra.sh" "g3-empty-root"; do
  case "$W_DETAIL" in
    *"$shape"*) ;;
    *) w_unseen="$w_unseen [$shape]" ;;
  esac
done
if [[ -z "$w_unseen" ]]; then
  ok "the measuring net named all three: the overwritten untracked deliverable, the file left in an untracked directory, and the empty directory"
else
  fail "the measuring net did not report:$w_unseen — it reached the worktree through 'git status --porcelain', which summarizes an untracked directory instead of reading it and cannot see an empty one: $WORKTREEONLY"
fi
if (( W_PARSE == 0 )); then
  ok "no command was logged for any of them either, so only the fingerprint could have seen them"
else
  fail "the naming net claims to have judged a worktree write that ran no command at all: $WORKTREEONLY"
fi

# The other half of a naming net's worth: one that accuses a correct gate is a
# net someone will cut, and the accusation reads as evidence. These are the
# read-only forms a later change to the gate could legitimately use, and every
# one of them was reported as a write by a version of the judge — a false alarm
# is how the next person concludes the check is broken.
READS="$WORK/reads.log"
python3 - "$TREE" "$READS" <<'PY'
import json, sys
tree, out = sys.argv[1], sys.argv[2]
lines = [
    "config user.name", "config --get user.name", "branch --show-current",
    "remote -v", "symbolic-ref HEAD", "worktree list", "tag -l",
    "status --porcelain", "rev-parse HEAD", "ls-files", "log --oneline -3",
    "diff --stat",
    # Reported by the third review as false alarms.
    "diff-index --quiet HEAD", "ls-tree HEAD", "grep -l x", "count-objects -v",
    "stash list", "hash-object README.md",
]
with open(out, "w") as f:
    for line in lines:
        sub, _, rest = line.partition(" ")
        f.write(json.dumps({"cwd": tree, "sub": sub, "targets": [tree],
                            "argv": rest.split()}) + "\n")
PY
FALSE_ALARMS="$(python3 "$WORK/judge.py" "$READS" "$TREE" | cut -f2)"
if (( FALSE_ALARMS == 0 )); then
  ok "the naming net does not accuse 18 read-only forms of writing in the tree"
else
  fail "the naming net accuses $FALSE_ALARMS of 18 read-only forms of writing in the tree: $(python3 "$WORK/judge.py" "$READS" "$TREE" | cut -f3)"
fi

# A record the judge cannot read is not a record it judged. The log is JSON so
# that no argument can forge a field; the price is that a format mismatch
# between the shim and the judge would leave every command unread — and every
# command unread must not read as every command clean.
BADLOG="$WORK/badlog"
printf 'this line is not a record\n' > "$BADLOG"
UNREADABLE="$(python3 "$WORK/judge.py" "$BADLOG" "$TREE")"
U_REST="${UNREADABLE#*$'\t'*}"; U_VIOLATIONS="${U_REST#*$'\t'*}"
case "$U_VIOLATIONS" in
  *"could not be read"*) ok "a record the judge cannot read is reported, not skipped" ;;
  *) fail "an unreadable record was passed over in silence: $UNREADABLE" ;;
esac

# --- 5. git's environment cannot redirect the gate into the tree -------------
# GIT_DIR outranks the working directory and is inherited: with it set, a
# guarded `git init "$WORK/work"` exits 0 having created nothing, and the probe
# commit lands in the tree under test — the L1-20260913-16 harm, before any
# guard can refuse it. The end-of-run check would notice, after the commit
# already sat on the Worker's branch.
TREE="$WORK/tree-f"
new_tree "$TREE"
BEFORE="$(tree_state "$TREE")"
run_gate "$TREE" "GIT_DIR=$TREE/.git"
if (( RC == 0 )); then
  fail "the script passed with GIT_DIR pointing at the tree it grades"
else
  ok "a redirected git environment does not make the run succeed (exit $RC)"
fi
AFTER="$(tree_state "$TREE")"
[[ "$BEFORE" == "$AFTER" ]] \
  && ok "GIT_DIR did not let the run write in the tree under test" \
  || fail "GIT_DIR redirected the run into the tree under test: [$BEFORE] -> [$AFTER]"
if git -C "$TREE" log --format=%s -3 2>/dev/null | grep -q 'g3 probe\|g3 should be refused'; then
  fail "a probe commit landed in the tree under test under GIT_DIR: $(git -C "$TREE" log --format='%h %an %s' -1)"
else
  ok "no probe commit was made in the tree under test"
fi

printf '\n'
if (( FAILS )); then
  printf 'gitea-real-services guard unit tests: %d failure(s)\n' "$FAILS"
  exit 1
fi
printf 'gitea-real-services guard unit tests: all checks passed\n'
