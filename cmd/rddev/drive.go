package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lichman0405/post/internal/devorchestrator"
)

const driveUsage = `Usage:
  rddev drive [--parallel N] [--poll DUR] [--once] [--worker-timeout DUR]
  rddev drive --clear-decision TASK
  rddev status [--json]

drive runs the persistent Supervisor loop: it adopts whatever Workers already
exist, collects the ones that finished, and carries each task through review,
acceptance, commit, pull request and merge — dispatching more work as the DAG
allows and the parallelism limit permits.

It is a long-lived process by design. A driver that lives inside a Supervisor
conversation stops when the conversation does, leaving finished Workers
uncollected with nobody to notice; this one holds an exclusive lock (a second
driver refuses, naming the holder), writes a heartbeat, and records on disk
whatever needs a judgement, so the Supervisor can be absent and return.

It never lowers a gate: every action is performed by invoking rddev itself, so
the driver chooses what to attempt and rddev refuses on its own terms.

status reports the driver (alive or dead, from the heartbeat), the Workers it
can see, and the decisions waiting for the Supervisor.

--clear-decision TASK drops the decisions recorded for one task. A decision
bracketed to a run clears itself the moment a rework or respawn supersedes that
attempt; one recorded for a task that was never spawned has no run to change
(a phase refusing dispatch because it had no G3, say), so the Supervisor says
the condition is gone rather than the driver guessing.
`

func runDrive(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, driveUsage)
		return exitOK
	}
	vals, pos, err := parseFlags(args,
		flagSpec{"--json", false},
		flagSpec{"--parallel", true},
		flagSpec{"--poll", true},
		flagSpec{"--once", false},
		flagSpec{"--worker-timeout", true},
		flagSpec{"--tasks-json", true},
		flagSpec{"--state-json", true},
		flagSpec{"--gates", true},
		flagSpec{"--repo-root", true},
		flagSpec{"--clear-decision", true},
	)
	if err != nil {
		return usageError(stderr, err.Error(), driveUsage)
	}
	if len(pos) > 0 {
		return usageError(stderr, fmt.Sprintf("rddev drive: unexpected argument %q", pos[0]), driveUsage)
	}
	repoRoot := vals["--repo-root"]
	if repoRoot == "" {
		var err error
		if repoRoot, err = os.Getwd(); err != nil {
			return operationalError(stderr, "rddev drive", err)
		}
	}
	opts := &devorchestrator.DriveOpts{
		RepoRoot:  repoRoot,
		DagPath:   vals["--tasks-json"],
		StatePath: vals["--state-json"],
		GatesPath: vals["--gates"],
		Parallel:  2,
		Poll:      20 * time.Second,
		Out:       stdout,
	}
	if v := vals["--parallel"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return usageError(stderr, "--parallel must be an integer", driveUsage)
		}
		opts.Parallel = n
	}
	if v := vals["--poll"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return usageError(stderr, "--poll must be a duration (e.g. 20s)", driveUsage)
		}
		opts.Poll = d
	}
	if v := vals["--worker-timeout"]; v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return usageError(stderr, "--worker-timeout must be a duration", driveUsage)
		}
		opts.WorkerTimeout = d
	}
	if _, ok := vals["--once"]; ok {
		opts.Once = true
	}
	// A decision recorded for a task that was NEVER spawned has no run to
	// change, so it cannot clear itself when the condition behind it goes away
	// (P6 refusing dispatch because the phase had no G3, say). The Supervisor
	// resolves that by saying so — the condition is theirs to judge.
	if task := vals["--clear-decision"]; task != "" {
		if err := devorchestrator.ClearDecisions(repoRoot, task, ""); err != nil {
			return operationalError(stderr, "rddev drive", err)
		}
		fmt.Fprintf(stdout, "cleared decisions for %s\n", task)
		return exitOK
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	err = opts.Drive(ctx)
	if err != nil && err != context.Canceled {
		return operationalError(stderr, "rddev drive", err)
	}
	if opts.Once {
		reportDriverStatus(stdout, repoRoot, jsonOut)
	}
	return exitOK
}

