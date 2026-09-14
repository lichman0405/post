package devorchestrator

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/lichman0405/post/internal/config"
)

// DefaultDAGPath and DefaultStatePath are the in-repo locations of the DAG
// and the state truth source, relative to the repo root (rddev's cwd).
const (
	DefaultDAGPath   = "tasks/tasks.json"
	DefaultStatePath = "tasks/task_status.json"
)

// LockFileName is the lock guarding a state file. It is derived from the
// state file's *canonical directory*, so every spelling of the same state file
// (`tasks/task_status.json`, `./tasks/task_status.json`, an absolute path)
// maps to exactly one lock file.
const LockFileName = ".task_status.lock"

// lockPathFor returns the single lock file guarding statePath.
//
// Deriving the lock from the path string is what broke mutual exclusion
// before: the default spelling used one lock file and an explicitly re-spelled
// path used another, so two processes writing the SAME file took DIFFERENT
// locks and silently lost an update. Canonicalising first makes the lock a
// property of the file, not of how a caller happened to type it.
func lockPathFor(statePath string) (string, error) {
	abs, err := filepath.Abs(statePath)
	if err != nil {
		return "", fmt.Errorf("resolving state path %s: %w", statePath, err)
	}
	return filepath.Join(filepath.Dir(filepath.Clean(abs)), LockFileName), nil
}

// taskStateTime renders a time the way tasks/task_status.json records time:
// UTC, ISO 8601, to the second. That is the shape scripts/validate_task_state.py
// enforces in CI — its ISO_TS_RE admits no fractional seconds, and the check
// applies it to started_at, completed_at and merged_at (NOT to history entries:
// the validator iterates those four named keys only, so a history `at` it would
// never read. History is rendered through here because the file should hold one
// shape, not because CI would catch it) — so this is the one place the rule is
// written down. It is NOT the shape of the run-record times (nowRFC3339,
// milliseconds): those order records, this one describes a task's lifecycle to
// the Supervisor and to CI.
func taskStateTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// taskStateStamp is taskStateTime for a timestamp some other layer recorded.
// The run start spawn passes to StartWorkerFrom carries nanoseconds — a start to
// the second could not order a verdict written inside the same second (#105) —
// and this file must not carry them.
//
// A value that does not parse is passed through unchanged rather than replaced
// with a clock reading: this runs inside a state transition, and inventing a
// time there would hide the wrong value. What the pass-through does NOT
// guarantee is that CI names it: the validator's ISO_TS_RE accepts a date with
// no time at all — `2026-09-13` fails time.Parse here and passes that regex — so
// a date-only value would go through silently. Passing it through is still the
// right half of the trade (a wrong value that survives is reported as itself,
// where a clock reading would replace it with a plausible lie), but the value
// being wrong is only visible if it is wrong in a way the regex rejects. The
// only production caller passes runStartedAt(), which always parses.
func taskStateStamp(recorded string) string {
	t, err := time.Parse(time.RFC3339, recorded)
	if err != nil {
		return recorded
	}
	return taskStateTime(t)
}

// NewRunID returns a random run_id for a state change (crypto/rand, 8 bytes
// hex). Callers may instead pass a Supervisor-supplied run id via --run-id.
func NewRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand is not expected to fail; fall back to a timestamp so a
		// run_id is always carried (the write must never silently lack one).
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(b[:])
}

// Store reads the task DAG and applies guarded transitions to the state file.
// All mutations serialize on an exclusive lock and commit with an atomic
// rename; readers see either the complete old or the complete new file.
type Store struct {
	dag       *DAG
	statePath string
	lockPath  string
}

// OpenStore loads the DAG and resolves the state/lock paths.
func OpenStore(dagPath, statePath string) (*Store, error) {
	d, err := LoadDAG(dagPath)
	if err != nil {
		return nil, err
	}
	lock, err := lockPathFor(statePath)
	if err != nil {
		return nil, err
	}
	return &Store{dag: d, statePath: statePath, lockPath: lock}, nil
}

