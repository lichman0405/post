package devorchestrator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Authoritative gate inputs (T0012 security fix). Before this change, collect
// judged the two central invariants — HEAD == baseline_sha and the diff ⊆
// allowed_scope — against values read from .rddev/workers/<TASK>/, a
// directory the Worker itself can write (it must, for RESULT.json). A Worker
// could widen its own allowed_scope or rewrite its recorded baseline and the
// gate would validate against inputs the gated party rewrote.
//
// Spawn now records the authoritative gate inputs under
// .rddev/runtime/tasks/<TASK>/ — a path the Worker's guard refuses for both
// reads and shell writes (the guard confines the Worker to its worktree, its
// result dir and /tmp). Collect judges by those values and verifies the
// Worker-writable copies (registry.json, task-package.json, the generated
// guard layer) against them: a mismatch is a hard reject, not a judgement on
// the tampered values. The reaper also writes its exit.status into this
// directory, so the exit code collect trusts is not Worker-writable either.

// RuntimeTasksRoot is the parent of every task's authoritative input dir.
func RuntimeTasksRoot(repoRoot string) string {
	return filepath.Join(repoRoot, ".rddev", "runtime", "tasks")
}

// RuntimeTasksDir returns the Supervisor-owned authoritative input directory
// for one task (outside the Worker's write envelope).
func RuntimeTasksDir(repoRoot, taskID string) string {
	return filepath.Join(RuntimeTasksRoot(repoRoot), taskID)
}