// runStatus prints the driver, the Workers and the open decisions.
func runStatus(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) {
		fmt.Fprint(stdout, driveUsage)
		return exitOK
	}
	vals, pos, err := parseFlags(args, flagSpec{"--json", false},
		flagSpec{"--tasks-json", true}, flagSpec{"--state-json", true},
		flagSpec{"--repo-root", true})
	if err != nil {
		return usageError(stderr, err.Error(), driveUsage)
	}
	if _, ok := vals["--json"]; ok {
		jsonOut = true
	}
	if len(pos) > 0 {
		return usageError(stderr, fmt.Sprintf("rddev status: unexpected argument %q", pos[0]), driveUsage)
	}
	repoRoot := vals["--repo-root"]
	if repoRoot == "" {
		var err error
		if repoRoot, err = os.Getwd(); err != nil {
			return operationalError(stderr, "rddev status", err)
		}
	}
	reportDriverStatus(stdout, repoRoot, jsonOut)
	return exitOK
}

// reportDriverStatus is the answer to "is anything still running, and is
// anything waiting for me?" — from disk, so it is the same answer whether the
// Supervisor's session is live, and it can tell a crashed driver from a quiet
// one by the heartbeat rather than by looking for a process.
func reportDriverStatus(stdout io.Writer, repoRoot string, jsonOut bool) {
	now := time.Now().UTC()
	st, _ := devorchestrator.ReadDriverStatus(repoRoot)
	decisions, _ := devorchestrator.ReadDecisions(repoRoot)
	workers, _ := devorchestrator.DiscoverWorkers(repoRoot)

	type workerLine struct {
		Task   string `json:"task"`
		Status string `json:"status"`
		PID    int    `json:"pid,omitempty"`
		Log    string `json:"log,omitempty"`
	}
	var running []workerLine
	for _, w := range workers {
		if w.Status != devorchestrator.WorkerRunning {
			continue
		}
		running = append(running, workerLine{Task: w.Record.TaskID, Status: "running", PID: w.Record.PID, Log: w.LogPath})
	}
	driverState := "dead"
	age := ""
	if st != nil && st.Alive(now) {
		driverState = "alive"
		if hb, err := time.Parse(time.RFC3339, st.HeartbeatAt); err == nil {
			age = now.Sub(hb).Truncate(time.Second).String()
		}
	} else if st != nil {
		driverState = "dead (heartbeat stale)"
	}

	if jsonOut {
		out, _ := json.MarshalIndent(struct {
			Driver    string                     `json:"driver"`
			Heartbeat string                     `json:"heartbeat_age,omitempty"`
			PID       int                        `json:"driver_pid,omitempty"`
			Running   []workerLine               `json:"running_workers"`
			Decisions []devorchestrator.Decision `json:"waiting_decisions"`
		}{driverState, age, pidOf(st), running, decisions}, "", "  ")
		fmt.Fprintln(stdout, string(out))
		return
	}

	fmt.Fprintf(stdout, "driver:  %s", driverState)
	if st != nil {
		fmt.Fprintf(stdout, " (pid %d", st.PID)
		if age != "" {
			fmt.Fprintf(stdout, ", heartbeat %s ago", age)
		}
		fmt.Fprintf(stdout, ")")
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "workers: %d running\n", len(running))
	for _, w := range running {
		fmt.Fprintf(stdout, "  %s pid=%d\n", w.Task, w.PID)
	}
	fmt.Fprintf(stdout, "decisions waiting for the Supervisor: %d\n", len(decisions))
	for _, d := range decisions {
		fmt.Fprintf(stdout, "  %s %s: %s\n", d.Task, d.Action, firstLineOf(d.Reason))
	}
}

