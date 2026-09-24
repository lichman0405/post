#!/bin/bash
# shellcheck disable=SC2016,SC2088  # the single-quoted $HOME/~/$VAR strings are
#                                   # literal guard inputs, deliberately unexpanded
# guard-regression.sh — two-sided regression suite for worker-guard.sh.
#
# Every isolation rule is tested BOTH ways: the dangerous action is blocked
# AND the legitimate neighbouring action is allowed. Over-blocking has been
# the recurring defect of this isolation layer (tasks/decisions.md
# L1-20260912-9/13/16), so an allow-side failure is a regression exactly like
# a block-side failure.
#
# The suite drives the guard the way Claude Code does: the tool call JSON on
# stdin, the worker environment variables in the process environment, and the
# verdict as the exit code (0 = allow, 2 = block). It needs no Claude Code,
# no network and no git repository.
#
# Usage: bash tests/worker-guard/guard-regression.sh [path-to-worker-guard.sh]
# The canonical source of worker-guard.sh is embedded in the rddev binary
# (internal/devorchestrator/embed/worker-guard.sh); the default here runs the
# checked-out copy, which a drift test keeps identical to the embedded one.

set -u

GUARD=${1:-"$(dirname "$0")/../../internal/devorchestrator/embed/worker-guard.sh"}
GUARD=$(realpath "$GUARD")

# Default worker environment, mirroring what rddev worker spawn sets.
export POST_REPO_ROOT=/repo
export POST_WORKER_TASK_ID=T0010
export POST_WORKER_WORKTREE=/repo/.rddev/worktrees/T0010
export POST_WORKER_RESULT_DIR=/repo/.rddev/workers/T0010
export POST_WORKER_DOCKER_GRANT=""

pass=0
fail=0
failed_cases=""

# check NAME EXPECT REASON TOOL FIELD VALUE [ENV_KEY=VAL ...]
# EXPECT is "allow" or "block". REASON is a fragment the block message must
# contain ("" for allow). The tool call is {"tool_name":TOOL,
# "tool_input":{FIELD:VALUE}}; additional arguments override the worker
# environment for this one case.
check() {
	name=$1
	expect=$2
	reason=$3
	tool=$4
	field=$5
	value=$6
	shift 6

	json=$(python3 - "$tool" "$field" "$value" <<'PYEOF'
import json, sys
print(json.dumps({"tool_name": sys.argv[1], "tool_input": {sys.argv[2]: sys.argv[3]}}))
PYEOF
	)

	# build the per-case environment (env -u clears a default)
	env_args=()
	for kv in "$@"; do
		key=${kv%%=*}
		val=${kv#*=}
		if [ -n "$val" ]; then
			env_args+=( "$key=$val" )
		else
			env_args+=( -u "$key" )
		fi
	done

	out=$(printf '%s\n' "$json" | env "${env_args[@]}" sh "$GUARD" 2>&1)
	code=$?
	# hook protocol: exit 2 = block, exit 0 = allow
	case "$code:$expect" in
		2:block|0:allow)
			if [ "$expect" = "block" ] && [ -n "$reason" ]; then
				case "$out" in
					*"$reason"*) : ;;
					*) record_fail "$name" "blocked but message missing '$reason': $out"; return ;;
				esac
			fi
			pass=$((pass + 1)) ;;
		*)
			record_fail "$name" "expected $expect, got exit $code: $out" ;;
	esac
}

record_fail() {
	fail=$((fail + 1))
	failed_cases="$failed_cases\n  - $1: $2"
}

# check_json NAME EXPECT REASON RAW-JSON — the same verdict contract as
# check(), for rules the string-only helper cannot express: a BOOLEAN field
# (run_in_background) or a payload whose PATH is not the plain string check()
# would build (a decoy key inside model-controlled content, duplicate keys,
# unparseable JSON). The document is passed through verbatim.
check_json() { # name expect-reason raw-json
	name=$1
	expect=$2
	reason=$3
	json=$4
	out=$(printf '%s\n' "$json" | sh "$GUARD" 2>&1)
	code=$?
	case "$code:$expect" in
		2:block|0:allow)
			if [ "$expect" = "block" ] && [ -n "$reason" ]; then
				case "$out" in
					*"$reason"*) : ;;
					*) record_fail "$name" "blocked but message missing '$reason': $out"; return ;;
				esac
			fi
			pass=$((pass + 1)) ;;
		*)
			record_fail "$name" "expected $expect, got exit $code: $out" ;;
	esac
}