// GateInputs is the authoritative spawn-time record collect judges by. Every
// field is captured by spawn before the Worker runs; none of them are read
// back from the Worker-writable copies.
type GateInputs struct {
	TaskID             string   `json:"task_id"`
	RunID              string   `json:"run_id"`
	SessionID          string   `json:"session_id"`
	BaselineSHA        string   `json:"baseline_sha"`
	Branch             string   `json:"branch"`
	Worktree           string   `json:"worktree"`
	ResultDir          string   `json:"result_dir"`
	LogPath            string   `json:"log_path"`
	RefsBefore         []string `json:"refs_before"`
	PID                int      `json:"pid"`
	StartTime          uint64   `json:"start_time"`
	SessionLeaderPID   int      `json:"session_leader_pid"`
	ListenersBefore    []string `json:"listeners_before"`
	StartedAt          string   `json:"started_at"`
	AllowedScope       []string `json:"allowed_scope"`
	RequiredTests      []string `json:"required_tests"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	// ReviewDiffSHA is the sha256 of the reviewed worktree's content at
	// review-spawn time; review collect rejects a verdict produced against
	// different code (or a review Worker that wrote into the task worktree).
	ReviewDiffSHA string `json:"review_diff_sha,omitempty"`
}

// gateInputsPath returns the authoritative record path for one task.
func gateInputsPath(repoRoot, taskID string) string {
	return filepath.Join(RuntimeTasksDir(repoRoot, taskID), "gate-inputs.json")
}

// authoritativeExitStatusPath is where the reaper writes its second copy of
// exit.status (arg 5 of run-worker.sh).
func authoritativeExitStatusPath(repoRoot, taskID string) string {
	return filepath.Join(RuntimeTasksDir(repoRoot, taskID), "exit.status")
}

// authoritativeGuardFiles lists the generated guard-layer files stored
// verbatim at spawn, keyed by their path inside the task runtime dir.
var authoritativeGuardFiles = []string{
	"guard/worker-guard.sh",
	"worker-settings.json",
	"run-worker.sh",
}

// WriteGateInputs records the authoritative gate inputs and byte-copies of
// the package and guard layer for one spawn. It must run before the reaper
// starts, so a collect can never observe a Worker without an authoritative
// record (a missing record means a pre-T0012 spawn, handled as a degraded
// warning, never as a silent judgement on Worker-writable copies).
// taskDir is the per-task runtime dir (.rddev/workers/<TASK>).
func WriteGateInputs(repoRoot, taskID string, inputs *GateInputs, taskDir string) error {
	dir := RuntimeTasksDir(repoRoot, taskID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data := marshalIndentBytes(inputs)
	if err := writeFileAtomic(gateInputsPath(repoRoot, taskID), data); err != nil {
		return fmt.Errorf("writing authoritative gate inputs: %w", err)
	}
	// Byte copies of everything the Worker could tamper with. task-package.json
	// carries allowed_scope; the guard layer is the Worker's own confinement.
	copies := map[string]string{"task-package.json": filepath.Join(taskDir, "task-package.json")}
	for _, f := range authoritativeGuardFiles {
		copies[f] = filepath.Join(taskDir, f)
	}
	for rel, src := range copies {
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("reading %s for the authoritative copy: %w", src, err)
		}
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := writeFileAtomic(dst, data); err != nil {
			return fmt.Errorf("writing authoritative copy %s: %w", dst, err)
		}
	}
	return nil
}

// LoadGateInputs reads the authoritative record; nil when absent (a
// pre-T0012 spawn).
func LoadGateInputs(repoRoot, taskID string) (*GateInputs, error) {
	data, err := os.ReadFile(gateInputsPath(repoRoot, taskID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading authoritative gate inputs for %s: %w", taskID, err)
	}
	var g GateInputs
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("parsing authoritative gate inputs for %s: %w", taskID, err)
	}
	if g.TaskID != taskID {
		return nil, fmt.Errorf("authoritative gate inputs for %s carry task_id %q — the record belongs to a different task", taskID, g.TaskID)
	}
	return &g, nil
}

// FinalizeGateInputs records the process-identity fields in the authoritative
// record once the Worker exists (the early WriteGateInputs runs before the
// process is started, so PID/start time/session leader/listener baseline are
// unknown then). It refuses when the record on disk does not match the
// expected run/session — a finalize against a rewritten record must never
// bless the rewrite.
func FinalizeGateInputs(repoRoot, taskID, runID, sessionID string, pid int, startTime uint64, sessionLeaderPID int, startedAt string, listenersBefore []string) error {
	gate, err := LoadGateInputs(repoRoot, taskID)
	if err != nil {
		return err
	}
	if gate == nil {
		return fmt.Errorf("finalizing %s: no authoritative gate-inputs record exists (spawn must write it first)", taskID)
	}
	if gate.RunID != runID || gate.SessionID != sessionID {
		return fmt.Errorf("finalizing %s: the authoritative record carries run_id %q session %q, not the expected %q/%q — refusing to bless a rewritten record", taskID, gate.RunID, gate.SessionID, runID, sessionID)
	}
	gate.PID = pid
	gate.StartTime = startTime
	gate.SessionLeaderPID = sessionLeaderPID
	gate.StartedAt = startedAt
	gate.ListenersBefore = append([]string(nil), listenersBefore...)
	if err := writeFileAtomic(gateInputsPath(repoRoot, taskID), marshalIndentBytes(gate)); err != nil {
		return fmt.Errorf("finalizing authoritative gate inputs: %w", err)
	}
	return nil
}

// GateInputTamper is one detected mismatch between the Worker-writable copy
// and the authoritative record.
type GateInputTamper struct {
	What   string `json:"what"` // registry | task-package.json | guard layer | exit.status
	Detail string `json:"detail"`
}

// VerifyGateInputs compares the Worker-writable copies against the
// authoritative record and returns every mismatch. It checks the registry's
// gate fields (the ones spawn recorded before the Worker ran), the package
// bytes, the guard-layer bytes, and the reaper's exit.status copy. An empty
// result means the Worker did not touch its own gate inputs.
func VerifyGateInputs(repoRoot string, rec *WorkerRecord, gate *GateInputs) []GateInputTamper {
	var out []GateInputTamper
	if rec.TaskID != gate.TaskID {
		out = append(out, GateInputTamper{What: "registry", Detail: fmt.Sprintf("task_id %q != authoritative %q", rec.TaskID, gate.TaskID)})
	}
	// The fields spawn recorded before the Worker existed. ExitStatus and its
	// reconcilers legitimately change after the run (reaper/discovery), so
	// they are not part of the comparison.
	compare := []struct {
		name string
		got  any
		want any
	}{
		{"run_id", rec.RunID, gate.RunID},
		{"baseline_sha", rec.BaselineSHA, gate.BaselineSHA},
		{"branch", rec.Branch, gate.Branch},
		{"worktree", rec.Worktree, gate.Worktree},
		{"pid", rec.PID, gate.PID},
		{"start_time", rec.StartTime, gate.StartTime},
		{"session_leader_pid", rec.SessionLeaderPID, gate.SessionLeaderPID},
		{"started_at", rec.StartedAt, gate.StartedAt},
	}
	for _, c := range compare {
		if c.got != c.want {
			out = append(out, GateInputTamper{What: "registry", Detail: fmt.Sprintf("field %s is %v, authoritative %v", c.name, c.got, c.want)})
		}
	}
	if !equalStrings(rec.RefsBefore, gate.RefsBefore) {
		out = append(out, GateInputTamper{What: "registry", Detail: "refs_before differs from the spawn snapshot — a new ref the Worker created could be hidden"})
	}
	if !equalStrings(rec.ListenersBefore, gate.ListenersBefore) {
		out = append(out, GateInputTamper{What: "registry", Detail: "listeners_before differs from the spawn baseline — residue detection could be blinded"})
	}
	if rec.ResultDir != gate.ResultDir {
		out = append(out, GateInputTamper{What: "registry", Detail: fmt.Sprintf("result_dir %q != authoritative %q", rec.ResultDir, gate.ResultDir)})
	}
	if rec.LogPath != gate.LogPath {
		out = append(out, GateInputTamper{What: "registry", Detail: fmt.Sprintf("log_path %q != authoritative %q", rec.LogPath, gate.LogPath)})
	}
	// task-package.json (allowed_scope) and the guard layer. byteCopyDiff's
	// ok reports "an authoritative copy exists to compare against"; only a
	// non-empty detail is an actual tamper (equal copies yield "").
	if diff, ok := byteCopyDiff(rec.ResultDir, gate.TaskID, repoRoot, "task-package.json"); ok && diff != "" {
		out = append(out, GateInputTamper{What: "task-package.json", Detail: diff})
	}
	for _, f := range authoritativeGuardFiles {
		if diff, ok := byteCopyDiff(rec.ResultDir, gate.TaskID, repoRoot, f); ok && diff != "" {
			what := "guard layer"
			if f == "run-worker.sh" {
				what = "run-worker.sh"
			}
			out = append(out, GateInputTamper{What: what, Detail: diff})
		}
	}
	// exit.status: the reaper writes both copies; they must agree.
	authStatus, aerr := os.ReadFile(authoritativeExitStatusPath(repoRoot, gate.TaskID))
	workStatus, werr := os.ReadFile(filepath.Join(rec.ResultDir, "exit.status"))
	if aerr == nil && werr == nil && !bytes.Equal(bytes.TrimSpace(authStatus), bytes.TrimSpace(workStatus)) {
		out = append(out, GateInputTamper{What: "exit.status", Detail: fmt.Sprintf("result-dir copy %q differs from the authoritative reaper copy %q", strings.TrimSpace(string(workStatus)), strings.TrimSpace(string(authStatus)))})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].What < out[j].What })
	return out
}

// byteCopyDiff compares the Worker-writable copy of rel (under the task
// result dir) with the authoritative copy. ok=false when the authoritative
// copy is absent (pre-T0012 spawn), in which case the value is "".
func byteCopyDiff(resultDir, taskID, repoRoot, rel string) (detail string, ok bool) {
	auth, err := os.ReadFile(filepath.Join(RuntimeTasksDir(repoRoot, taskID), rel))
	if err != nil {
		return "", false // no authoritative copy to compare against
	}
	work, err := os.ReadFile(filepath.Join(resultDir, rel))
	if err != nil {
		return fmt.Sprintf("%s is missing from the task dir but was recorded at spawn", rel), true
	}
	if !bytes.Equal(auth, work) {
		return fmt.Sprintf("%s differs from the copy spawn recorded — the Worker edited its own %s", rel, rel), true
	}
	return "", true
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// readAuthoritativeExitStatus returns the exit code the reaper recorded in
// the Supervisor-owned directory; ok=false when absent (pre-T0012 spawn or
// the reaper has not written yet).
func readAuthoritativeExitStatus(repoRoot, taskID string) (int, bool, error) {
	data, err := os.ReadFile(authoritativeExitStatusPath(repoRoot, taskID))
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("reading authoritative exit status for %s: %w", taskID, err)
	}
	code := 0
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &code); err != nil {
		return 0, false, fmt.Errorf("authoritative exit status for %s is not an integer: %q", taskID, strings.TrimSpace(string(data)))
	}
	return code, true, nil
}

// writeAuthoritativeExitStatus writes the Supervisor-side exit.status copy
// (used by `worker stop`, whose termination the reaper does not witness).
func writeAuthoritativeExitStatus(repoRoot, taskID string, code int) error {
	if err := os.MkdirAll(RuntimeTasksDir(repoRoot, taskID), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(authoritativeExitStatusPath(repoRoot, taskID), []byte(fmt.Sprintf("%d\n", code)))
}

// writeExitStatusCopies writes *both* exit.status copies — the task-dir one
// and the Supervisor-side authoritative one. The reaper writes both when it
// witnesses the exit; whoever has to reconstruct a lost run (discovery
// reconciling a Worker whose reaper never got to write) must write both too,
// or VerifyGateInputs would later compare a present copy against an absent
// one and read the reconstruction itself as tampering.
func writeExitStatusCopies(repoRoot, taskID string, code int) error {
	if err := writeAuthoritativeExitStatus(repoRoot, taskID, code); err != nil {
		return err
	}
	if err := os.MkdirAll(WorkerTaskDir(repoRoot, taskID), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(exitStatusPath(repoRoot, taskID), []byte(fmt.Sprintf("%d\n", code)))
}
