# .rddev.example — worker runtime layout

`rddev worker spawn` creates the Supervisor's runtime state under `.rddev/`
(gitignored, docs/65). This directory is the **checked-in reference copy** of
the artifacts that layout produces, so reviewers can see exactly what a Worker
receives without spawning one. The copies are drift-checked against the real
generators (`rddev` embeds `worker-guard.sh`; a test keeps this copy and the
embedded one identical).

## What spawn creates, per task

```
.rddev/
├── worktrees/<TASK_ID>/        Supervisor-owned git worktree (one per task)
└── workers/<TASK_ID>/
    ├── registry.json           run facts: pid, starttime, session, claude
    │                           version, model, budget, baseline, refs, paths
    ├── exit.status             claude's exit code (written by run-worker.sh)
    ├── claude.pid              claude's pid (forensics; registry is canonical)
    ├── worker.log              the claude stream-json log
    ├── task-package.json       the validated task package (schema-checked)
    ├── prompt.md               the rendered worker prompt
    ├── system.md               the rendered worker contract
    ├── worker-settings.json    Claude Code permissions + PreToolUse hook
    ├── run-worker.sh           reaper wrapper (survives rddev exit, records exit)
    ├── gh-config/              neutralised GH_CONFIG_DIR (empty, no creds)
    └── guard/worker-guard.sh   the PreToolUse isolation guard (see below)
```

## guard/worker-guard.sh

The canonical isolation guard, embedded in the rddev binary and copied here.
It is driven by Claude Code as a PreToolUse hook (`worker-settings.json`) and
enforces:

- git control-plane subcommands blocked in **command position only**
  (`echo "git commit"` and `grep "push" file` are not matches). Every
  separator the shell honours starts a new command position — `;`, `|`, `&&`,
  `||`, `&` and a newline — so the same verb on a second line is refused
  exactly as it is on the first (T1221: the command text is JSON-decoded, so
  the `\n` of a multi-line command reaches the tokenizer as a newline);
- gh/glab/tea CLIs and sudo/su/doas/pkexec blocked;
- Docker socket access blocked except version queries, unless the task was
  spawned with `--docker` (POST_WORKER_DOCKER_GRANT=1);
- file-tool reads confined: credential stores, `.env` files (except
  `.env.example`) and other Workers' runtime state blocked for Read/Grep/
  Glob/NotebookRead (and for the file-writing tools, which read what they
  change). The guard decides on the command TEXT, so what a *shell* command
  reads is not inspected (`cat ~/.ssh/id_rsa` passes the hook) — the limit the
  Worker contract states in the same words;
- writes confined to the Worker's own worktree, its result dir and `/tmp`,
  whichever tool makes them — shell writes (rm/cp/mv/ln/install/tee,
  `>`/`>>` redirections) and the file-writing tools (Write/Edit/MultiEdit/
  NotebookEdit) go through the same normalisation and the same allow-set;
  unresolvable `$` paths fail closed;
- missing contract environment (POST_*) fails closed, and so does a tool call
  the guard cannot read: unparseable JSON, or a tool name / command / path
  field holding something other than the string the rule needs. Only an
  unknown tool name is allowed through (no path or command policy applies to a
  tool that names neither).

The matcher above decides WHICH tools reach the hook, and a tool missing from
it is a tool the guard never sees whatever the list above claims — Write and
Edit were in `permissions.allow` and absent from the matcher until T1219, so
`dontAsk` ran them bare to any path. `TestWriteGuardFiles` derives the
matcher's required contents from `permissions.allow`, and
`TestGuardMatcherNamesExactlyTheKnownTools` fixes the set in both
directions, so that seam cannot be reopened quietly; the tools that name a path but are refused by the
permission layer today (Read/Grep/Glob/NotebookRead/NotebookEdit/MultiEdit)
stay in the matcher so the envelope does not rest on that accident.

Every rule has a two-sided regression test: the dangerous action is blocked
AND the legitimate neighbouring action is allowed
(`tests/worker-guard/guard-regression.sh`). That suite is executed by
`TestGuardRegressionSuiteRunsWhereCIRunsIt`, which runs it against the
embedded script on every `go test ./...` — including CI's `go` job — so it is
a gate rather than a file nothing reads.

## worker-settings.json (example)

```json
{
  "permissions": {
    "defaultMode": "dontAsk",
    "allow": ["Bash", "Write", "Edit"],
    "deny": [
      "Bash(git commit:*)",
      "Bash(git push:*)",
      "Bash(git worktree:*)",
      "Bash(gh:*)",
      "Bash(sudo:*)"
    ],
    "ask": []
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash|Read|Grep|Glob|NotebookRead|Write|Edit|MultiEdit|NotebookEdit",
        "hooks": [{"type": "command", "command": "sh /ABS/PATH/guard/worker-guard.sh"}]
      }
    ]
  }
}
```

The full deny list is generated by spawn (`denyRules()`); the example shows
its shape. The deny layer is blunt, the hook is the precision layer
(L1-20260912-9): e.g. `git branch --list` is allowed by the hook although
`git branch -D` is denied twice over.

## registry.json (example)

```json
{
  "task_id": "T0001",
  "run_id": "run-0123456789abcdef",
  "session_id": "a1b2c3d4-…",
  "claude_version": "2.1.269 (Claude Code)",
  "model": "",
  "effort": "",
  "pid": 12345,
  "start_time": 9876543,
  "worktree": "/repo/.rddev/worktrees/T0001",
  "branch": "task/T0001-source-repo-preflight",
  "baseline_sha": "0123456789abcdef…",
  "refs_before": ["refs/heads/main 0123456…"],
  "log_path": "/repo/.rddev/workers/T0001/worker.log",
  "result_dir": "/repo/.rddev/workers/T0001",
  "started_at": "2026-09-12T10:00:00Z",
  "ended_at": "2026-09-12T10:20:00Z",
  "exit_status": 0,
  "exit_source": "reconciled"
}
```

`start_time` is the `/proc/<pid>/stat` field-22 process start time — pairing
it with the pid defeats pid reuse: a recycled pid reads as `stale`, never as
`running`. `status` itself is derived, not stored: `running` (pid alive with
matching starttime) / `exited` (exit recorded) / `stale` (pid gone, no exit
recorded — never treated as completed). The schema is
`specs/orchestrator/worker-registry.schema.json`.