func pidOf(st *devorchestrator.DriverStatus) int {
	if st == nil {
		return 0
	}
	return st.PID
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

const rebaselineUsage = `Usage: rddev rebaseline TASK [--reason-file FILE] [--json]

Advance a task's baseline onto main while keeping its work, then send it back
for re-verification: baseline frozen -> changed, the Worker is rejected with the
reason, and reworked on the new baseline.

Why it exists: the task branch IS the baseline, and main moves under a task while
it is reviewed — the Supervisor merges other work, and the scratch gate then
correctly refuses a change that no longer composes. Doing the ten-step git
sequence by hand, five times, is not the correct part of that.

Generated files are handled by REGENERATION, never by merging text. A derived
artifact is a function of its inputs: applying the branch's copy would restore a
digest describing the branch's old specs, and applying main's would drop the
task's spec edits. Both are wrong; the artifact is recomputed from the merged
tree instead, which is what declaring it derived means.

The advance is refused, leaving the worktree untouched, when the task's change
does not apply even with generated files excluded, or when a path the task had
changed would be lost.
`

func runRebaseline(args []string, stdout, stderr io.Writer, jsonOut bool) int {
	if wantsHelp(args) || len(args) == 0 {
		fmt.Fprint(stdout, rebaselineUsage)
		if wantsHelp(args) {
			return exitOK
		}
		return usageError(stderr, "rddev rebaseline: missing TASK", rebaselineUsage)
	}
	vals, pos, err := parseFlags(args,
		flagSpec{"--json", false},
		flagSpec{"--tasks-json", true}, flagSpec{"--state-json", true},
		flagSpec{"--repo-root", true}, flagSpec{"--reason-file", true},
	)
	if err != nil {
		return usageError(stderr, err.Error(), rebaselineUsage)
	}
	if _, ok := vals["--json"]; ok {
		jsonOut = true
	}
	if len(pos) != 1 {
		return usageError(stderr, "rddev rebaseline: requires exactly one TASK", rebaselineUsage)
	}
	task := pos[0]
	repoRoot := vals["--repo-root"]
	if repoRoot == "" {
		if repoRoot, err = os.Getwd(); err != nil {
			return operationalError(stderr, "rddev rebaseline", err)
		}
	}
	dagPath := stringOr(vals["--tasks-json"], devorchestrator.DefaultDAGPath)
	statePath := stringOr(vals["--state-json"], devorchestrator.DefaultStatePath)

	res, err := devorchestrator.RebaselineTask(repoRoot, task, dagPath, statePath)
	if err != nil {
		return operationalError(stderr, "rddev rebaseline", err)
	}
	if jsonOut {
		out, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return operationalError(stderr, "rddev rebaseline", err)
		}
		fmt.Fprintln(stdout, string(out))
	} else {
		fmt.Fprintf(stdout, "%s: baseline %s -> %s (%d file(s) carried", task, res.FromSHA[:12], res.ToSHA[:12], res.Files)
		if len(res.Regenerated) > 0 {
			fmt.Fprintf(stdout, "; regenerated %s", strings.Join(res.Regenerated, ", "))
		}
		fmt.Fprintln(stdout, ")")
	}

	// Hand it back for re-verification with a reason that says what happened —
	// the Worker did nothing wrong, and a reason that reads like a defect would
	// send it looking for one.
	reason := vals["--reason-file"]
	if reason == "" {
		reason = filepath.Join(os.TempDir(), "post-rebaseline-reason.txt")
		_ = os.WriteFile(reason, []byte(rebaselineReason(res)), 0o600)
		defer os.Remove(reason)
	}
	// A task the Supervisor parked as rejected has no rejected -> rejected
	// transition, so `task reject` cannot record the reason — and the reason
	// still has to reach the Worker. `worker rework --reason-file` (#126) is the
	// door for exactly that case: it writes the RejectRecord itself before the
	// dispatch. Regenerating a reason is the same either way, so ask the state
	// rather than reading the refusal text.
	parked, err := taskIsRejected(dagPath, statePath, task)
	if err != nil {
		return operationalError(stderr, "rddev rebaseline", err)
	}
	rework := rebaselineReworkArgs(task, reason, parked)
	if parked {
		// The reason rides the rework — and it is still recorded, because the
		// rework writes it as a RejectRecord before it dispatches.
	} else if out, code := devorchestrator.RunRDDev(repoRoot, dagPath, statePath, "task", "reject", task, "--reason-file", reason); code != 0 {
		return operationalError(stderr, "rddev rebaseline: the tree advanced but the rejection failed", fmt.Errorf("task reject %s: %s", task, strings.TrimSpace(out)))
	}
	if out, code := devorchestrator.RunRDDev(repoRoot, dagPath, statePath, rework...); code != 0 {
		return operationalError(stderr, "rddev rebaseline: the tree advanced but the rework failed", fmt.Errorf("worker rework %s: %s", task, strings.TrimSpace(out)))
	}
	if parked {
		fmt.Fprintf(stdout, "%s: was already rejected; the reason was re-recorded and it is reworking on %s\n", task, res.ToSHA[:12])
	} else {
		fmt.Fprintf(stdout, "%s: rejected (baseline advanced, not a defect) and reworking on %s\n", task, res.ToSHA[:12])
	}
	return exitOK
}