# ---------------------------------------------------------------------------
# Shell write confinement — own worktree / own result dir / /tmp / /dev sinks
# allowed, everything else blocked.

check "redirect into worktree"            allow ""  Bash command 'echo hi > notes.txt'
check "redirect into /etc"                block "confined" Bash command 'echo hi > /etc/x'
check "redirect into /tmp"                allow ""  Bash command 'echo hi > /tmp/x'
check "redirect to /dev/null"             allow ""  Bash command 'echo hi > /dev/null'
check "stderr redirect to /dev/null"      allow ""  Bash command 'go build 2>/dev/null'
check "fd dup 2>&1 plus local log"        allow ""  Bash command 'make check 2>&1 > build.log'
check "quoted redirect to /etc (conservative over-block)" block "confined" Bash command 'echo "x > /etc/y"'
check "quoted absolute redirect caught"   block "confined" Bash command 'echo hi > "/etc/quoted"'
check "append to /tmp allowed"            allow ""  Bash command 'echo hi >> /tmp/log.txt'

check "rm own worktree file"              allow ""  Bash command 'rm -f file.txt'
check "rm relative dir inside worktree"   allow ""  Bash command 'rm -rf internal/config/tmp'
check "rm /etc"                           block "confined" Bash command 'rm -f /etc/hosts'
check "rm /home"                          block "confined" Bash command 'rm -rf /home/o/x'
check "rm ~"                              block "confined" Bash command 'rm ~/x'
check "rm \$HOME"                         block "confined" Bash command 'rm $HOME/x'
check "rm \${HOME}"                       block "confined" Bash command 'rm ${HOME}/x'
check "rm /tmp allowed"                   allow ""  Bash command 'rm /tmp/x'
check "rm unresolved var fails closed"    block "confined" Bash command 'rm $UNKNOWN/x'

check "cp inside worktree"                allow ""  Bash command 'cp a b'
check "cp to /etc"                        block "confined" Bash command 'cp a /etc/b'
check "cp -t /etc"                        block "confined" Bash command 'cp -t /etc a'
check "cp --target-directory=/etc"        block "confined" Bash command 'cp --target-directory=/etc a'
check "cp -t/etc attached value"          block "confined" Bash command 'cp -t/etc a'
check "cp -t /tmp allowed"                allow ""  Bash command 'cp -t /tmp a'
check "mv inside worktree"                allow ""  Bash command 'mv a b'
check "mv to /etc"                        block "confined" Bash command 'mv a /etc/b'
check "ln inside worktree"                allow ""  Bash command 'ln -s a b'
check "ln to /etc"                        block "confined" Bash command 'ln -s a /etc/b'
check "install inside worktree"           allow ""  Bash command 'install -m 644 a b'
check "install -d /etc"                   block "confined" Bash command 'install -d /etc/x'
check "tee inside worktree"               allow ""  Bash command 'tee notes.log'
check "tee piped to /etc"                 block "confined" Bash command 'echo x | tee /etc/x'
check "tee -a /tmp allowed"               allow ""  Bash command 'tee -a /tmp/x'

check "write own RESULT.json (contract path)" allow "" Bash command 'echo done > /repo/.rddev/workers/T0010/RESULT.json'
check "write another workers result dir"  block "confined" Bash command 'echo x > /repo/.rddev/workers/T0009/x'

# ---------------------------------------------------------------------------
# Git control plane — command-position matching, read-only forms stay usable.

