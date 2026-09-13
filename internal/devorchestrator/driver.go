package devorchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The persistent Supervisor driver.
//
// A driver that lives inside the Supervisor's conversation is not a driver: the
// mechanical stretch — collect, review, accept, commit, PR, wait for CI, merge,
// dispatch — outlives any single turn, and a process that dies with its launcher
// leaves finished Workers sitting uncollected with nobody to notice. This one is
// owned by the filesystem instead:
//
//   - one instance, enforced by an exclusive lock (a second refuses, naming the
//     holder), because two drivers would double-collect and double-merge;
//   - a heartbeat, so "alive" is a fact readable by anything rather than an
//     inference from a process table;
//   - adoption, so a driver started after (or during) Workers exists picks them
//     up from their state on disk instead of re-spawning them;
//   - decisions recorded on disk, so what needs judgement survives the session
//     that has to make it.
//
// Every ACTION is performed by invoking rddev itself, so the driver can never
// apply weaker rules than the CLI: it decides what to attempt next, and rddev
// refuses on its own terms exactly as it does for a human.

// Driver lock and status paths, under the Supervisor-owned runtime dir.
const (
	driverLockFile   = "driver.lock"
	driverStatusFile = "driver.json"
	decisionsFile    = "decisions.json"
)

// DriverPaths groups the driver's files.
type DriverPaths struct {
	Lock, Status, Decisions string
}

// DriverFilesAt resolves the driver's files for a repository root.
func DriverFilesAt(repoRoot string) DriverPaths {
	dir := filepath.Join(repoRoot, ".rddev", "runtime")
	return DriverPaths{
		Lock:      filepath.Join(dir, driverLockFile),
		Status:    filepath.Join(dir, driverStatusFile),
		Decisions: filepath.Join(dir, decisionsFile),
	}
}

// DriverStatus is what `rddev status` reads. Alive is computed from the
// heartbeat age, not from a live process handle, so a crashed driver is
// reported dead rather than merely unreachable.
type DriverStatus struct {
	PID           int      `json:"pid"`
	StartedAt     string   `json:"started_at"`
	HeartbeatAt   string   `json:"heartbeat_at"`
	State         string   `json:"state"`
	Parallel      int      `json:"parallel"`
	RunningTasks  []string `json:"running_tasks"`
	Merged        int      `json:"merged"`
	LastAction    string   `json:"last_action,omitempty"`
	HeartbeatSeen string   `json:"heartbeat_seen,omitempty"`
}

// StaleAfter is how long a heartbeat may go unheard before the driver is
// considered dead. Generous: a driver blocked on a 90-minute Worker still
// heartbeats every tick, so silence means the process is gone, not busy.
const StaleAfter = 3 * time.Minute

// Alive reports whether the heartbeat is recent enough to believe.
func (s *DriverStatus) Alive(now time.Time) bool {
	if s == nil || s.HeartbeatAt == "" {
		return false
	}
	hb, err := time.Parse(time.RFC3339, s.HeartbeatAt)
	if err != nil {
		return false
	}
	return now.Sub(hb) < StaleAfter
}

// driverFile is the syscall flag that makes an open refuse to follow a symlink.
//
// The driver's files live under .rddev/runtime/, which the Worker guard blocks
// for both reads and writes, so a Worker cannot plant a link there today. The
// guard is not the reason to skip this: the driver runs as the Supervisor, and
// an open that follows a link would truncate or disclose whatever it pointed at
// with the Supervisor's reach. The question "who controls this path" has the
// same answer here as it did for the review diff and the driver's scratch
// files — so the open refuses rather than trusting the answer to stay the same.
const noFollow = syscall.O_NOFOLLOW

// DriverLock is a held exclusive lock on the driver slot.
type DriverLock struct{ f *os.File }

// AcquireDriverLock takes the driver slot or reports who holds it. Non-blocking
// on purpose: a second driver must fail loudly and immediately, not queue.
func AcquireDriverLock(repoRoot string) (*DriverLock, error) {
	paths := DriverFilesAt(repoRoot)
	if err := os.MkdirAll(filepath.Dir(paths.Lock), 0o755); err != nil {
		return nil, fmt.Errorf("creating the driver runtime dir: %w", err)
	}
	// O_NOFOLLOW: a symlink at the lock path is an error, not a path to follow.
	// Truncating through one would destroy the linked file.
	f, err := os.OpenFile(paths.Lock, os.O_CREATE|os.O_RDWR|noFollow, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening the driver lock %s (refusing to follow a symlink): %w", paths.Lock, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Whoever holds it wrote their pid into the lock file.
		holder, _ := os.ReadFile(paths.Lock)
		f.Close()
		return nil, fmt.Errorf("another driver already holds the slot (pid %s) — refusing to start a second one: two drivers would double-collect and double-merge", strings.TrimSpace(string(holder)))
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, err
	}
	if _, err := f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0); err != nil {
		f.Close()
		return nil, err
	}
	return &DriverLock{f: f}, nil
}

