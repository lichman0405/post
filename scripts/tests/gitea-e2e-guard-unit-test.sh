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
#      config, every entry in the worktree, every entry in the gitdir and in
#      the common dir, by kind, with each regular file's mode, timestamp and
#      bytes) before and after the
#      run — it needs no grammar, so nothing can be spelled past it, but it
#      cannot say who did it, and "nothing can be spelled past it" is a claim
#      about what the record HOLDS: bytes were not enough for a `chmod`, which
#      the sixth review walked through, and the record is now the bytes, the
#      mode and the timestamp. What it still cannot see is a write that leaves
#      all of them identical — and whatever is inside a path whose mode denies
#      reading, which the record names out loud as `unreadable` instead of
#      carrying an empty field that reads as "no bytes". The run fails if either
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
  # A tracked file whose name starts with `.git` but is not `.git` — the
  # boundary of the one exclusion the worktree net keeps. It is written as a
  # PATH (`-path ./.git`), and a version of it written as a GLOB (`-path
  # './.git*'`) would drop this file from the record in silence. Being TRACKED
  # is the harder half of it: `git status --porcelain` names the file as well
  # (` M .gitignore`), so the assertion that tells the two spellings apart has
  # to name the worktree walk rather than search the merged delta for the path.
  # It searched the merged delta, and the glob spelling passed the whole suite
  # — found by the mutation battery, which reported its case MISSED, and not by
  # a review: the two nets were both right and the assertion reading them was
  # not.
  printf 'x\n' > "$d/.gitignore"
  git -C "$d" add -A
  git -C "$d" -c user.name=t -c user.email=t@e commit -q -m base
  printf 'worker deliverable\n' > "$d/internal/config/deliverable.txt"
  # Two untracked files whose only job is to be the TARGET of a metadata-only
  # write. Every content probe has a target of its own, and a chmod sharing a
  # target with a content write would be caught by the other probe's bytes —
  # the shape being pinned is "the bytes did not change and the write is still
  # real", so the bytes must be the thing that does not change.
  printf 'nothing writes these bytes\n' > "$d/internal/config/probe-mode.txt"
  printf 'nor these\n' > "$d/internal/config/probe-time.txt"
  chmod 644 "$d/internal/config/probe-mode.txt" "$d/internal/config/probe-time.txt"
  # A directory and two symlinks, for the fields the seventh review showed were
  # IN the record and pinned by nothing: a probe that only CREATES an entry is
  # named by the entry appearing, so a later refactor could drop what the record
  # says about an entry's kind — a directory's mode, a symlink's target, a
  # symlink's own time — and the whole suite stayed green while the net lost a
  # class of write. These are the targets of writes that change only those
  # fields: a chmod with no new entry, a re-pointed link, a `touch -h`.
  mkdir -p "$d/internal/config/probe-dir"
  ln -s deliverable.txt "$d/internal/config/probe-link"
  ln -s deliverable.txt "$d/internal/config/probe-link2"
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
# The graded tree's OWN index is the one path excluded, and it is excluded by
# RESOLVED PATH rather than by name. Reading rewrites it (`git status` refreshes
# stat information), so recording it makes this net fire on its own bookkeeping;
# but in a LINKED worktree — the shape the G3 gate actually runs in — that index
# is not the entry called `./index` in either walk: it lives in the COMMON dir,
# at `./worktrees/<id>/index`. Excluding the literal `./index` therefore left
# the net recording the one file every read rewrites, and it fired on its own
# `git status`: the fingerprint differed from itself with nothing in between, in
# the linked shape only, measured 10 of 10 runs. `git rev-parse --git-path index`
# says where the file is, whatever layout produced the worktree.
#
# Its CONTENT is covered from the other side — a staged change is exactly what
# `status --porcelain` reports — and its FLAGS, which porcelain does not report,
# are covered by the `ls-files -v` field of tree_state below.
file_bytes() { # path -> the digest of its bytes, or `unreadable` if it has none
  # An empty field here is a claim: "these are the bytes, and they are nothing".
  # A file this process cannot open is not a file with no bytes — it is a file
  # whose bytes nobody measured — and the difference decides what a later
  # equality means. A Worker's `chmod 000` deliverable was recorded as
  # `… 0@<time>@` with the digest missing, so a rewrite of it, with mode and
  # time restored, compared equal. (It still does: see the header — a write that
  # leaves every field identical is beyond any fingerprint.) What changes is
  # that the record now says WHICH of the two it is instead of looking like a
  # digest of nothing.
  local d
  if d="$(sha256sum -- "$1" 2>/dev/null)"; then printf '%s' "${d%% *}"
  else printf 'unreadable'; fi
}
record_of() { # state label kind path -> the ONE record for that path, or nothing
  # Every claim about a record has to be about that record alone. A `case`
  # pattern is matched against the whole state string, so
  # `*"<path>"*unreadable*` is also satisfied by any OTHER record that says
  # `unreadable` — the round-8 mutation battery caught exactly that: a
  # directory record stripped of its `:unreadable` still passed, because the
  # file record beside it carried the word. Splitting the state back into
  # records and keeping the one for the path makes the claim about it. The
  # records are `;`-separated inside a section and the sections are
  # `|`-separated, so both become line breaks and the label, kind and path
  # select the one line. All three, because a record is `label kind path …`:
  # selecting on the label alone hands back every record in the walk, and
  # selecting on the label and kind hands back every record of that kind —
  # which is the round-8 leak again, one narrowing later. The ninth review
  # found it that way: the assertion that a mode-000 file is marked was
  # satisfied by a sibling file's record, so the file's own `unreadable`
  # could have been dropped and the suite would not have noticed.
  printf '%s' "$1" | tr '|;' '\n\n' | grep -F "$2 $3 $4 " || true
}
dot_git_walk() { # git dir, label, resolved path to exclude (the tree's own index)
  # The label is not decoration. Every record here is a path RELATIVE to the
  # directory walked (`./hooks/pre-commit`), and so is every record the
  # worktree walk makes; a diff of two states therefore showed `d .` for the
  # tree's root AND for the git dir, and the report could not say which. A
  # linked worktree walks THREE directories, two of which print the same
  # `./objects/…` paths as each other. Prefixed, a delta line names its tree.
  local gd="$1" label="$2" excl="${3:-}"
  # Resolved before anything is compared against it: the path the caller has and
  # the path `find` prints are only comparable as resolved paths.
  gd="$(cd -- "$gd" 2>/dev/null && pwd)" || return 0
  ( cd "$gd" || return 0
    # EVERY entry, not every file, and nothing excluded by name. The first
    # version of this digested `-type f`, skipped `*.lock`, and a review walked
    # three writes straight through it: a hook installed as a SYMLINK (which git
    # will run), an empty directory, and a leftover lock file. None of the three
    # is a regular file and one of them was excluded by its name. The same
    # question applies to the ONE exclusion that is still here, which is why the
    # file is matched by its resolved path and not by its name.
    #
    # The lock exclusion had no reason to exist. This fingerprint is taken
    # before and after a COMPLETED run, so a transient lock is already gone;
    # what survives to be fingerprinted is a lock somebody left behind.
    find . -printf '%y %p\0' 2>/dev/null | sort -z | \
      while IFS= read -r -d '' rec; do
        local kind="${rec%% *}" path="${rec#* }"
        # The exclusion, on the resolved path: see file_bytes' neighbour above
        # for the measured failure that put a path here where a name used to be.
        [[ -n "$excl" && "$(realpath -m -- "$path" 2>/dev/null)" == "$excl" ]] && continue
        case "$kind" in
          # A regular file: its bytes AND its mode and timestamp. Bytes alone
          # was the sixth review's shape of a net with a spelling problem:
          # `chmod` and
          # `touch` write neither a byte nor an entry, so a gate that made the
          # Worker's deliverable read-only, or removed the exec bit from a
          # script, changed nothing the record held and the run reported "the
          # tree under test is as this script found it".
          f) printf '%s %s %s %s@%s;' "$label" "$kind" "$path" "$(stat -c '%a@%y' -- "$path" 2>/dev/null)" \
               "$(file_bytes "$path")" ;;
          # A symlink: where it points, because the target's content is not what
          # was written into the tree — and when it was made, for the same
          # reason as the timestamp above.
          l) printf '%s %s %s ->%s@%s;' "$label" "$kind" "$path" "$(readlink -- "$path" 2>/dev/null)" \
               "$(stat -c '%y' -- "$path" 2>/dev/null)" ;;
          # Everything else — directories here, and whatever a filesystem can
          # hold that is neither a file nor a link. Mode and size, but NOT the
          # timestamp, and that omission is measured rather than preferred:
          # `git status` alone moves the git dir's mtime (it writes
          # `.git/index.lock` and renames it over `.git/index`), and the index
          # is excluded from this walk for exactly that reason — reading
          # rewrites it. A directory's mtime is a summary of its ENTRIES, and
          # every entry is recorded here in its own right, with bytes, mode and
          # time; keeping the summary as well bought nothing and made the net
          # fire on the gate's own reads, which is a net somebody would cut.
          # The mode stays: `chmod 000` on the directory holding the
          # deliverable is a write that changes no entry and no byte.
          #
          # A directory this process cannot search is marked, for the reason
          # file_bytes marks an unreadable file: `d ./secret 0:4096` reads as an
          # empty directory of that mode, and it is not one — it is a directory
          # whose contents nobody measured.
          *) printf '%s %s %s %s%s;' "$label" "$kind" "$path" "$(stat -c '%a:%s' -- "$path" 2>/dev/null)" \
               "$([[ -r "$path" && -x "$path" ]] || printf ':unreadable')" ;;
        esac
      done )
}
dot_git_state() { # tree -> one record per entry under .git, whatever its type
  local t="$1" gd common excl=""
  # Where the graded tree's own index is, for the walks below to skip — computed
  # from git's own answer rather than from a rule about where indexes live.
  excl="$(git -C "$t" rev-parse --git-path index 2>/dev/null)" || excl=""
  if [[ -n "$excl" ]]; then
    [[ "$excl" == /* ]] || excl="$t/$excl"
    excl="$(realpath -m -- "$excl" 2>/dev/null)" || excl=""
  fi
  if [[ -f "$t/.git" ]]; then
    # The shape the gate ACTUALLY runs in: this tree is a linked worktree, so
    # `.git` is a FILE naming the real gitdir. `cd "$t/.git"` fails there — and
    # this function returned 0 with an EMPTY record, while `worktree_state`
    # prunes `./.git` whether it is a directory or a file, so the gitfile was in
    # neither net. A net that measures nothing compares equal to itself forever,
    # and the tree it silently stopped measuring is the tree being graded. The
    # gitfile is recorded, and BOTH directories it stands between are walked:
    # the worktree's own gitdir (its HEAD) and the common dir (objects, refs,
    # hooks) — the second is where a blob or a hook lands.
    #
    # Its mode and timestamp are recorded too, not only its digest: a `touch` on
    # the gitfile changes no byte, and the record used to hold the bytes alone —
    # so the file that says which gitdir this tree is was the one entry in it a
    # write could reach without moving the fingerprint.
    printf 'gitfile %s %s@%s;' "$t/.git" "$(stat -c '%a@%y' -- "$t/.git" 2>/dev/null)" \
           "$(file_bytes "$t/.git")"
    gd="$(git -C "$t" rev-parse --git-dir 2>/dev/null)" || gd=""
    # rev-parse answers relative to the cwd it ran in, and `-C` makes that the
    # tree; resolve it here rather than handing a relative path to `cd`.
    [[ "$gd" == /* ]] || gd="$t/$gd"
    gd="$(cd "$gd" 2>/dev/null && pwd)" || gd=""
    if [[ -n "$gd" ]]; then
      dot_git_walk "$gd" gitdir "$excl"
      common="$(git -C "$t" rev-parse --git-common-dir 2>/dev/null)" || common=""
      [[ "$common" == /* ]] || common="$t/$common"
      common="$(cd "$common" 2>/dev/null && pwd)" || common=""
      if [[ -n "$common" && "$common" != "$gd" ]]; then
        dot_git_walk "$common" common "$excl"
      fi
    fi
    return 0
  fi
  if [[ ! -d "$t/.git" ]]; then
    # Neither a directory nor a gitfile: there is nothing here to fingerprint,
    # and that must not read as "nothing changed". The record is a marker, so
    # two trees that both lack a git dir still compare equal while a tree that
    # GAINS one does not.
    printf 'no-git-dir %s;' "$t"
    return 0
  fi
  dot_git_walk "$t/.git" gitdir "$excl"
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
#
# Every record carries a label, for the reason dot_git_walk's does and one more.
# The first is that both walks print `./`-relative paths, so a delta line has to
# say which walk produced it. The second is that the whole state is ONE string
# per tree — HEAD, refs, config, porcelain, this walk, the git dirs — and it is
# diffed as a whole, so a delta line naming a path does not say WHICH net named
# it, and porcelain is one of the nets in that string. An assertion that
# searched the delta for a path was therefore satisfied by ` M .gitignore` from
# `git status` while this walk had dropped the file, and the mutation spelling
# the exclusion `-path './.git*'` passed the entire suite. Label, then kind,
# then path, lets the assertion require the walk that has to do the naming.
#
# A literal label and not a parameter: dot_git_walk takes one because it walks
# two directories, and there is exactly one worktree walk.
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
          # Bytes AND mode AND timestamp, for the reason spelled out in
          # dot_git_walk: `chmod` and `touch` write none of git's bookkeeping,
          # no new entry and not one byte, so a record holding only the digest
          # says a tree is untouched after a gate has made the Worker's
          # deliverable read-only or taken the exec bit off a script. The
          # digest comes from file_bytes, so a file this process cannot open is
          # recorded as unreadable rather than as a file with no bytes.
          f) printf 'worktree %s %s %s@%s;' "$kind" "$path" "$(stat -c '%a@%y' -- "$path" 2>/dev/null)" \
               "$(file_bytes "$path")" ;;
          l) printf 'worktree %s %s ->%s@%s;' "$kind" "$path" "$(readlink -- "$path" 2>/dev/null)" \
               "$(stat -c '%y' -- "$path" 2>/dev/null)" ;;
          # Mode and size, no timestamp — same rule and same reason as the git
          # walk above. What it costs is a write that creates and removes an
          # entry without leaving one behind: the tree IS as it was found, and
          # this net says so, which is the claim it exists to make. A directory
          # this process cannot search is marked, for the reason file_bytes
          # marks a file it cannot read.
          *) printf 'worktree %s %s %s%s;' "$kind" "$path" "$(stat -c '%a:%s' -- "$path" 2>/dev/null)" \
               "$([[ -r "$path" && -x "$path" ]] || printf ':unreadable')" ;;
        esac
      done )
}
# The state string: HEAD, every ref, the config, porcelain, the index FLAGS,
# the worktree walk, the git dirs. The flags field is not porcelain's job:
# `git update-index --assume-unchanged` (and `--skip-worktree`) makes every later
# `git status` and `git diff` in that tree stop mentioning the file, and changes
# no byte, no mode, no entry and no blob — a gate that hid the Worker's
# deliverable from the Supervisor's inspection left this fingerprint equal. It is
# the one write in the index whose content is not read from anywhere else, which
# is why the index is excluded from the walks but named here. `ls-files -v`
# prints the flag letter per entry: `H` normal, `h` assume-unchanged, `S`
# skip-worktree.
tree_state() { # tree -> state string
  local t="$1"
  printf '%s|%s|%s|%s|%s|%s|%s' \
    "$(git -C "$t" rev-parse HEAD 2>/dev/null)" \
    "$(git -C "$t" for-each-ref --format='%(refname)=%(objectname)' 2>/dev/null | sort | tr '\n' ';')" \
    "$(sha256sum "$t/.git/config" 2>/dev/null | cut -d' ' -f1)" \
    "$(git -C "$t" status --porcelain 2>/dev/null | tr '\n' ';')" \
    "$(git -C "$t" ls-files -v 2>/dev/null | tr '\n' ';')" \
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

judge_tree() { # tree -> "n_mutating \t n_parse_bad \t measured \t added \t violations"
  local tree="$1" before after parsed rest n_mut n_parse measured violations delta added
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
  added=""
  if [[ "$before" != "$after" ]]; then
    measured=1
    # Split on both separators: one field per ref, one per file inside .git, so
    # the report names what changed rather than printing two whole states.
    #
    # Every line, not the first twelve. The cap was there to keep a failure
    # message short, and it made the report a summary: a run with six writes
    # printed the first twelve lines and the assertion that reads this detail —
    # "the net named every shape" — failed on a shape the net HAD named. A
    # truncated list of what changed is read as "these are the things that
    # changed", which is how an instrument starts lying about its own reach.
    delta="$(diff <(printf '%s' "$before" | tr '|;' '\n\n') \
                  <(printf '%s' "$after" | tr '|;' '\n\n') | grep '^[<>]' | tr '\n' ' ')"
    # And the after side on its own. A delta line starting with `<` is what the
    # BEFORE state held, so an assertion asking "does the report name X" could
    # be satisfied by a record that VANISHED: `chmod 000` on a directory hides
    # everything the probe wrote inside it, the probe's own record then appears
    # only as a removal, and the run — which had just lost the record of a real
    # write — passed the shape assertion. What has to be named is what the net
    # recorded AFTER the run.
    added="$(diff <(printf '%s' "$before" | tr '|;' '\n\n') \
                  <(printf '%s' "$after" | tr '|;' '\n\n') | grep '^>' | tr '\n' ' ')"
    violations="${violations:+$violations; }the tree itself changed during the run: $delta"
  fi
  printf '%s\t%s\t%s\t%s\t%s\n' "$n_mut" "$n_parse" "$measured" "$added" "$violations"
}
# One place that knows judge_tree's field layout. Four call sites read four
# fields out of one tab-separated string positionally, and a fifth field inserted
# in the middle would have been read as the fourth at every one of them — the
# suite would have reported somebody else's delta as the violations, silently.
judge_fields() { # judge_tree output -> J_MUT J_PARSE J_MEASURED J_ADDED J_VIOLATIONS
  J_MUT="${1%%$'\t'*}"; local rest="${1#*$'\t'}"
  J_PARSE="${rest%%$'\t'*}"; rest="${rest#*$'\t'}"
  J_MEASURED="${rest%%$'\t'*}"; rest="${rest#*$'\t'}"
  J_ADDED="${rest%%$'\t'*}"; J_VIOLATIONS="${rest#*$'\t'}"
}

TREE="$WORK/tree-d"
new_tree "$TREE"
BEFORE="$(tree_state "$TREE")"
JUDGE="$(judge_tree "$TREE")"
AFTER="$(tree_state "$TREE")"
[[ "$BEFORE" == "$AFTER" ]] \
  && ok "the logged run left the tree byte-identical" \
  || fail "the logged run changed the tree it graded: [$BEFORE] -> [$AFTER]"

judge_fields "$JUDGE"
N_MUTATING="$J_MUT" N_PARSE="$J_PARSE" MEASURED="$J_MEASURED" VIOLATIONS="$J_VIOLATIONS"
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
python3 - "$GATE" "$BROKEN/gate.sh" "$BROKEN/gate-shell.sh" "$BROKEN/gate-worktree.sh" "$BROKEN/gate-gitfile-lock.sh" "$BROKEN/gate-gitfile-corrupt.sh" <<'PY'
import sys
src = open(sys.argv[1]).read()
out, shell_out, worktree_out = sys.argv[2], sys.argv[3], sys.argv[4]
gitfile_lock_out, gitfile_corrupt_out = sys.argv[5], sys.argv[6]
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
    # Two more of the same kind one field down: writes that change only what the
    # record says about an entry that was already there — a gitdir FILE's
    # mode@mtime, a gitdir DIRECTORY's mode — where the four above all announce
    # themselves by appearing. The seventh review reverted each of these fields
    # separately and the suite stayed green.
    'chmod 600 "$ROOT/.git/description"\n',
    'chmod 750 "$ROOT/.git/refs"\n',
    # And the exclusion that survives: `.git/index` is skipped because READING
    # can rewrite it. It is skipped by PATH — this write is a file that merely
    # shares the name, and a rule that skipped `-name index` would have let it
    # through. The one exclusion left in the net is the one this probe pins.
    ': > "$ROOT/.git/refs/index"\n',
    # And the opposite of every probe above: a write that takes a record AWAY.
    # chmod'ing a directory to 000 hides everything inside it, so a path the
    # before-state held is in the after-state as a removal and as nothing else.
    # The control after the shape assertions reads the delta for exactly this
    # path, because "does the report name X" searched over both sides of a diff
    # is satisfied by a record that vanished — which is what the shape
    # assertions below did until this round.
    'chmod 000 "$ROOT/.git/g3-hidden"\n',
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
    # The sixth review's: writes that change no byte and create no
    # entry. The record held each file's digest and nothing else, so a gate that
    # took the exec bit off a script, or made the deliverable read-only, left
    # all of it looking untouched — and the run reported "the tree under test is
    # exactly as this script found it". Separate targets from every content
    # probe above, on purpose: the shape is "the bytes did not change and the
    # write is still real".
    'chmod 604 "$ROOT/internal/config/probe-mode.txt"\n',
    # An mtime set to a fixed date rather than "now": a touch that only moves
    # the timestamp forward can land inside the filesystem's granularity or the
    # same second the fixture was written, and a probe that fails for that
    # reason would be read as "the net saw it" by nobody and as flakiness by
    # everyone.
    'touch -d "2001-01-01 00:00:00" "$ROOT/internal/config/probe-time.txt"\n',
    # And the boundary of the ONE exclusion the worktree net keeps: a tracked
    # file whose name starts with `.git`. `-path ./.git` covers it; `-path
    # './.git*'` — a spelling nobody would question in review — drops it.
    'printf "the gate rewrote the ignore rules\\n" > "$ROOT/.gitignore"\n',
    # The seventh review's three: writes that change only the fields describing
    # an entry the fixture already had — a directory's mode, a symlink's target,
    # a symlink's own time. A record can name every path in the tree and still
    # be blind to all three, which is what the suite was.
    'chmod 750 "$ROOT/internal/config/probe-dir"\n',
    'ln -sfn probe-mode.txt "$ROOT/internal/config/probe-link"\n',
    'touch -h -d "2001-01-01 00:00:00" "$ROOT/internal/config/probe-link2"\n',
    # And a write that takes a record away instead of writing one: the same
    # chmod-to-000 shape as the gitdir probes above, in the worktree this time,
    # for the reader control after the shape assertions below.
    'chmod 000 "$ROOT/internal/config/g3-hidden"\n',
]
open(worktree_out, "w").write(src.replace(anchor, probe + "".join(worktree_probes) + anchor, 1))

# And TWO more broken gates for the shape the G3 gate actually runs in: a tree
# that is a LINKED WORKTREE, so `.git` is a file naming the real gitdir. Both
# probes are pure shell — the first reads the gitfile's content the way a shell
# would, so even the naming net has no command to read.
#
# Two gates and not two probes in one, because the second breaks the tree in a
# way that stops it being nameable at all: with `x` appended, git can no longer
# resolve the worktree, so `rev-parse --git-dir` fails and the walk of the gitdir
# comes back EMPTY — the delta then holds every path as REMOVED and the lock the
# first probe wrote names nothing. A run doing both could only be asserted as
# "something changed"; split, each gate pins its own claim: the one names a path
# INSIDE the directory the gitfile points at, the other names the gitfile.
gitfile_lock_probes = [
    'gd="$(sed -n "1s/^gitdir: //p" "$ROOT/.git")"\n'
    ': > "$gd/g3-leftover.lock"\n',
    # And a write to the gitfile that changes no byte of it: the record held its
    # digest alone, so the one file that says which gitdir this tree is could be
    # touched without moving the fingerprint. `-h` here is inert (the gitfile is
    # a regular file) and is written so the probe reads the same as the symlink
    # one above.
    'touch -h -d "2001-01-01 00:00:00" "$ROOT/.git"\n',
    # And the index the exclusion was NOT written for: the COMMON dir has an
    # index of its own, belonging to the main worktree, and the graded tree's
    # index is a different file under it (`worktrees/<id>/index`). An exclusion
    # spelled as a name, or as the literal `./index` the old walk carried, drops
    # this one too — from the very directory being walked. The write is a touch,
    # so the main repo stays usable for the cases after this one, and the shell
    # has no command for the naming net to read: it is derived from the gitfile
    # the same way the first probe derives its gitdir.
    'gd="$(sed -n "1s/^gitdir: //p" "$ROOT/.git")"\n'
    'touch "${gd%/worktrees/*}/index"\n',
    # And a record the run takes away rather than adds, for the reader control
    # after the shape assertions of the first tree below: the worktree walk runs
    # in this shape too, and a directory chmod'ed to 000 hides what is inside it
    # from the after-state while the before-state holds it.
    'chmod 000 "$ROOT/internal/config/g3-hidden"\n',
]
open(gitfile_lock_out, "w").write(src.replace(anchor, probe + "".join(gitfile_lock_probes) + anchor, 1))
gitfile_corrupt_probes = [
    'printf "x\\n" >> "$ROOT/.git"\n',
]
open(gitfile_corrupt_out, "w").write(src.replace(anchor, probe + "".join(gitfile_corrupt_probes) + anchor, 1))
PY
chmod +x "$BROKEN/gate.sh" "$BROKEN/gate-shell.sh" "$BROKEN/gate-worktree.sh" \
         "$BROKEN/gate-gitfile-lock.sh" "$BROKEN/gate-gitfile-corrupt.sh"
# The mutations have to be in the tree under test for the nets to have
# something to find, so the gate is copied into a tree of its own.
TREE="$WORK/tree-e"
new_tree "$TREE"
cp "$BROKEN/gate.sh" "$TREE/tests/acceptance/gitea-real-services-e2e.sh"
ESCAPED="$(judge_tree "$TREE")"
judge_fields "$ESCAPED"
E_MUT="$J_MUT" E_PARSE="$J_PARSE" E_MEASURED="$J_MEASURED" E_ADDED="$J_ADDED"
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
# The record the run takes away, for the control after the shape assertions: a
# file that exists before the run and is hidden by it. It is built here and not
# in new_tree because it is a target rather than a fixture — no other case in
# this file wants a directory that disappears under it.
mkdir -p "$TREE/.git/g3-hidden"
printf 'x\n' > "$TREE/.git/g3-hidden/inside.txt"
SHELLONLY="$(judge_tree "$TREE")"
judge_fields "$SHELLONLY"
S_MUT="$J_MUT" S_PARSE="$J_PARSE" S_MEASURED="$J_MEASURED" S_ADDED="$J_ADDED"
S_MERGED="$J_VIOLATIONS"
if (( S_MEASURED == 1 )); then
  ok "the measuring net saw the tree change with no command logged to parse"
else
  fail "7 shell-level writes went into the tree's own .git and the measuring net saw nothing: $SHELLONLY"
fi
# One write being visible is not the claim, and "the tree differs" does not say
# what was seen. The fingerprint digested regular files only until a review put
# a SYMLINKED HOOK, an empty directory and a leftover `.lock` past it — and the
# third of those is a regular file, which is why the exclusion that skipped it
# is part of the same finding rather than a separate one. So the assertion is
# that the report names all seven, by kind and by path. A net that sees one kind
# of file is a net with a spelling problem, and so is one that skips by name
# what it meant to skip by path.
#
# The shapes are required on the AFTER side of the delta (the `>` lines), not
# merely somewhere in a diff of two states. Both sides used to be searched, and
# a record that VANISHED satisfied the search: `chmod 000` on the directory
# holding a probe's file hides the file from the net, the file's record shows up
# only as a removal, and the run — which had just lost the record of a real
# write — passed. What has to be named is what the net recorded after the run.
S_ADDED_DETAIL="$J_ADDED"
unseen=""
# With the label of the walk, for the same reason as the worktree shapes below:
# the delta is one string merged from every net, so a path on its own does not
# say which net named it.
for shape in "gitdir f ./hooks/commit-msg" "gitdir l ./hooks/pre-commit ->/bin/true" \
             "gitdir f ./g3-leftover.lock" "gitdir d ./g3-empty-dir" "gitdir f ./refs/index" \
             "gitdir f ./description 600@" "gitdir d ./refs 750:"; do
  case "$S_ADDED_DETAIL" in
    *"$shape"*) ;;
    *) unseen="$unseen [$shape]" ;;
  esac
done
if [[ -z "$unseen" ]]; then
  ok "the measuring net named all seven on the after side, each under the label of the walk that found it: two that are not regular files, the one it skipped for its name, the one that only shares a name with the index, and the two whose mode alone moved"
else
  fail "the measuring net did not report:$unseen — what it digests is a spelling too, and this one excluded symlinks, directories and anything named *.lock, and recorded an entry's path without what its kind is worth: $SHELLONLY"
fi
if (( S_PARSE == 0 )); then
  ok "no command was logged for any of them, so the naming net was blind to them by construction"
else
  fail "the naming net claims to have judged a write that ran no command at all: $SHELLONLY"
fi
# The two checks below are about the READER above, not about the net, and they
# exist because the sentence in the comment was not true of the code: the
# shapes are read from `$S_ADDED_DETAIL`, and for a round that variable held the
# merged delta, so the fix could be reverted without the suite noticing. The
# first check is the control's own: one of the probes must take a record away,
# or "the reader is not fooled by a vanished record" is a claim about nothing.
# The second is the claim — the record taken away must NOT be in the field the
# shapes are searched in, and must be in the delta, which is where it can be
# read without being mistaken for a record the net made.
S_VANISHED="gitdir f ./g3-hidden/inside.txt"
case "$S_MERGED" in
  *"$S_VANISHED"*) ;;
  *) fail "no probe hides a record the before-state held, so the after-side control below proves nothing: $SHELLONLY" ;;
esac
case "$S_ADDED_DETAIL" in
  *"$S_VANISHED"*)
    fail "the shapes above are being searched in a diff of two states rather than in what the net recorded after the run: a record the run had just LOST would count as one it named" ;;
  *) ok "a record the run hides is not counted as one the net named, and the removal it leaves is in the delta — the shapes are read from the after side" ;;
esac
# The probe leaves a directory nothing can search; the tree is finished with, so
# it goes back to a mode the cleanup can walk.
chmod 755 "$TREE/.git/g3-hidden"

# The same question one region out. The five above are all inside .git; these
# three are in the worktree, which the net reached only through porcelain — and
# porcelain is not a fingerprint: it summarizes a wholly untracked directory in
# one line, and sees no directory git cannot represent. This is the region the
# gate lands in, since the Worker's deliverable and the gate's own copy are both
# untracked, so a write here was the most likely one and the least visible.
TREE="$WORK/tree-h"
new_tree "$TREE"
cp "$BROKEN/gate-worktree.sh" "$TREE/tests/acceptance/gitea-real-services-e2e.sh"
mkdir -p "$TREE/internal/config/g3-hidden"
printf 'x\n' > "$TREE/internal/config/g3-hidden/inside.txt"
WORKTREEONLY="$(judge_tree "$TREE")"
judge_fields "$WORKTREEONLY"
W_MUT="$J_MUT" W_PARSE="$J_PARSE" W_MEASURED="$J_MEASURED" W_ADDED="$J_ADDED" W_DETAIL="$J_VIOLATIONS"
if (( W_MEASURED == 1 )); then
  ok "the measuring net saw writes in the worktree, not only inside .git"
else
  fail "9 shell-level writes went into the worktree outside .git and the measuring net saw nothing: $WORKTREEONLY"
fi
w_unseen=""
# Each shape is asserted WITH the label of the walk that has to name it, for the
# reason the linked-worktree case below states: the delta is one string merged
# from every net, so a path alone does not say who saw it. `.gitignore` is
# tracked, and porcelain names it too (` M .gitignore`) — asserting the bare
# path here let the whole suite pass while the worktree walk was dropping that
# very file, which is what `-path './.git*'` does.
#
# On the AFTER side of the delta, for the reason the gitdir shapes above state:
# a probe whose record vanishes (here by chmod'ing a directory to 000 around it)
# would otherwise be "named" by its own removal.
W_ADDED_DETAIL="$J_ADDED"
for shape in "worktree f ./internal/config/deliverable.txt" "worktree f ./tests/acceptance/g3-extra.sh" \
             "worktree d ./g3-empty-root" "worktree f ./internal/config/probe-mode.txt" \
             "worktree f ./internal/config/probe-time.txt" "worktree f ./.gitignore" \
             "worktree d ./internal/config/probe-dir 750:" \
             "worktree l ./internal/config/probe-link ->probe-mode.txt@" \
             "worktree l ./internal/config/probe-link2 ->deliverable.txt@2001-01-01"; do
  case "$W_ADDED_DETAIL" in
    *"$shape"*) ;;
    *) w_unseen="$w_unseen [$shape]" ;;
  esac
done
if [[ -z "$w_unseen" ]]; then
  ok "the measuring net named all nine on the after side, each under the label of the walk that found it: the overwritten untracked deliverable, the file left in an untracked directory, the empty directory, the chmod'ed file, the touched file, the tracked .gitignore at the boundary of the .git exclusion, and the three writes that change only what an entry's kind is worth — a directory's mode, a symlink's target, a symlink's own time"
else
  fail "the measuring net did not report:$w_unseen — it reached the worktree through 'git status --porcelain', which summarizes an untracked directory instead of reading it and cannot see an empty one, it recorded each file by its BYTES, which a chmod and a touch do not change, and it recorded a path without what its kind is worth, which a re-pointed symlink or a chmod'ed directory does not change either; a shape named only by porcelain is not named here: $WORKTREEONLY"
fi
if (( W_PARSE == 0 )); then
  ok "no command was logged for any of them either, so only the fingerprint could have seen them"
else
  fail "the naming net claims to have judged a worktree write that ran no command at all: $WORKTREEONLY"
fi
# The same reader control as the gitdir block's, one region out — the worktree
# walk, where the shape assertions above are read from `$W_ADDED_DETAIL`. A
# record the run took away must be in the delta and must NOT be in the field the
# shapes are searched in; the first check keeps the second from being vacuous.
W_VANISHED="worktree f ./internal/config/g3-hidden/inside.txt"
case "$W_DETAIL" in
  *"$W_VANISHED"*) ;;
  *) fail "no worktree probe hides a record the before-state held, so the after-side control below proves nothing: $WORKTREEONLY" ;;
esac
case "$W_ADDED_DETAIL" in
  *"$W_VANISHED"*)
    fail "the worktree shapes above are being searched in a diff of two states rather than in what the net recorded after the run: a record the run had just LOST would count as one it named" ;;
  *) ok "a worktree record the run hides is not counted as one the net named, and the removal it leaves is in the delta" ;;
esac
chmod 755 "$TREE/internal/config/g3-hidden"

# --- 3b. a tree that IS a linked worktree ------------------------------------
# The shape the G3 gate actually runs in, and the one the measuring net used to
# stop measuring. `git worktree add` leaves a FILE at `.git` naming the real
# gitdir, so `cd "$ROOT/.git"` fails — and the net returned 0 with an EMPTY
# record, while the worktree net prunes `./.git` whether it is a directory or a
# file, so the gitfile was in neither. A net that measures nothing compares
# equal to itself forever, which means the tree it silently stopped measuring
# reports clean. Neither write runs a command the naming net can read.
MAIN="$WORK/tree-main"
mkdir -p "$MAIN"
git -C "$MAIN" init -q
printf 'x\n' > "$MAIN/README.md"
git -C "$MAIN" add -A
git -C "$MAIN" -c user.name=t -c user.email=t@e commit -q -m base
# Sets WTREE rather than printing it: fail() prints to stdout, and a fixture
# that reported a problem would hand that report back as a path.
WTREE=""
new_worktree() { # name -> WTREE, a linked worktree of $MAIN holding the fixture
  WTREE="$WORK/$1"
  git -C "$MAIN" worktree add -q "$WTREE"
  [[ -f "$WTREE/.git" ]] \
    || fail "the fixture $1 is not a linked worktree, so this case cannot speak about the shape it exists for"
  mkdir -p "$WTREE/tests/acceptance" "$WTREE/internal/config"
  printf 'worker deliverable\n' > "$WTREE/internal/config/deliverable.txt"
  # The target of the metadata-only write in the read control below. It is here
  # and not only in new_tree because the control that needs it has to run in a
  # LINKED worktree: the standalone control cannot speak for that shape, which
  # is the whole reason the second one exists.
  printf 'nothing writes these bytes\n' > "$WTREE/internal/config/probe-mode.txt"
  chmod 644 "$WTREE/internal/config/probe-mode.txt"
}

# (i) a file dropped in the gitdir the gitfile names. This is the path the old
# net could not reach: it is not under the worktree, and it is not under `.git`
# — which is a file here.
new_worktree tree-i
TREE="$WTREE"
cp "$BROKEN/gate-gitfile-lock.sh" "$TREE/tests/acceptance/gitea-real-services-e2e.sh"
mkdir -p "$TREE/internal/config/g3-hidden"
printf 'x\n' > "$TREE/internal/config/g3-hidden/inside.txt"
LINKED="$(judge_tree "$TREE")"
judge_fields "$LINKED"
L_MUT="$J_MUT" L_PARSE="$J_PARSE" L_MEASURED="$J_MEASURED" L_ADDED="$J_ADDED" L_DETAIL="$J_VIOLATIONS"
if (( L_MEASURED == 1 )); then
  ok "the measuring net saw a write in a tree whose .git is a FILE, not a directory"
else
  fail "a shell write went into the gitdir of a linked worktree and the measuring net measured nothing at all: $LINKED"
fi
case "$L_ADDED" in
  # The label is asserted with the path, not decoration: the same file reads as
  # `f ./g3-leftover.lock` in whichever walk found it, and the claim here is
  # that the walk of the GITDIR found it. On the after side, for the reason the
  # other shape assertions state: a record that vanished is not a record that
  # named anything.
  *"gitdir f ./g3-leftover.lock"*)
    ok "the measuring net named the file dropped in the gitdir the gitfile points at, under the label of the walk that found it" ;;
  *) fail "in a linked worktree the measuring net did not report 'gitdir f ./g3-leftover.lock' in its delta: $LINKED" ;;
esac
# And the one path in that region the old exclusion reached by accident: the
# COMMON dir's index, which is not this tree's index and is not rewritten by
# this tree's reads. Naming it here is what makes "excluded by resolved path,
# scoped to the graded tree" a claim with a probe rather than a preference.
case "$L_ADDED" in
  *"common f ./index "*)
    ok "the common dir's own index — which the graded tree's reads do not rewrite — is recorded, not swept up by the exclusion written for this tree's" ;;
  *) fail "the index exclusion reached the COMMON dir's index too, a file this tree's reads never touch: $LINKED" ;;
esac
# And the gitfile itself, with a write that changes none of its bytes. The
# record was the digest alone, so the one file that says which gitdir this tree
# IS could be touched without moving the fingerprint — a write the naming net
# cannot see either, since it runs no command.
case "$L_ADDED" in
  *"gitfile $TREE/.git "*@2001-01-01*)
    ok "the measuring net named the gitfile itself by its mode and time, not only by its digest" ;;
  *) fail "a touch on the gitfile — no byte of it changed — left the net naming nothing about it: $LINKED" ;;
esac
if (( L_PARSE == 0 )); then
  ok "no command was logged for that write, so only the fingerprint could have seen it"
else
  fail "the naming net claims to have judged a worktree write that ran no command at all: $LINKED"
fi
# And the reader control in the shape the G3 gate actually runs in: three
# assertions above read `$L_ADDED`, and one of the probes takes a record away.
L_VANISHED="worktree f ./internal/config/g3-hidden/inside.txt"
case "$L_DETAIL" in
  *"$L_VANISHED"*) ;;
  *) fail "no probe in the linked worktree hides a record the before-state held, so the after-side control below proves nothing: $LINKED" ;;
esac
case "$L_ADDED" in
  *"$L_VANISHED"*)
    fail "the linked-worktree shapes above are being searched in a diff of two states rather than in what the net recorded after the run: a record the run had just LOST would count as one it named" ;;
  *) ok "a linked-worktree record the run hides is not counted as one the net named, and the removal it leaves is in the delta" ;;
esac
chmod 755 "$TREE/internal/config/g3-hidden"

# (ii) the gitfile itself. Its own case, because appending to it leaves git
# unable to resolve the worktree — the gitdir walk comes back empty, and the
# delta then holds no path from inside it at all. What is pinned here is that
# the gitfile is inside the net (it was in neither net before) and that the net
# says WHICH file changed, by name and digest.
new_worktree tree-j
TREE="$WTREE"
cp "$BROKEN/gate-gitfile-corrupt.sh" "$TREE/tests/acceptance/gitea-real-services-e2e.sh"
CORRUPT="$(judge_tree "$TREE")"
judge_fields "$CORRUPT"
C_MUT="$J_MUT" C_PARSE="$J_PARSE" C_MEASURED="$J_MEASURED" C_DETAIL="$J_VIOLATIONS"
if (( C_MEASURED == 1 )); then
  ok "the measuring net saw the gitfile itself change"
else
  fail "a write to the gitfile left the linked worktree's state unchanged: $CORRUPT"
fi
case "$C_DETAIL" in
  *"gitfile $TREE/.git "*) ok "the measuring net named the gitfile, with the mode, the time and the digest it recorded" ;;
  *) fail "the measuring net did not name the gitfile as what changed: $CORRUPT" ;;
esac
if (( C_PARSE == 0 )); then
  ok "no command was logged for that write either"
else
  fail "the naming net claimed to have judged a write that ran no command at all: $CORRUPT"
fi

# The read half, in the shape the gate ACTUALLY runs in. This is the control the
# seventh review asked for, and the reason it is a second control rather than an
# extension of the one below: that one is built by new_tree, a STANDALONE
# repository, where the index is `./index` — exactly the spelling the exclusion
# was written for. In a linked worktree the index is
# `<common>/worktrees/<id>/index`, and a net that excluded the NAME recorded it,
# so `git status` rewrote a file the net was fingerprinting and the net fired on
# its own bookkeeping: measured, the fingerprint differed from itself with
# nothing in between in 10 runs out of 10, and the delta was the index's mtime
# and nothing else. A control that cannot fail in the shape under test is how
# that defect survived a round whose whole subject was the linked worktree.
#
# The gate here is the real one, unmodified, and the claim is made only if it
# ran to the end — the same sentence case 1 asserts — so "the gate read the
# tree" is measured rather than assumed.
READONLY_WT="$WORK/tree-k2"
new_worktree tree-k2
RW_TREE="$WTREE"
cp "$GATE" "$RW_TREE/tests/acceptance/gitea-real-services-e2e.sh"
RW_BEFORE="$(tree_state "$RW_TREE")"
run_gate "$RW_TREE"
RW_AFTER="$(tree_state "$RW_TREE")"
case "$OUT" in
  *"the tree under test is exactly as this script found it"*) RW_RAN=1 ;;
  *) RW_RAN=0 ;;
esac
if [[ "$RW_BEFORE" == "$RW_AFTER" ]]; then
  ok "the whole gate, run in a linked worktree, leaves the fingerprint equal — reading rewrites the index git keeps for that worktree in the common dir, and it is excluded wherever it is"
else
  fail "the measuring net fired on a read in the shape the gate runs in: $(diff <(printf '%s' "$RW_BEFORE" | tr '|;' '\n\n') <(printf '%s' "$RW_AFTER" | tr '|;' '\n\n') | grep '^[<>]' | tr '\n' ' ')"
fi
if (( RW_RAN == 1 )); then
  ok "and that run reached the non-interference check, so the control above is a whole run and not an early exit"
else
  fail "the linked-worktree control did not run the gate to its end, so it asserts nothing about the net: $(printf '%s' "$OUT" | head -3 | tr '\n' ' ')"
fi
RW_BEFORE="$(tree_state "$RW_TREE")"
chmod 604 "$RW_TREE/internal/config/probe-mode.txt"
RW_AFTER="$(tree_state "$RW_TREE")"
if [[ "$RW_BEFORE" == "$RW_AFTER" ]]; then
  fail "a chmod in a linked worktree left the fingerprint equal — the net is blind to it there, so the assertion above proves nothing about that shape"
else
  ok "and a metadata-only write in that same linked worktree is still a change it sees"
fi

# The other half of a net's worth, and the one this round's own review asked
# for: it must not fire on what the gate READS. `git status` rewrites
# `.git/index` (create a lock, rename over the index) — that is why the index
# is excluded from the walk — and it moves the git dir's mtime. Recording the
# directory timestamp made every run of the gate a change to the tree, which is
# a net that is red on a correct gate and gets cut. Both halves are asserted:
# the read moves nothing, and a write still moves something.
READONLY="$WORK/tree-k"
new_tree "$READONLY"
R_BEFORE="$(tree_state "$READONLY")"
git -C "$READONLY" status --porcelain >/dev/null
git -C "$READONLY" rev-parse HEAD >/dev/null
git -C "$READONLY" diff --stat >/dev/null
R_AFTER="$(tree_state "$READONLY")"
if [[ "$R_BEFORE" == "$R_AFTER" ]]; then
  ok "reading the tree — which rewrites .git/index and moves the git dir's mtime — leaves the fingerprint equal"
else
  fail "the measuring net fired on a read: $(diff <(printf '%s' "$R_BEFORE" | tr '|;' '\n\n') <(printf '%s' "$R_AFTER" | tr '|;' '\n\n') | grep '^[<>]' | tr '\n' ' ')"
fi
R_BEFORE="$(tree_state "$READONLY")"
chmod 604 "$READONLY/internal/config/probe-mode.txt"
R_AFTER="$(tree_state "$READONLY")"
if [[ "$R_BEFORE" == "$R_AFTER" ]]; then
  fail "a chmod on a file in the tree left the fingerprint equal — the net is blind to it, so the assertion above proves nothing"
else
  ok "and a metadata-only write in that same tree is still a change it sees"
fi

# The index has a third thing to say besides its contents, and it is the one the
# net had nothing for: its FLAGS. `git update-index --assume-unchanged` (and
# `--skip-worktree`) makes every later `git status` and `git diff` in that tree
# stop mentioning the file — a gate that hid the Worker's deliverable from the
# Supervisor's inspection wrote no byte, no mode and no entry, and the
# fingerprint called the tree unchanged. `ls-files -v` is the field that sees it;
# the file here is tracked and unmodified, so nothing else in the state moves.
FLAGS="$WORK/tree-m"
new_tree "$FLAGS"
F_BEFORE="$(tree_state "$FLAGS")"
git -C "$FLAGS" update-index --assume-unchanged README.md
F_AFTER="$(tree_state "$FLAGS")"
if [[ "$F_BEFORE" == "$F_AFTER" ]]; then
  fail "an index flag write — which hides a file from every later git status and diff — left the fingerprint equal, so no later inspection can be trusted to mention the Worker's files: $F_BEFORE"
else
  ok "an index flag write the tree's files and porcelain both hide is a change the fingerprint sees"
fi

# And what the record says about a path it could not read at all. `0@<time>@`
# with an empty field after the last `@` is a claim: these are the bytes, and
# they are nothing. A file whose mode denies reading is not that, and a
# directory whose mode denies searching is not an empty one — so the record
# names the measurement it did not make. (What the net still cannot see INSIDE
# such a path is stated at the top of this file: no fingerprint reads what the
# mode forbids. What it must not do is record the two as the same string.)
UNREADABLE="$WORK/tree-n"
new_tree "$UNREADABLE"
printf 'nothing may read this\n' > "$UNREADABLE/internal/config/probe-secret.txt"
mkdir -p "$UNREADABLE/internal/config/probe-secret-dir"
chmod 000 "$UNREADABLE/internal/config/probe-secret.txt" "$UNREADABLE/internal/config/probe-secret-dir"
# Two neighbours the mode does NOT deny, so the word below can be shown to name
# a path rather than a net that simply says it could not read anything.
printf 'anyone may read this\n' > "$UNREADABLE/internal/config/probe-readable.txt"
mkdir -p "$UNREADABLE/internal/config/probe-readable-dir"
printf 'x\n' > "$UNREADABLE/internal/config/probe-readable-dir/inside.txt"
U_STATE="$(tree_state "$UNREADABLE")"
u_missing=""
u_file_rec="$(record_of "$U_STATE" worktree f ./internal/config/probe-secret.txt)"
u_dir_rec="$(record_of "$U_STATE" worktree d ./internal/config/probe-secret-dir)"
case "$u_file_rec" in
  "") u_missing="$u_missing [no record at all for the file it could not digest]" ;;
  *unreadable*) ;;
  *) u_missing="$u_missing [the file's own record does not say it could not digest it]" ;;
esac
case "$u_dir_rec" in
  "") u_missing="$u_missing [no record at all for the directory it could not search]" ;;
  *unreadable*) ;;
  *) u_missing="$u_missing [the directory's own record does not say it could not search it]" ;;
esac
if [[ -z "$u_missing" ]]; then
  ok "a path whose mode denies reading is recorded as unreadable, not as a file with no bytes or an empty directory"
else
  fail "the record does not say it could not read:$u_missing — an empty digest and a directory with no visible contents are claims this net never measured: $U_STATE"
fi
# The other half, and the half the ninth review found unasserted: every check
# above asks whether the word APPEARS, and a net that wrote it on every record
# would satisfy all of them while telling a later reader nothing — "somebody
# made a path unreadable" is only a statement about the tree if the paths
# somebody did NOT touch are recorded as measured. The two neighbours the mode
# left alone are asserted in their own right: the file's digest is a digest,
# and the directory is not marked. (Each is asked for its OWN record: a `case`
# over the whole state is satisfied by any record in it that says the word.)
u_read_file="$(record_of "$U_STATE" worktree f ./internal/config/probe-readable.txt)"
u_read_dir="$(record_of "$U_STATE" worktree d ./internal/config/probe-readable-dir)"
u_false=""
case "$u_read_file" in
  "") u_false="$u_false [no record at all for the readable file beside them]" ;;
  *unreadable*) u_false="$u_false [the readable file's digest says it could not be read]" ;;
esac
case "$u_read_dir" in
  "") u_false="$u_false [no record at all for the searchable directory beside them]" ;;
  *:unreadable*) u_false="$u_false [the searchable directory's record says it could not be searched]" ;;
esac
if [[ -z "$u_false" ]]; then
  ok "and the readable file and directory beside them are recorded as measured, so the word names the path and not the net"
else
  fail "the mark is not about the path:$u_false — a net that reports it could not read everything has told a later reader nothing about which write it saw: $U_STATE"
fi
chmod 755 "$UNREADABLE/internal/config/probe-secret-dir"

# The third thing the git walk can be handed, and the one that must not read as
# "unchanged": a tree with no git dir at all. The record names the absence, so
# two such trees still compare equal — while a tree that GAINS one does not,
# which is the direction that matters: "there was nothing to measure" and
# "nothing changed" are the two readings this whole net exists to keep apart.
NOGIT="$WORK/tree-l"
mkdir -p "$NOGIT"
N_BEFORE="$(tree_state "$NOGIT")"
case "$N_BEFORE" in
  *"no-git-dir $NOGIT"*) ok "a tree with no git dir is recorded as that, not as nothing" ;;
  *) fail "a tree with no git dir measured nothing at all: $N_BEFORE" ;;
esac
mkdir -p "$NOGIT/.git"
N_AFTER="$(tree_state "$NOGIT")"
if [[ "$N_BEFORE" == "$N_AFTER" ]]; then
  fail "a tree that gained a git dir compares equal to one without — the net cannot see one appearing"
else
  ok "and a git dir appearing where there was none is a change it sees"
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
# The count is a CHECK, not a caption. `grep -l x` could be deleted from the
# fixture — or a line could be added for a command the judge does accuse — and
# the suite stayed green and went on printing "18", because the assertion is
# "no form in this file is accused", which fewer forms satisfy just as well. A
# form that is not in the fixture cannot be a form the judge is asked about, so
# the number in the sentence is asserted against the file it describes.
READS_N="$(grep -c . "$READS")"
if (( READS_N != 18 )); then
  fail "the reads fixture holds $READS_N forms, not the 18 this check's reach is written as — a form that is not in it was never asked about"
else
  ok "the reads fixture holds all 18 forms the naming net has to pass"
fi
if (( FALSE_ALARMS == 0 )); then
  ok "the naming net does not accuse $READS_N read-only forms of writing in the tree"
else
  fail "the naming net accuses $FALSE_ALARMS of $READS_N read-only forms of writing in the tree: $(python3 "$WORK/judge.py" "$READS" "$TREE" | cut -f3)"
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