check "git commit"                        block "control-plane" Bash command 'git commit --allow-empty -m x'
check "git push"                          block "control-plane" Bash command 'git push origin main'
check "git merge"                         block "control-plane" Bash command 'git merge main'
check "git rebase"                        block "control-plane" Bash command 'git rebase main'
check "git tag create"                    block "control-plane" Bash command 'git tag v1'
check "git tag --list (harness state)"    block "control-plane" Bash command 'git tag --list'
check "git cherry-pick"                   block "control-plane" Bash command 'git cherry-pick abc'
check "git stash"                         block "control-plane" Bash command 'git stash'
check "git reset"                         block "control-plane" Bash command 'git reset --hard HEAD~1'
check "git fetch"                         block "control-plane" Bash command 'git fetch origin'
check "git pull"                          block "control-plane" Bash command 'git pull'
check "git worktree list (harness state)" block "control-plane" Bash command 'git worktree list'
check "git checkout -b"                   block "control-plane" Bash command 'git checkout -b worker-branch'
check "git checkout branch"               block "control-plane" Bash command 'git checkout main'
check "git checkout restore form"         allow ""  Bash command 'git checkout -- file.txt'
check "git branch list"                   allow ""  Bash command 'git branch'
check "git branch -a"                     allow ""  Bash command 'git branch -a'
check "git branch -r"                     allow ""  Bash command 'git branch -r'
check "git branch --show-current"         allow ""  Bash command 'git branch --show-current'
check "git branch --list"                 allow ""  Bash command 'git branch --list'
check "git branch --merged main"          allow ""  Bash command 'git branch --merged main'
check "git branch --contains bare"        allow ""  Bash command 'git branch --contains'
check "git branch --contains value"       block "control-plane" Bash command 'git branch --contains HEAD'
check "git branch create"                 block "control-plane" Bash command 'git branch foo'
check "git branch -D"                     block "control-plane" Bash command 'git branch -D foo'
check "git branch -m"                     block "control-plane" Bash command 'git branch -m foo bar'
check "git branch --set-upstream-to"      block "control-plane" Bash command 'git branch --set-upstream-to=origin/main'
check "git remote list"                   allow ""  Bash command 'git remote'
check "git remote -v"                     allow ""  Bash command 'git remote -v'
check "git remote get-url"                allow ""  Bash command 'git remote get-url origin'
check "git remote show"                   allow ""  Bash command 'git remote show origin'
check "git remote add"                    block "control-plane" Bash command 'git remote add x url'
check "git remote set-url"                block "control-plane" Bash command 'git remote set-url origin url'
check "git remote prune"                  block "control-plane" Bash command 'git remote prune origin'
check "git status"                        allow ""  Bash command 'git status --short'
check "git log"                           allow ""  Bash command 'git log --oneline -5'
check "git diff"                          allow ""  Bash command 'git diff HEAD'
check "git rev-parse"                     allow ""  Bash command 'git rev-parse HEAD'
check "git global -C flag skipped"        allow ""  Bash command 'git -C /tmp status'
check "git config"                        block "control-plane" Bash command 'git config user.name x'
check "git grep"                          allow ""  Bash command 'git grep -n foo'
check "git blame"                         allow ""  Bash command 'git blame file.go'
check "git show"                          allow ""  Bash command 'git show HEAD:README.md'
check "git init"                          block "control-plane" Bash command 'git init'
check "git clone"                         block "control-plane" Bash command 'git clone url'
check "git reflog (read-only)"            allow ""  Bash command 'git reflog'
check "git reflog delete"                 block "control-plane" Bash command 'git reflog delete HEAD@{0}'
check "git am"                            block "control-plane" Bash command 'git am patch'
check "unknown git subcommand fail closed" block "control-plane" Bash command 'git frobnicate'

# Command position only — forbidden words in arguments or quoted text are not
# matched (the false positive the harness fixed; tasks/decisions.md).
check "echo git commit is not git"        allow ""  Bash command 'echo "git commit"'
check "grep service is not control-plane" allow ""  Bash command 'grep -n "service" docs/'
check "second segment after && caught"    block "control-plane" Bash command 'cd x && git push origin main'
check "segment after pipe caught"         block "control-plane" Bash command 'git log | git push'
check "assignment prefix still caught"    block "control-plane" Bash command 'VAR=x git push'
check "env assignment prefix caught"      block "control-plane" Bash command 'env FOO=x git push'
check "env -u option prefix caught"       block "control-plane" Bash command 'env -u VAR git push'
check "env direct caught"                 block "control-plane" Bash command 'env git push'
check "env of read-only git allowed"      allow ""  Bash command 'env FOO=x git log --oneline'
check "sh -c quoting is not parsed (documented limit)" allow "" Bash command 'sh -c "echo git commit"'

# ---------------------------------------------------------------------------
# Control-plane CLIs and privilege escalation.

check "gh auth status"                    block "control-plane" Bash command 'gh auth status'
check "gh --version"                      block "control-plane" Bash command 'gh --version'
check "glab"                              block "control-plane" Bash command 'glab mr list'
check "tea (gitea)"                       block "control-plane" Bash command 'tea issues'
check "sudo"                              block "privilege escalation" Bash command 'sudo -n true'
check "su"                                block "privilege escalation" Bash command 'su -'
check "doas"                              block "privilege escalation" Bash command 'doas x'
check "pkexec"                            block "privilege escalation" Bash command 'pkexec x'
check "echo sudo is not sudo"             allow ""  Bash command 'echo sudo'

# ---------------------------------------------------------------------------
# Docker — version queries allowed, socket access needs the explicit grant.

