package devorchestrator

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DriveOpts parametrizes the persistent driver.
type DriveOpts struct {
	RepoRoot, DagPath, StatePath, GatesPath string
	Binary                                  string // the rddev executable actions are run with
	Parallel                                int
	Poll                                    time.Duration
	WorkerTimeout                           time.Duration
	Once                                    bool // one tick then return (tests, cron-style use)
	Out                                     io.Writer
}

func (o *DriveOpts) logf(format string, a ...any) {
	if o.Out == nil {
		return
	}
	fmt.Fprintf(o.Out, "%s drive: %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, a...))
}

// run executes rddev itself. Nothing the driver does bypasses the CLI: it
// decides WHAT to attempt, and rddev refuses on its own terms exactly as it
// would for a human. That is the property that keeps automation from lowering a
// gate.
func (o *DriveOpts) run(args ...string) (string, int) {
	// The DAG/state overrides are PER-COMMAND flags, not globals: placed before
	// the subcommand they are read as the subcommand, and every action fails
	// with a usage error. That is exactly what happened - the driver recorded
	// twelve of them as decisions and then sat looking alive, because a
	// recorded decision (correctly) stops it retrying. Written once, here,
	// because there is only one place that assembles a command.
	cmd := exec.Command(o.binary(), o.rddevArgs(args)...)
	cmd.Dir = o.RepoRoot
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	return string(out), -1
}

// rddevArgs assembles one rddev invocation. Pure, and separate, because the
// order is the whole correctness of it: --tasks-json before the subcommand is
// read AS the subcommand, which turned every action the driver took into a
// usage error — recorded as a decision, which then (correctly) stopped it
// retrying. The driver looked alive and did nothing for ten minutes.
func (o *DriveOpts) rddevArgs(args []string) []string {
	return append(append([]string{}, args...), "--tasks-json", o.dagPath(), "--state-json", o.statePath())
}

func (o *DriveOpts) dagPath() string {
	if o.DagPath != "" {
		return o.DagPath
	}
	return DefaultDAGPath
}
func (o *DriveOpts) statePath() string {
	if o.StatePath != "" {
		return o.StatePath
	}
	return DefaultStatePath
}
func (o *DriveOpts) binary() string {
	if o.Binary != "" {
		return o.Binary
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return "rddev"
}

// Drive runs until the DAG is exhausted, every remaining task is waiting on a
// Supervisor decision, or ctx is cancelled. It is the long-lived process; the
// Supervisor's session is a window onto it, not a precondition for it.
func (o *DriveOpts) Drive(ctx context.Context) error {
	lock, err := AcquireDriverLock(o.RepoRoot)
	if err != nil {
		return err
	}
	defer lock.Release()

	if o.Parallel <= 0 {
		o.Parallel = 2
	}
	if o.Poll <= 0 {
		o.Poll = 20 * time.Second
	}
	st := &DriverStatus{PID: os.Getpid(), StartedAt: nowRFC3339(), Parallel: o.Parallel, State: "starting"}
	_ = WriteDriverStatus(o.RepoRoot, st)
	o.logf("started (pid %d, parallel %d)", st.PID, o.Parallel)

	// Adoption, not assumption: whatever state the repository is in — Workers
	// already running from a previous driver, a Worker that finished while
	// nothing was watching, a decision left open — the first tick reads it from
	// disk and continues from there. Nothing is re-spawned that already exists.
	_, _ = DiscoverWorkers(o.RepoRoot)

	// The ref ledger is part of that adoption. Collect exempts a new ref only
	// when it is on the Supervisor's record, and a dispatch that predates the
	// ledger would look like a ref its sibling created — a false finding
	// produced by the fix that introduced the ledger. Recording the branches of
	// the dispatches already on disk, once, at startup, closes that window
	// (ref_ledger.go: ReconcileSupervisorRefs).
	if refs, err := ReconcileSupervisorRefs(o.RepoRoot); err != nil {
		o.logf("ref ledger reconcile failed: %v (collects may report unattributable new refs until this is fixed)", err)
	} else if len(refs) > 0 {
		o.logf("ref ledger: %d existing dispatch branch(es) recorded", len(refs))
	}

	for {
		acted, err := o.tick(st)
		st.HeartbeatAt = nowRFC3339()
		_ = WriteDriverStatus(o.RepoRoot, st)
		if err != nil {
			return err
		}
		if o.Once {
			return nil
		}
		if !acted {
			done, err := o.exhausted()
			if err != nil {
				return err
			}
			if done {
				st.State = "idle"
				_ = WriteDriverStatus(o.RepoRoot, st)
				o.logf("nothing to do: no dispatchable work, no running Worker, no open decision")
				return nil
			}
		}
		select {
		case <-ctx.Done():
			st.State = "stopping"
			_ = WriteDriverStatus(o.RepoRoot, st)
			return ctx.Err()
		case <-time.After(o.Poll):
		}
	}
}

// exhausted reports whether there is nothing left the driver can act on.
func (o *DriveOpts) exhausted() (bool, error) {
	running, err := o.runningTasks()
	if err != nil {
		return false, err
	}
	if len(running) > 0 {
		return false, nil
	}
	out, _ := o.run("task", "next")
	if strings.TrimSpace(out) != "" {
		return false, nil
	}
	ds, err := ReadDecisions(o.RepoRoot)
	if err != nil {
		return false, err
	}
	// Remaining decisions are for the Supervisor; they are not work for us.
	return len(ds) == 0, nil
}

// runningTasks lists tasks whose Worker has been dispatched and not yet
// collected — the set the parallelism limit counts.
//
// It returns an error rather than an empty slice on failure. Swallowing it made
// the driver answer "no running tasks" to a question it had failed to ask, and
// then sit in a loop looking healthy while a finished Worker waited forever:
// exactly the silent no-op this driver exists to remove.
func (o *DriveOpts) runningTasks() ([]string, error) {
	store, err := OpenStore(o.dagPath(), o.statePath())
	if err != nil {
		return nil, fmt.Errorf("reading the task state: %w", err)
	}
	var out []string
	for _, id := range store.dag.IDs() {
		insp, err := store.Inspect(id)
		if err != nil || insp.State.Status != StateRunning {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

// tick performs at most one action per task and returns whether it did anything.
func (o *DriveOpts) tick(st *DriverStatus) (bool, error) {
	// Reconcile every tick. The registry's exit_status is written by
	// DiscoverWorkers, not by the reaper, so a driver that only reconciles at
	// startup never notices a Worker exiting — it reports "still working"
	// forever. That was masked while the Supervisor happened to run `rddev
	// status` by hand, which reconciles as a side effect: the driver depended on
	// someone else to tell it the news.
	if _, err := DiscoverWorkers(o.RepoRoot); err != nil {
		return false, fmt.Errorf("reconciling Workers: %w", err)
	}
	if err := staleDecisions(o.RepoRoot); err != nil {
		return false, err
	}
	open, err := ReadDecisions(o.RepoRoot)
	if err != nil {
		return false, err
	}
	running, err := o.runningTasks()
	if err != nil {
		return false, err
	}
	st.RunningTasks = running

	// Every task the driver still owes something, not only the running ones.
	// Collecting and then stopping — because verification and accepted were
	// never revisited — was the first version of this loop: it did the one
	// step whose input is a running Worker and left everything after it
	// untouched, which is a pipeline that stops at the first gate.
	pending, err := o.tasksNeedingAction()
	if err != nil {
		return false, err
	}
	if len(pending) > 0 {
		o.logf("pending %v", pending)
	}

	acted := false
	for _, id := range pending {
		if len(OpenDecisionsFor(open, id)) > 0 {
			continue // waiting on the Supervisor; retrying would just re-fail
		}
		did, err := o.stepTask(id, st)
		if err != nil {
			return acted, err
		}
		acted = acted || did
	}

	since, err := o.runningTasks()
	if err != nil {
		return false, err
	}
	if len(since) < o.Parallel {
		open, err = ReadDecisions(o.RepoRoot)
		if err != nil {
			return false, err
		}
		if did := o.dispatch(st, open); did {
			acted = true
		}
	}
	return acted, nil
}

// stepTask advances one collected-or-running task by exactly one action.
func (o *DriveOpts) stepTask(id string, st *DriverStatus) (bool, error) {
	store, err := OpenStore(o.dagPath(), o.statePath())
	if err != nil {
		return false, err
	}
	insp, err := store.Inspect(id)
	if err != nil {
		return false, err
	}
	switch insp.State.Status {
	case StateRunning:
		rec, err := LoadRegistry(o.RepoRoot, id)
		if err != nil {
			return false, fmt.Errorf("reading the Worker record for %s: %w", id, err)
		}
		if rec == nil {
			o.logf("%s is running with no Worker record — nothing to collect", id)
			return false, nil
		}
		if rec.ExitStatus == nil {
			o.logf("%s still working", id)
			return false, nil
		}
		o.logf("%s's Worker exited (%d) — collecting", id, *rec.ExitStatus)
		if out, code := o.run("worker", "collect", id); code != 0 {
			return true, o.decide(id, "collect", out)
		}
		o.logf("%s collected", id)
		st.LastAction = time.Now().Format(time.RFC3339) + " collect " + id
		return true, nil

	case StateVerification:
		return o.stepVerification(id, st)
	case StateAccepted:
		return o.stepAccepted(id, st)
	}
	return false, nil
}

// stepVerification moves a collected task through review and acceptance.
func (o *DriveOpts) stepVerification(id string, st *DriverStatus) (bool, error) {
	reviewID := ReviewTaskID(id)
	rec, err := LoadRegistry(o.RepoRoot, reviewID)
	if err != nil {
		return false, err
	}
	if rec == nil {
		if out, code := o.run("review", "spawn", id); code != 0 {
			return true, o.decide(id, "review-spawn", out)
		}
		o.logf("%s review dispatched", id)
		return true, nil
	}
	if rec.ExitStatus == nil {
		return false, nil // reviewer still working
	}
	// A verdict describes the code it was produced against, and a rework
	// changes that code. Collecting a review of a superseded attempt fails
	// forever — the reviewer exited and its verdict cannot change — so the
	// decision it used to raise was the driver handing the Supervisor a
	// condition only the driver could fix. Ask again instead (L1-20260913-17).
	//
	// Failing to ANSWER that question is not a reason to stop: returning this
	// error ends the long-lived driver (Drive propagates it), so one unreadable
	// file becomes a dead pipeline that still looks alive. Falling through
	// cannot lower a gate — collect asks the same question of the same facts
	// and refuses on its own terms, and that refusal is recorded as a decision
	// the Supervisor sees.
	if stale, why, err := ReviewIsStale(o.RepoRoot, id); err != nil {
		o.logf("%s: cannot tell whether the review is superseded (%v) — leaving that judgement to collect", id, err)
	} else if stale {
		o.logf("%s: %s — dispatching a fresh review", id, why)
		if out, code := o.run("review", "spawn", id); code != 0 {
			return true, o.decide(id, "review-spawn", out)
		}
		return true, nil
	}
	if _, code := o.run("review", "collect", id); code != 0 {
		return true, o.decide(id, "review-collect", "the reviewer's verdict needs a decision (see the report in .rddev/runtime/gates/"+id+")")
	}
	if out, code := o.run("task", "accept", id); code != 0 {
		return true, o.decide(id, "accept", out)
	}
	o.logf("%s accepted", id)
	// The merge happens in stepAccepted, on a later tick, so a red CI never
	// blocks the rest of the pipeline.
	return true, nil
}

// stepAccepted takes an accepted task through commit, PR and merge.
func (o *DriveOpts) stepAccepted(id string, st *DriverStatus) (bool, error) {
	// Commit refuses when the tree is unchanged, which is the normal case after
	// a verification-only rework: not an error.
	_, _ = o.run("git", "commit", id)
	if out, code := o.run("git", "push", id); code != 0 {
		return true, o.decide(id, "push", out)
	}
	_, _ = o.run("pr", "open", id) // an existing PR is fine

	branch := TaskBranch(id, "")
	if out, code := o.run("pr", "status", id); code != 0 {
		// Not a decision yet: it may simply be waiting on CI.
		_ = branch
		_ = out
	}
	o.logf("%s pushed; awaiting merge", id)
	// The merge itself is a separate tick once CI is green, so a red CI never
	// blocks the rest of the pipeline.
	if out, code := o.run("pr", "merge", id); code != 0 {
		if strings.Contains(out, "required checks are not all green") || strings.Contains(out, "no checks reported") {
			return false, nil // CI still running; try again next tick
		}
		return true, o.decide(id, "merge", out)
	}
	o.logf("%s merged", id)
	st.Merged++
	st.LastAction = time.Now().Format(time.RFC3339) + " merge " + id
	return true, nil
}

// dispatch starts the next task the DAG allows, up to the parallelism limit.
func (o *DriveOpts) dispatch(st *DriverStatus, open []Decision) bool {
	out, code := o.run("task", "next")
	if code != 0 {
		return false
	}
	var next string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && strings.HasPrefix(f[0], "T") {
			next = f[0]
			break
		}
	}
	if next == "" {
		return false
	}
	// A task with an open decision is waiting on the Supervisor. Retrying it
	// every tick would re-record the same decision forever and bury the log.
	if len(OpenDecisionsFor(open, next)) > 0 {
		return false
	}
	if _, code := o.run("task", "ready", next); code != 0 {
		// Already ready is not a failure to dispatch.
		if insp, err := OpenStore(o.dagPath(), o.statePath()); err == nil {
			if s, err := insp.Inspect(next); err == nil && s.State.Status == StateReady {
				// fall through to spawn
				_ = s
			}
		}
	}
	args := []string{"worker", "spawn", next}
	if o.WorkerTimeout > 0 {
		args = append(args, "--timeout", o.WorkerTimeout.String())
	}
	if out, code := o.run(args...); code != 0 {
		_ = o.decide(next, "spawn", out)
		return true
	}
	o.logf("%s dispatched", next)
	return true
}

// decide records a refusal as a decision and logs it. It never resolves one: a
// refusal is a judgement call, and judging is the Supervisor's.
func (o *DriveOpts) decide(task, action, reason string) error {
	rec, _ := LoadRegistry(o.RepoRoot, task)
	runID := ""
	if rec != nil {
		runID = rec.RunID
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 2000 {
		reason = reason[:2000] + "…"
	}
	o.logf("DECISION NEEDED: %s %s — %s", task, action, firstLine(reason))
	return RecordDecision(o.RepoRoot, Decision{Task: task, Action: action, Reason: reason, RunID: runID})
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// tasksNeedingAction lists the tasks the driver owes something: one whose Worker
// is running (collect when it exits), one that is collected (review, accept) and
// one that is accepted (commit, PR, merge). Iterating only the running ones
// silently stops the pipeline at the first gate.
func (o *DriveOpts) tasksNeedingAction() ([]string, error) {
	store, err := OpenStore(o.dagPath(), o.statePath())
	if err != nil {
		return nil, fmt.Errorf("reading the task state: %w", err)
	}
	var out []string
	for _, id := range store.dag.IDs() {
		insp, err := store.Inspect(id)
		if err != nil {
			continue
		}
		switch insp.State.Status {
		case StateRunning, StateVerification, StateAccepted:
			out = append(out, id)
		}
	}
	return out, nil
}

// RunRDDev invokes rddev with the DAG/state overrides in place, for callers
// outside this package that must go through the CLI rather than around it. The
// flags are PER-COMMAND: before the subcommand they are read as the subcommand.
func RunRDDev(repoRoot, dagPath, statePath string, args ...string) (string, int) {
	o := &DriveOpts{RepoRoot: repoRoot, DagPath: dagPath, StatePath: statePath}
	return o.run(args...)
}
