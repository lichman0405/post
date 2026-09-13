package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
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