check "docker --version"                  allow ""  Bash command 'docker --version'
check "docker compose version"            allow ""  Bash command 'docker compose version'
check "docker-compose version"            allow ""  Bash command 'docker-compose version'
check "docker ps"                         block "Docker socket" Bash command 'docker ps'
check "docker compose up"                 block "Docker socket" Bash command 'docker compose up -d'
check "docker exec"                       block "Docker socket" Bash command 'docker exec -it x sh'
check "docker ps with explicit grant"     allow ""  Bash command 'docker ps' POST_WORKER_DOCKER_GRANT=1
check "docker compose up with explicit grant" allow "" Bash command 'docker compose up -d' POST_WORKER_DOCKER_GRANT=1

# ---------------------------------------------------------------------------
# Credential probes.

check "printenv credential probe"         block "credential" Bash command 'printenv GITHUB_PERSONAL_ACCESS_TOKEN'
check "printenv two credentials"          block "credential" Bash command 'printenv GITHUB_TOKEN GH_TOKEN'
check "printenv PATH allowed"             allow ""  Bash command 'printenv PATH'
check "env sets a fake token for a test"  allow ""  Bash command 'env GITHUB_TOKEN=smoke-tok go test ./...'

# ---------------------------------------------------------------------------
# File-read confinement — credential stores, .env, other Workers' state.

check "Read gh hosts.yml"                 block "credential store" Read file_path '/home/shibo/.config/gh/hosts.yml'
check "Read ssh private key"              block "credential store" Read file_path '~/.ssh/id_rsa'
check "Read ssh config"                   block "credential store" Read file_path '~/.ssh/config'
check "Read gnupg"                        block "credential store" Read file_path '~/.gnupg/secring.gpg'
check "Read docker config"                block "credential store" Read file_path '~/.docker/config.json'
check "Read aws credentials"              block "credential store" Read file_path '~/.aws/credentials'
check "Read git-credentials"              block "credential store" Read file_path '~/.git-credentials'
check "Read netrc"                        block "credential store" Read file_path '~/.netrc'
check "Read claude settings"              block "credential store" Read file_path '~/.claude/settings.json'
check "Read harmless home file"           allow ""  Read file_path '~/notes.md'
check "Read .env"                         block ".env" Read file_path '.env'
check "Read .env.dev"                     block ".env" Read file_path '.env.dev'
check "Read .env.example"                 allow ""  Read file_path '.env.example'
check "Read repo .env"                    block ".env" Read file_path '/repo/.env'
check "Read nested .env.local"            block ".env" Read file_path '/repo/docs/.env.local'
check "Read ordinary repo file"           allow ""  Read file_path '/repo/README.md'
check "Grep in .env"                      block ".env" Grep path '.env'
check "Grep in docs"                      allow ""  Grep path 'docs/'
check "Glob go files"                     allow ""  Glob pattern '**/*.go'
check "Glob .env files"                   block ".env" Glob pattern '**/.env*'
check "Glob .env.example"                 allow ""  Glob pattern '**/.env.example'
check "Glob ssh keys"                     block "credential store" Glob pattern '~/.ssh/*'
check "Read own worktree"                 allow ""  Read file_path '/repo/.rddev/worktrees/T0010/README.md'
check "Read other workers worktree"       block "another Worker" Read file_path '/repo/.rddev/worktrees/T0009/README.md'
check "Read own RESULT.json"              allow ""  Read file_path '/repo/.rddev/workers/T0010/RESULT.json'
check "Read other workers RESULT.json"    block "another Worker" Read file_path '/repo/.rddev/workers/T0009/RESULT.json'
check "Read dispatch harness"             block "dispatch" Read file_path '/repo/.rddev/dispatch/spawn.sh'
check "Read main checkout spec"           allow ""  Read file_path '/repo/specs/orchestrator/worker-permissions.yaml'
check "NotebookRead matcher on .env"      block ".env" NotebookRead notebook_path '/repo/.env'
check "Read dynamic path fails closed"    block "static path" Read file_path '$FOO/x'

# ---------------------------------------------------------------------------
# File-WRITING tools (T1219). These tools were in permissions.allow and absent
# from the PreToolUse matcher, so dontAsk ran them bare and the hook was never
# invoked: the write envelope below did not hold for the two tools most likely
# to be used to leave it. Reproduced live before the fix — a real claude
# session, driven with the generated worker-settings.json, wrote
# .rddev/runtime/tasks/<TASK>/gate-inputs.json with the Write tool and
# reported CREATED-OK. Every rule got a two-sided pair for the same reason the
# shell-write rules did.
#
# They are held to BOTH halves: a Write/Edit names a destination (the write
# half, identical to a shell redirection), and an Edit/NotebookEdit reads the
# file it is about to change — its diff returns to the model — so the read
# confinement applies to it on the same terms as the Read tool.

