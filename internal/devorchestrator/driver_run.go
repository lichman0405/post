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
	full := append([]string{"--tasks-json", o.dagPath(), "--state-json", o.statePath()}, args...)
	cmd := exec.Command(o.binary(), full...)
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
	if len(o.runningTasks()) > 0 {
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
func (o *DriveOpts) runningTasks() []string {
	store, err := OpenStore(o.dagPath(), o.statePath())
	if err != nil {
		return nil
	}
	var out []string
	for _, id := range store.dag.IDs() {
		insp, err := store.Inspect(id)
		if err != nil || insp.State.Status != StateRunning {
			continue
		}
		out = append(out, id)
	}
	return out
}

// tick performs at most one action per task and returns whether it did anything.
func (o *DriveOpts) tick(st *DriverStatus) (bool, error) {
	_ = staleDecisions(o.RepoRoot)
	open, err := ReadDecisions(o.RepoRoot)
	if err != nil {
		return false, err
	}
	st.RunningTasks = o.runningTasks()

	acted := false
	for _, id := range append([]string{}, st.RunningTasks...) {
		if len(OpenDecisionsFor(open, id)) > 0 {
			continue // waiting on the Supervisor; retrying would just re-fail
		}
		did, err := o.stepTask(id, st)
		if err != nil {
			return acted, err
		}
		acted = acted || did
	}

	if len(o.runningTasks()) < o.Parallel {
		if did := o.dispatch(st); did {
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
		if err != nil || rec == nil || rec.ExitStatus == nil {
			return false, nil // still working
		}
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
	if _, code := o.run("review", "collect", id); code != 0 {
		return true, o.decide(id, "review-collect", "the reviewer's verdict needs a decision (see the report in .rddev/runtime/gates/"+id+")")
	}
	if out, code := o.run("task", "accept", id); code != 0 {
		return true, o.decide(id, "accept", out)
	}
	o.logf("%s accepted", id)
	st.Merged = st.Merged // unchanged; merging happens in stepAccepted
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
func (o *DriveOpts) dispatch(st *DriverStatus) bool {
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