// Release drops the driver slot.
func (l *DriverLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}

// WriteDriverStatus commits the heartbeat atomically; readers never see half.
func WriteDriverStatus(repoRoot string, st *DriverStatus) error {
	return writeFileAtomic(DriverFilesAt(repoRoot).Status, marshalIndentBytes(st))
}

// ReadDriverStatus reads the heartbeat; nil when none has ever been written.
func ReadDriverStatus(repoRoot string) (*DriverStatus, error) {
	if err := refuseSymlink(DriverFilesAt(repoRoot).Status); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(DriverFilesAt(repoRoot).Status)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st DriverStatus
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("parsing the driver status file: %w", err)
	}
	return &st, nil
}

// Decision is one thing the driver refused to do on its own.
//
// Refusals are the driver's stop conditions, not its failures: a gate said no,
// and what to do about that (rework, respawn, reject, fix the environment) is a
// judgement. Recording them on disk means the judgement survives the turn, and
// bracketing each with the run it was about means it clears itself the moment
// that attempt is superseded — the driver resumes without being told twice.
type Decision struct {
	Task       string `json:"task"`
	Action     string `json:"action"`
	Reason     string `json:"reason"`
	RunID      string `json:"run_id,omitempty"`
	At         string `json:"at"`
	Supervisor string `json:"supervisor_action,omitempty"`
}

// ReadDecisions returns the open decisions, newest last.
func ReadDecisions(repoRoot string) ([]Decision, error) {
	if err := refuseSymlink(DriverFilesAt(repoRoot).Decisions); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(DriverFilesAt(repoRoot).Decisions)
	if os.IsNotExist(err) {
		return []Decision{}, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Decision
	if len(data) == 0 {
		return []Decision{}, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parsing the decisions file: %w", err)
	}
	return out, nil
}

func writeDecisions(repoRoot string, ds []Decision) error {
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].At < ds[j].At })
	return writeFileAtomic(DriverFilesAt(repoRoot).Decisions, marshalIndentBytes(ds))
}

// RecordDecision opens a decision, replacing any earlier one for the same task
// and action.
func RecordDecision(repoRoot string, d Decision) error {
	ds, err := ReadDecisions(repoRoot)
	if err != nil {
		return err
	}
	if d.At == "" {
		d.At = nowRFC3339()
	}
	out := ds[:0]
	for _, x := range ds {
		if x.Task == d.Task && x.Action == d.Action {
			continue
		}
		out = append(out, x)
	}
	out = append(out, d)
	return writeDecisions(repoRoot, out)
}

// ClearDecisions drops every decision for task, optionally only those recorded
// against a specific run.
func ClearDecisions(repoRoot, task, runID string) error {
	ds, err := ReadDecisions(repoRoot)
	if err != nil {
		return err
	}
	out := make([]Decision, 0, len(ds))
	for _, x := range ds {
		if x.Task == task && (runID == "" || x.RunID == runID) {
			continue
		}
		out = append(out, x)
	}
	return writeDecisions(repoRoot, out)
}

// OpenDecisionsFor returns the decisions recorded for one task.
func OpenDecisionsFor(ds []Decision, task string) []Decision {
	var out []Decision
	for _, d := range ds {
		if d.Task == task {
			out = append(out, d)
		}
	}
	return out
}

// staleDecisions clears decisions whose task has since been redispatched: the
// run the judgement was about no longer exists, so the decision is answered by
// the very act of reworking or respawning.
func staleDecisions(repoRoot string) error {
	ds, err := ReadDecisions(repoRoot)
	if err != nil {
		return err
	}
	if len(ds) == 0 {
		return nil
	}
	var keep []Decision
	for _, d := range ds {
		if d.RunID == "" {
			keep = append(keep, d)
			continue
		}
		rec, err := LoadRegistry(repoRoot, d.Task)
		if err != nil || rec == nil {
			keep = append(keep, d)
			continue
		}
		if rec.RunID != d.RunID {
			continue // superseded: the attempt this was about is gone
		}
		keep = append(keep, d)
	}
	if len(keep) == len(ds) {
		return nil
	}
	return writeDecisions(repoRoot, keep)
}

// refuseSymlink rejects a driver path that is a symlink. Reads through one
// would pull an arbitrary file into the status output; the writes go through a
// temp file and a rename, which replaces the link rather than following it, so
// only the read side needs saying out loud.
func refuseSymlink(path string) error {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to read %s: it is a symlink, and the driver will not follow one with the Supervisor's reach", path)
	}
	return nil
}