check "Write into own worktree"           allow ""  Write file_path '/repo/.rddev/worktrees/T0010/notes.txt'
check "Write relative into worktree"      allow ""  Write file_path 'notes.txt'
check "Write own RESULT.json"             allow ""  Write file_path '/repo/.rddev/workers/T0010/RESULT.json'
check "Write to /tmp"                     allow ""  Write file_path '/tmp/t1219/scratch.txt'
check "Write to /etc"                     block "confined" Write file_path '/etc/x'
check "Write to \$HOME"                   block "confined" Write file_path '$HOME/x'
check "Write to ~"                        block "confined" Write file_path '~/x'
check "Write to another workers result dir" block "confined" Write file_path '/repo/.rddev/workers/T0009/RESULT.json'
check "Write unresolved var fails closed" block "static path" Write file_path '$UNKNOWN/x'
check "Write the authoritative runtime record" block "confined" Write file_path '/repo/.rddev/runtime/tasks/T0010/gate-inputs.json'
check "Write .env in own worktree"        block ".env" Write file_path '.env'

check "Edit inside worktree"              allow ""  Edit file_path '/repo/.rddev/worktrees/T0010/main.go'
check "Edit relative in worktree"         allow ""  Edit file_path 'internal/config/x.go'
check "Edit to /etc"                      block "confined" Edit file_path '/etc/hosts'
check "Edit on a credential store"        block "credential store" Edit file_path '~/.ssh/id_rsa'
check "Edit on the claude settings store" block "credential store" Edit file_path '~/.claude/settings.json'
check "Edit on a repo .env"               block ".env" Edit file_path '/repo/.env'
check "Edit another workers worktree"     block "another Worker" Edit file_path '/repo/.rddev/worktrees/T0009/main.go'

check "MultiEdit inside worktree"         allow ""  MultiEdit file_path 'internal/config/x.go'
check "MultiEdit to /etc"                 block "confined" MultiEdit file_path '/etc/hosts'
check "MultiEdit on a credential store"   block "credential store" MultiEdit file_path '~/.netrc'

check "NotebookEdit inside worktree"      allow ""  NotebookEdit notebook_path '/repo/.rddev/worktrees/T0010/nb.ipynb'
check "NotebookEdit to /etc"              block "confined" NotebookEdit notebook_path '/etc/nb.ipynb'
check "NotebookEdit on a credential store" block "credential store" NotebookEdit notebook_path '~/.aws/credentials'
check "NotebookEdit on a repo .env"       block ".env" NotebookEdit notebook_path '/repo/.env'

# The path the fix exists for: collect reads allowed_scope out of
# .rddev/runtime/tasks/<TASK>/gate-inputs.json, so a Worker that can rewrite it
# rewrites the input the gate judges it by.
check "Edit the authoritative runtime record" block "confined" Edit file_path '/repo/.rddev/runtime/tasks/T0010/gate-inputs.json'

# The path field is taken from a real JSON decode, not a text scan, so these
# pin the decoder rather than a live exploit: the payloads below are not
# producible through today's harness, where tool inputs are schema-validated
# before dispatch. A path check should not rest on the current encoding of its
# input, though, and the scan's failure mode is the quiet one — it reads a
# decoy and never examines the real target, so the call is allowed. Raw JSON,
# because `check` builds its own document.
#
# Measured, and the first case records it: a decoy inside an ESCAPED string
# literal does NOT fool the scan (a quote in a JSON string is escaped, so the
# key never appears contiguously inside Write.content). The nested-object and
# duplicate-key shapes do.
check_json "decoy file_path in Write.content does not hide the real target" block "confined" \
	'{"tool_name":"Write","tool_input":{"content":"\"file_path\": \"/repo/.rddev/worktrees/T0010/safe.txt\"","file_path":"/etc/evil"}}'
check_json "decoy nested object ahead of the real file_path" block "confined" \
	'{"tool_name":"Edit","tool_input":{"new_string":{"file_path":"/repo/.rddev/worktrees/T0010/safe.txt"},"file_path":"/etc/evil","old_string":"x"}}'
check_json "duplicate file_path (last wins, as the decoder resolves it)" block "confined" \
	'{"tool_name":"Write","tool_input":{"file_path":"/repo/.rddev/worktrees/T0010/safe.txt","file_path":"/etc/evil","content":"x"}}'