// DAG returns the loaded task DAG.
func (s *Store) DAG() *DAG { return s.dag }

// StatePath returns the targeted state file path.
func (s *Store) StatePath() string { return s.statePath }

// NextTask is one entry of `rddev task next` / `task ready` output.
type NextTask struct {
	ID           string   `json:"id"`
	Status       State    `json:"status"`
	Phase        string   `json:"phase"`
	Title        string   `json:"title"`
	Dependencies []string `json:"dependencies"`
	DepsMet      bool     `json:"deps_met"`
	UnmetDeps    []string `json:"unmet_deps"`
}

// depsMet returns the dependency statuses of id per the given state map:
// every dependency must be merged (docs/30 §3: dependencies are verified
// merged before a task starts).
func (s *Store) depsMet(states map[string]State, id string) (met bool, unmet []string) {
	t := s.dag.Get(id)
	unmet = make([]string, 0, len(t.Dependencies))
	for _, dep := range t.Dependencies {
		if states[dep] != StateMerged {
			unmet = append(unmet, dep)
		}
	}
	return len(unmet) == 0, unmet
}

// checkForStateDrift rejects a state file that EXISTS but declares no tasks
// while the DAG has tasks.
//
// Such a file is truncated, renamed or hand-edited — it is not "every task is
// todo". Treating it that way silently resets every completed task to todo and
// lets finished work be re-dispatched, with no warning: a fail-open on state
// drift. A *missing* file is different and still means a fresh repository.
func (s *Store) checkForStateDrift(entryCount int) error {
	if entryCount == 0 && len(s.dag.Tasks) > 0 {
		return fmt.Errorf("task status file %s exists but declares no tasks while "+
			"the DAG has %d: refusing to read this as 'all tasks are todo' — the "+
			"file is truncated, renamed or drifted. Restore it, or delete it "+
			"deliberately to start from a fresh state", s.statePath, len(s.dag.Tasks))
	}
	return nil
}

// readStates loads the state file (absent entries default to todo).
func (s *Store) readStates() (map[string]State, error) {
	data, err := os.ReadFile(s.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]State{}, nil
		}
		return nil, fmt.Errorf("reading task status file %s: %w", s.statePath, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing task status file %s: %w", s.statePath, err)
	}
	entries, err := decodeTaskEntries(raw["tasks"])
	if err != nil {
		return nil, fmt.Errorf("parsing task status file %s: %w", s.statePath, err)
	}
	if err := s.checkForStateDrift(len(entries)); err != nil {
		return nil, err
	}
	states := make(map[string]State, len(entries)+len(s.dag.Tasks))
	for id, entry := range entries {
		var ts TaskState
		if err := json.Unmarshal(entry, &ts); err != nil {
			return nil, fmt.Errorf("task status file %s: task %s entry is not valid JSON: %w", s.statePath, id, err)
		}
		if !ValidState(string(ts.Status)) {
			return nil, fmt.Errorf("task status file %s: task %s has unknown status %q", s.statePath, id, ts.Status)
		}
		states[id] = ts.Status
	}
	for _, t := range s.dag.Tasks { // absent == todo
		if _, ok := states[t.ID]; !ok {
			states[t.ID] = StateTodo
		}
	}
	return states, nil
}

// Next returns tasks whose dependencies are all satisfied (every dependency
// merged) and whose status is todo or ready — the dispatch pool, sorted by
// id. This is the scheduling answer `rddev task next` provides the
// Supervisor; tasks with an unsatisfied dependency are excluded entirely.
func (s *Store) Next() ([]NextTask, error) {
	states, err := s.readStates()
	if err != nil {
		return nil, err
	}
	out := make([]NextTask, 0) // empty pool serializes as [], never null
	for _, id := range s.dag.IDs() {
		st := states[id]
		if st != StateTodo && st != StateReady {
			continue
		}
		met, unmet := s.depsMet(states, id)
		if !met {
			continue
		}
		t := s.dag.Get(id)
		out = append(out, NextTask{
			ID: id, Status: st, Phase: t.Phase, Title: t.Title,
			Dependencies: t.Dependencies, DepsMet: met, UnmetDeps: unmet,
		})
	}
	return out, nil
}