// rebaselineReworkArgs builds the rework command that follows a baseline
// advance. When the task was already rejected the reason has to ride this
// command, because the step that would normally record it — `task reject` — has
// no rejected -> rejected transition (#126).
func rebaselineReworkArgs(task, reason string, parked bool) []string {
	args := []string{"worker", "rework", task, "--timeout", "60m"}
	if parked {
		args = append(args, "--reason-file", reason)
	}
	return args
}

// taskIsRejected reports whether the task is sitting in `rejected` — the state
// the Supervisor parks a task in when it must wait for another merge, and the
// one state `task reject` cannot write to again. Asked of the state file rather
// than inferred from a refusal's wording.
func taskIsRejected(dagPath, statePath, task string) (bool, error) {
	store, err := devorchestrator.OpenStore(dagPath, statePath)
	if err != nil {
		return false, fmt.Errorf("reading the state of %s: %w", task, err)
	}
	insp, err := store.Inspect(task)
	if err != nil {
		return false, fmt.Errorf("inspecting %s: %w", task, err)
	}
	return insp.State.Status == devorchestrator.StateRejected, nil
}

func rebaselineReason(res *devorchestrator.RebaselineResult) string {
	return "【这不是缺陷 —— 基线推进，请在新基线上重新验证并重新提交】\n\n" +
		"你的交付在本地的合并态 Gate 上被拒，原因是**你的改动无法应用到当前 main**。\n" +
		"**这是 Gate 在正确工作**：G2 现在验证的是「当前 main + 本任务完整改动」，而不是你 worktree 里那棵孤立的树。\n" +
		"你运行期间 main 前进了（Supervisor 合并了别的工作），这是正常的。\n\n" +
		"**Supervisor 已完成**：你的任务分支已合并当前 main（" + res.ToSHA[:12] + "）" +
		"，你的完整改动原样重新应用，改动集合逐个文件比对一致" +
		regeneratedNote(res.Regenerated) + "。\n\n" +
		"【你要做的】在合并后的基线上重跑 required tests 与 make test-integration，" +
		"确认没有因合并而失效；确认 RESULT.json 自洽（completed 要求 tests[] 全部 passed，" +
		"覆盖任务要求的条目带 label）；若一切照旧，原样重新提交 RESULT.json。\n\n" +
		"【不要做的事】不要为了让 Gate 变绿而改动或放宽任何测试；不要删除测试；不要趁机重构无关代码。\n"
}

func regeneratedNote(regenerated []string) string {
	if len(regenerated) == 0 {
		return ""
	}
	return "；唯一被排除的是生成物 " + strings.Join(regenerated, ", ") +
		"（文本冲突对生成物没有意义，已在合并后的树上重新生成）"
}