check_json "a legitimate Write through the same shape is still allowed" allow "" \
	'{"tool_name":"Write","tool_input":{"content":"\"file_path\": \"/etc/decoy\"","file_path":"/repo/.rddev/worktrees/T0010/notes.txt"}}'
check_json "unparseable tool input fails closed for a write" block "cannot parse" \
	'{"tool_name":"Write","tool_input":{'
check_json "unparseable tool input fails closed for a read" block "cannot parse" \
	'{"tool_name":"Read","tool_input":{'

# ---------------------------------------------------------------------------
# Fail-closed without the worker contract env: an unset POST_WORKER_WORKTREE
# used to collapse the write allow-pattern to `/*` and admit every absolute
# path; both policies must refuse to decide instead (L1-20260912-3 regression).

check "write without contract env fails closed"  block "contract environment" Bash command 'echo x | tee /etc/x' POST_WORKER_WORKTREE= POST_WORKER_RESULT_DIR=
check "read without contract env fails closed"   block "contract environment" Read file_path '/repo/specs/README.md' POST_REPO_ROOT= POST_WORKER_WORKTREE= POST_WORKER_RESULT_DIR=
check "Write tool without contract env fails closed" block "contract environment" Write file_path '/repo/.rddev/worktrees/T0010/x' POST_REPO_ROOT= POST_WORKER_WORKTREE= POST_WORKER_RESULT_DIR=
check "Edit tool without contract env fails closed"  block "contract environment" Edit file_path '/repo/.rddev/worktrees/T0010/x' POST_REPO_ROOT= POST_WORKER_WORKTREE= POST_WORKER_RESULT_DIR=

# ---------------------------------------------------------------------------
# Background execution (T0011). A Worker that backgrounds long-running work and
# then ends its turn leaves the session with that work still pending: the
# session exits, the work is never done, and whatever RESULT is on disk
# describes a state that never completed. That happened twice, once producing a
# RESULT that falsely claimed success, so it is refused mechanically. These
# cases take raw JSON because the rule keys off a BOOLEAN field
# (`run_in_background`), which the string-only `check` helper cannot express.

check_json "background bash is refused"          block "background execution" \
	'{"tool_name":"Bash","tool_input":{"command":"make test","run_in_background":true}}'
check_json "background bash is refused even for read-only git" block "background execution" \
	'{"tool_name":"Bash","tool_input":{"command":"git status","run_in_background":true}}'
check_json "run_in_background false is allowed"  allow "" \
	'{"tool_name":"Bash","tool_input":{"command":"make test","run_in_background":false}}'
check_json "the same command in the foreground is allowed" allow "" \
	'{"tool_name":"Bash","tool_input":{"command":"make test"}}'

# Adversarial shapes a text scan gets wrong. A security review flagged the
# original raw-text check as fail-open; the specific injection below (the key
# inside the model-controlled command text) does NOT actually work, because a
# quote inside a JSON string is escaped - but duplicate keys and a changed
# serialisation would break a text scan, so the rule parses the JSON instead.
check_json "duplicate run_in_background (true last) is refused" block "background execution" \
	'{"tool_name":"Bash","tool_input":{"run_in_background":false,"run_in_background":true,"command":"make test"}}'
check_json "pretty-printed multi-line payload is refused" block "background execution" \
	'{
  "tool_name": "Bash",
  "tool_input": {
    "command": "make test",
    "run_in_background": true
  }
}'
check_json "the key inside the command text does not fool the rule" block "background execution" \
	'{"tool_name":"Bash","tool_input":{"command":"echo \"run_in_background\": false","run_in_background":true}}'

# ---------------------------------------------------------------------------
# Command POSITION — the SECOND position (T1221).
#
# Every separator the shell honours starts a new command position, and every
# verb family the guard names is refused there exactly as it is refused in the
# first position. That was false for one separator, and this section is the
# two-sided pair for each cell of separator × family.
#
# What happened: the Bash command text was pulled out of the tool-call JSON by
# a text scan (`json_field`) that ate the backslash of an escape and kept the
# next character, so the `\n` of a multi-line command became the LETTER n and
# welded the lines together — `true` + newline + `cp /tmp/x /etc/evil` decoded
# to the single word `truencp /tmp/x /etc/evil`. The tokenizer then saw one
# command with an unknown binary, so every rule keyed off command position was
# bypassed by pressing Enter: not only the write rule, but privilege
# escalation, the control-plane CLIs, git and printenv as well.
#
# The fix is that the command is JSON-decoded (`json_string`), so a newline
# reaches the tokenizer as a newline — which it always handled correctly. The
# rival diagnosis ("command_records only analyses the first line") is wrong,
# and `command_records` is unchanged: fed a real newline it resets command
# position per line, which is what these rows now pin.
#
# Every row runs `true` first, so a refusal can only come from the second
# position. The newline rows pass a REAL newline: `check` encodes the value
# with a JSON encoder, which emits exactly the `\n` escape Claude Code puts on
# the wire, so these rows exercise the wire shape and not a shell convenience.
# shellcheck disable=SC2034  # the loop variables below are read by the loops
nl=$'\n'