// ReadyList returns tasks currently in state ready, sorted by id.
func (s *Store) ReadyList() ([]NextTask, error) {
	states, err := s.readStates()
	if err != nil {
		return nil, err
	}
	out := make([]NextTask, 0) // empty pool serializes as [], never null
	for _, id := range s.dag.IDs() {
		if states[id] != StateReady {
			continue
		}
		met, unmet := s.depsMet(states, id)
		t := s.dag.Get(id)
		out = append(out, NextTask{
			ID: id, Status: states[id], Phase: t.Phase, Title: t.Title,
			Dependencies: t.Dependencies, DepsMet: met, UnmetDeps: unmet,
		})
	}
	return out, nil
}

// InspectResult is the payload of `rddev task inspect`: the DAG entry plus
// the full recorded state (history included).
type InspectResult struct {
	Task  TaskSpec  `json:"task"`
	State TaskState `json:"state"`
}

// Inspect returns the DAG entry and the full recorded state for id. A task
// with no state entry yet reports the implicit StateTodo. Reads are lock-free
// but always see a complete file (all writers commit with an atomic rename).
func (s *Store) Inspect(id string) (*InspectResult, error) {
	t := s.dag.Get(id)
	if t == nil {
		return nil, fmt.Errorf("unknown task %s in task DAG", id)
	}
	ts := TaskState{Status: StateTodo}
	data, err := os.ReadFile(s.statePath)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading task status file %s: %w", s.statePath, err)
	}
	if len(data) > 0 {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(data, &top); err != nil {
			return nil, fmt.Errorf("parsing task status file %s: %w", s.statePath, err)
		}
		tasks, err := decodeTaskEntries(top["tasks"])
		if err != nil {
			return nil, fmt.Errorf("parsing task status file %s: %w", s.statePath, err)
		}
		if err := s.checkForStateDrift(len(tasks)); err != nil {
			return nil, err
		}
		if raw, ok := tasks[id]; ok {
			if err := json.Unmarshal(raw, &ts); err != nil {
				return nil, fmt.Errorf("task status file %s: task %s entry is not valid JSON: %w", s.statePath, id, err)
			}
			if !ValidState(string(ts.Status)) {
				return nil, fmt.Errorf("task status file %s: task %s has unknown status %q", s.statePath, id, ts.Status)
			}
		}
	}
	return &InspectResult{Task: *t, State: ts}, nil
}

// TransitionResult reports a committed state change.
type TransitionResult struct {
	TaskID string `json:"task_id"`
	From   State  `json:"from"`
	To     State  `json:"to"`
	RunID  string `json:"run_id"`
	At     string `json:"at"`
}

// persistedReason is how a reason reaches a committed file. Every writer of
// ts.History and ts.RejectionReason goes through here, so the redaction is a
// property of the store rather than of one method: Transition carries the
// reasoning below, and StartWorkerFrom — which appends its own history entry
// without one — calls the same function, because "the reason is already
// redacted" is a claim about the file, not about the caller.
func persistedReason(reason string) string {
	return config.RedactTextForOutput(reason)
}