SEPARATORS=(
	';:semicolon'
	' | :pipe'
	' && :and-and'
	' || :or-or'
	' &:ampersand'
	"${nl}:newline"
)

# One dangerous use per verb family the guard names, with the fragment its
# refusal must carry (a block for the wrong reason is not a pass).
BLOCK_IN_SECOND=(
	'cp /tmp/x /etc/evil|confined'
	'rm -rf /etc/evil|confined'
	'mv /tmp/x /etc/evil|confined'
	'ln -s /tmp/x /etc/evil|confined'
	'install /tmp/x /etc/evil|confined'
	'tee /etc/evil|confined'
	'sudo ls|privilege escalation'
	'su -|privilege escalation'
	'doas ls|privilege escalation'
	'pkexec ls|privilege escalation'
	'gh auth login|control-plane'
	'glab mr list|control-plane'
	'tea issues|control-plane'
	'git commit -m x|control-plane'
	'printenv GITHUB_TOKEN|credential'
)

# The allowed neighbour of each family, in the same position. sudo and the
# control-plane CLIs have no legitimate use in a Worker at all, so their
# allowed side is the word that must NOT be matched when it is not in command
# position — the suite's existing shape ("echo sudo is not sudo").
ALLOW_IN_SECOND=(
	'cp a b'
	'rm -f file.txt'
	'mv a b'
	'ln -s a b'
	'install -m 644 a b'
	'tee notes.log'
	'git status --short'
	'git log --oneline -5'
	'printenv PATH'
	'echo "sudo"'
	'echo "gh auth login"'
)