// Transition applies id -> to under the exclusive lock: it validates the
// transition against the current state (re-read after acquiring the lock),
// appends a history entry carrying runID, and commits with an atomic rename.
// On an illegal transition the state file is left unchanged and an
// *IllegalTransitionError is returned. Entering `ready` additionally requires
// every dependency to be merged (docs/30 §3) — a *DependencyError otherwise.
func (s *Store) Transition(id string, to State, runID, reason string) (*TransitionResult, error) {
	if s.dag.Get(id) == nil {
		return nil, fmt.Errorf("unknown task %s in task DAG", id)
	}
	if runID == "" {
		return nil, fmt.Errorf("internal error: transition without run_id")
	}
	// A reason is a persisted artifact and task_status.json is committed, so
	// this is an output path like any other — but the text is free prose
	// composed elsewhere (a check's detail, a Worker's own test command), so it
	// goes through the text redactor rather than the value one. RedactForOutput
	// would replace a multi-kilobyte reason with "***" the moment it contained
	// one 40-character sha, which is how a leak turns into a deleted audit
	// trail; see RedactTextForOutput.
	//
	// Found by a scan of the committed tree: a rejection reason carried the
	// test command's inline environment verbatim, including a
	// `postgres://user:pw@host` DSN. The value was one of the dev fixtures in
	// internal/config/secretscan.go — already in docker-compose.yml and CI by
	// design, so nothing was disclosed — but the mechanism is the T0011 class,
	// and a command carrying a real token would have been committed
	// permanently to a file nobody re-reads.
	//
	// What this does not cover, stated so it is not read as full coverage:
	// RESULT.json is authored by the Worker in its own worktree and reaches the
	// repository through the task PR, not through this store. Redacting it
	// needs a collect-time pass over a file this package does not write.
	reason = persistedReason(reason)
	var result *TransitionResult
	err := s.mutate(id, func(ts *TaskState, from State, states map[string]State) error {
		if err := checkTransition(id, from, to); err != nil {
			return err
		}
		// Both transitions that mean "this task is about to be worked on" are
		// checked, not only ready. The rule docs/30 §3 states is about starting
		// ("dependencies are verified merged before a task starts"), and a task
		// does not start when it becomes ready — it starts when it is spawned
		// or reworked.
		//
		// The difference is not academic. The DAG is edited while tasks are in
		// flight, and an edge added to a task already past ready is never
		// re-examined by a check that only runs on the way in: T0603 was marked
		// ready against ['T0105'], #102 later added T0208 to its closure, and it
		// was dispatched anyway, ran a full Worker session, and reached
		// verification carrying a gate (rsg-real-services) that cannot go green
		// until the dependency it names is on main. Nothing could catch it
		// afterwards — the Worker was already running — so the moment worth
		// guarding is this one (L1-20260914-19).
		//
		// What this does not do, stated so it is not read as full coverage: it
		// cannot stop an edge from being added to a running task, and it cannot
		// unwind one that has already run. It only refuses to let it start
		// again.
		if to == StateReady || to == StateRunning {
			unmet := map[string]State{}
			for _, dep := range s.dag.Get(id).Dependencies {
				if states[dep] != StateMerged {
					unmet[dep] = states[dep]
				}
			}
			if len(unmet) > 0 {
				return &DependencyError{ID: id, Unmet: unmet}
			}
		}
		at := taskStateTime(time.Now())
		ts.Status = to
		ts.History = append(ts.History, StateChange{From: from, To: to, At: at, RunID: runID, Reason: reason})
		if to == StateAccepted {
			ts.AcceptedBySupervisorAt = strptr(at)
		}
		if to == StateMerged {
			ts.MergedAt = strptr(at)
		}
		if to == StateRejected && reason != "" {
			ts.RejectionReason = reason
		}
		result = &TransitionResult{TaskID: id, From: from, To: to, RunID: runID, At: at}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// mutate locks the state file, applies fn to the task entry, and commits
// atomically. fn receives the task's current state (todo if absent) and the
// full locked status map (absent tasks default to todo).
func (s *Store) mutate(id string, fn func(ts *TaskState, from State, states map[string]State) error) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	top, tasks, err := s.readRawLocked()
	if err != nil {
		return err
	}
	ts, from := TaskState{Status: StateTodo}, StateTodo
	if raw, ok := tasks[id]; ok {
		if err := json.Unmarshal(raw, &ts); err != nil {
			return fmt.Errorf("task status file %s: task %s entry is not valid JSON: %w", s.statePath, id, err)
		}
		from = ts.Status
	}
	states := make(map[string]State, len(s.dag.Tasks))
	for tid, raw := range tasks {
		var entry TaskState
		if err := json.Unmarshal(raw, &entry); err != nil {
			return fmt.Errorf("task status file %s: task %s entry is not valid JSON: %w", s.statePath, tid, err)
		}
		states[tid] = entry.Status
	}
	for _, t := range s.dag.Tasks {
		if _, ok := states[t.ID]; !ok {
			states[t.ID] = StateTodo
		}
	}
	if err := fn(&ts, from, states); err != nil {
		return err
	}
	entry, err := marshalNoEscape(&ts)
	if err != nil {
		return fmt.Errorf("encoding task %s state: %w", id, err)
	}
	tasks[id] = entry
	tasksRaw, err := marshalNoEscape(tasks)
	if err != nil {
		return fmt.Errorf("encoding task status file: %w", err)
	}
	top["tasks"] = tasksRaw
	if _, ok := top["version"]; !ok {
		top["version"] = json.RawMessage("2")
	}
	out, err := marshalIndent(top)
	if err != nil {
		return fmt.Errorf("encoding task status file: %w", err)
	}
	return writeFileAtomic(s.statePath, out)
}

// lock acquires the exclusive state-file lock (blocking; the OS releases it
// when the process dies, so a crashed writer cannot wedge the file forever).
func (s *Store) lock() (func(), error) {
	if dir := filepath.Dir(s.lockPath); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("creating lock directory %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening state lock %s: %w", s.lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking state file via %s: %w", s.lockPath, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // best effort
		f.Close()
	}, nil
}

// readRawLocked reads the raw status file (caller must hold the lock).
func (s *Store) readRawLocked() (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	top := map[string]json.RawMessage{}
	data, err := os.ReadFile(s.statePath)
	// A missing file is a legitimate fresh start and yields an empty state.
	// A file that exists but carries no tasks is drift, and checkForStateDrift
	// below refuses it — those two cases must not be conflated.
	if os.IsNotExist(err) {
		return top, map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading task status file %s: %w", s.statePath, err)
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &top); err != nil {
			return nil, nil, fmt.Errorf("parsing task status file %s: %w", s.statePath, err)
		}
	}
	tasks, err := decodeTaskEntries(top["tasks"])
	if err != nil {
		return nil, nil, fmt.Errorf("parsing task status file %s: %w", s.statePath, err)
	}
	if err := s.checkForStateDrift(len(tasks)); err != nil {
		return nil, nil, err
	}
	return top, tasks, nil
}

// decodeTaskEntries decodes a raw `tasks` object into per-task raw entries.
func decodeTaskEntries(raw json.RawMessage) (map[string]json.RawMessage, error) {
	tasks := map[string]json.RawMessage{}
	if len(raw) == 0 {
		return tasks, nil
	}
	if err := json.Unmarshal(raw, &tasks); err != nil {
		return nil, fmt.Errorf("invalid tasks object: %w", err)
	}
	return tasks, nil
}

// marshalNoEscape marshals without HTML escaping. Plain json.Marshal rewrites
// every ">" and "<" in a state note as \u003e / \u003c, so a note written as
// "ready -> running" is stored unreadable and every later write keeps it that
// way. task_status.json is the Supervisor's human-read truth source; its notes
// must stay legible.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// marshalIndent pretty-prints JSON preserving field order and raw Unicode
// (no HTML escaping, matching the existing hand-maintained state file).
func marshalIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeFileAtomic writes data to path via a temp file + fsync + rename so a
// concurrent reader never observes a partial write.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating state directory %s: %w", dir, err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".task-status-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp state file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp state file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming temp state file into place: %w", err)
	}
	return nil
}

func strptr(s string) *string { return &s }