# NB: check() assigns its parameters as globals (name/expect/reason/tool/...),
# so this loop keeps its own variables out of those names.
for sep_spec in "${SEPARATORS[@]}"; do
	sepv=${sep_spec%%:*}
	sepname=${sep_spec#*:}
	for row in "${BLOCK_IN_SECOND[@]}"; do
		rowverb=${row%%|*}
		rowreason=${row#*|}
		check "second position after $sepname: $rowverb" block "$rowreason" Bash command "true${sepv}${rowverb}"
	done
	for rowverb in "${ALLOW_IN_SECOND[@]}"; do
		check "allowed in second position after $sepname: $rowverb" allow "" Bash command "true${sepv}${rowverb}"
	done
done

# The raw wire shape, spelled as JSON rather than built by the helper: this is
# the payload Claude Code sends for a two-line command, and the one the text
# scan decoded wrong. The backslash is built rather than written so that the
# payload under test is unambiguous.
bs=$(printf '\\')
check_json "the wire shape of a two-line write is refused" block "confined" \
	"{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"true${bs}ncp /tmp/x /etc/evil\"}}"
check_json "the wire shape of a two-line git command is refused" block "control-plane" \
	"{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"true${bs}ngit commit -m x\"}}"
check_json "the wire shape of a two-line privilege escalation is refused" block "privilege escalation" \
	"{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"true${bs}nsudo ls\"}}"

# The same newline, spelled as a unicode escape — and the same trick applied to
# a verb. Claude Code's encoder emits the two-character newline escape, so no
# harness produces these today; they are here for the reason the path decoder's
# comment gives, that a check should not rest on the current encoding of its
# input. Both spellings decode to the same command text, and the decoded text
# is what the guard decides on. The text scan did not: it kept the escape's
# payload verbatim, so the newline one became the letters "u000a" (welding the
# lines into one word) and the verb one became "u0072m" (hiding `rm` behind an
# unknown binary) — the same two failures as the plain spellings above.
check_json "the newline as a unicode escape is refused" block "confined" \
	"{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"true${bs}u000acp /tmp/x /etc/evil\"}}"
check_json "a verb written as a unicode escape is refused" block "confined" \
	"{\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"${bs}u0072m -rf /etc/evil\"}}"

# ...and an escape that is NOT a separator stays one command. `\t` decodes to a
# tab, which the shell treats as word whitespace, so `true<TAB>cp a b` runs
# `true` with arguments and writes nothing. Pinned in both directions because
# the same decoder change moved both: the mis-decoded `\t` (the letter t) is a
# correctness bug with no bypass behind it, unlike `\n`.
tab=$(printf '\t')
check "a tab is whitespace, not a command position (write)"  allow "" Bash command "true${tab}cp a b"
check "a tab is whitespace, not a command position (git)"    allow "" Bash command "true${tab}git status --short"
check "a tab is whitespace, not a command position (sudo)"   allow "" Bash command "true${tab}echo sudo"

# ---------------------------------------------------------------------------
# A payload the guard cannot read is REFUSED (T1221). This is the arm the
# newline hole got in through the other way round: the text scan's verdict on a
# document it could not read was "no command", and "no command" was allowed
# through. The tool name chooses the policy branch, so an unreadable document is
# refused before the branch is even chosen.

check_json "unparseable tool-call JSON fails closed before the branch" block "cannot parse" \
	'{"tool_name":'
check_json "unparseable tool input fails closed for a shell command" block "cannot parse" \
	'{"tool_name":"Bash","tool_input":{"command":"rm -rf /etc/evil"'
check_json "a Bash call with no command string is refused" block "no command string" \
	'{"tool_name":"Bash","tool_input":{"timeout":120000}}'
check_json "an empty command is refused" block "no command string" \
	'{"tool_name":"Bash","tool_input":{"command":""}}'
check_json "a null command reads as no command, not as a shape mismatch" block "no command string" \
	'{"tool_name":"Bash","tool_input":{"command":null}}'

# ...and the SHAPE arm, which is the other half of the same rule. A field the
# guard reads that is PRESENT but holds something other than the string it needs
# is refused exactly like a document that does not parse: "there is a value here
# and I cannot read it" is not the same fact as "there is no value here", and
# letting the first through because reading it has no obvious answer is the same
# fail-open shape the newline hole came through. The measured basis for
# refusing it is in the guard (54031 recorded tool calls in this repo's worker
# logs: the fields read here are strings or absent every time, so the refusal
# costs no real call).
check_json "a non-string command is refused" block "cannot parse" \
	'{"tool_name":"Bash","tool_input":{"command":{"cmd":"rm -rf /etc/evil"}}}'
check_json "a non-string tool name is refused" block "cannot parse" \
	'{"tool_name":17,"tool_input":{"command":"true"}}'
check_json "a non-string run_in_background is refused" block "cannot parse" \
	'{"tool_name":"Bash","tool_input":{"command":"true","run_in_background":"yes"}}'
check_json "a path field that is a nested object is refused" block "cannot parse" \
	'{"tool_name":"Read","tool_input":{"file_path":{"path":"/etc/evil"}}}'
check_json "a path field that is a list is refused" block "cannot parse" \
	'{"tool_name":"Write","tool_input":{"file_path":["/etc/evil"],"content":"x"}}'

# The other side of that rule: an UNKNOWN tool name is not a broken payload.
# No path policy applies to a tool that names no path, and refusing it would be
# the false positive this hook must not become.
check_json "an unknown tool name is still allowed (no policy applies)" allow "" \
	'{"tool_name":"TodoWrite","tool_input":{"todos":[]}}'

# ---------------------------------------------------------------------------
# What the read envelope does NOT reach (T1221). The guard reads the command
# TEXT; it does not run the shell, so what a shell command goes on to read is
# not inspected. The Worker contract says so in those words, and
# TestTheGuardClaimInTheSystemPromptIsTrue drives this same guard to keep the
# sentence and the behaviour together: widening the confinement to the shell
# has to move the sentence in the same commit, and these two cases are what
# makes that failure visible.

check "shell read of a credential store is not inspected"      allow "" Bash command 'cat ~/.ssh/id_rsa'
check "shell read of the harness runtime state is not inspected" allow "" Bash command 'cat /repo/.rddev/runtime/tasks/T0010/gate-inputs.json'

# ---------------------------------------------------------------------------

if [ "$fail" -gt 0 ]; then
	printf 'guard-regression: %d/%d PASS, %d FAILED\n' "$pass" "$((pass + fail))" "$fail" >&2
	printf '%b\n' "$failed_cases" >&2
	exit 1
fi
printf 'guard-regression: %d/%d PASS\n' "$pass" "$pass"
