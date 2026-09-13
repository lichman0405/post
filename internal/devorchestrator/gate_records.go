package devorchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Gate evidence records (T0012): every gate decision — a collect outcome, a
// G2/G3 run, an accept, a reject, a review verdict, a git/PR action — is
// written to disk in a Supervisor-owned directory (.rddev/runtime/gates/<TASK>)
// outside the Worker's write envelope. Nothing about the four-gate loop
// depends on the Supervisor's memory: `rddev workflow` rebuilds the whole
// picture from these records plus the task state file, so an interrupted
// Supervisor session resumes from disk alone (requirement 7).

// Record types written under the gates dir.
const (
	RecordGateRun = "gate-run" // one executed gate (G2/G3) with jobs/steps/output
	RecordCollect = "collect"  // one collection outcome (G1)
	RecordAccept  = "accept"   // one acceptance decision with gate statuses
	RecordReject  = "reject"   // one rejection with its evidence
	RecordReview  = "review"   // one Review Worker verdict
	RecordGit     = "git"      // one Supervisor git/PR action; record FILES are named git-<action>-<run>.json (git-commit, git-push, git-pr-open, git-pr-merge) while the record_type field stays "git"
)

// GatesDir returns the per-task gate evidence directory.
func GatesDir(repoRoot, taskID string) string {
	return filepath.Join(repoRoot, ".rddev", "runtime", "gates", taskID)
}

// recordMeta is the common header every evidence record carries.
type recordMeta struct {
	RecordType string `json:"record_type"`
	TaskID     string `json:"task_id"`
	RunID      string `json:"run_id"`
	At         string `json:"at"` // RFC 3339 UTC, fixed width — lexicographic == chronological
}

// WriteRecord persists one evidence record under the gates dir as
// <record_type>-<run_id>.json and returns its path.
func WriteRecord(repoRoot, taskID, recordType, runID string, rec any) (string, error) {
	if err := os.MkdirAll(GatesDir(repoRoot, taskID), 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(GatesDir(repoRoot, taskID), recordType+"-"+runID+".json")
	if err := writeFileAtomic(path, marshalIndentBytes(rec)); err != nil {
		return "", fmt.Errorf("writing %s record for %s: %w", recordType, taskID, err)
	}
	return path, nil
}

// GateJobResult is one executed job of a gate run: its status and each step
// with its exit code and output log path.
type GateJobResult struct {
	Job    string           `json:"job"`
	Status string           `json:"status"` // passed | failed
	Steps  []GateStepResult `json:"steps"`
}

// GateStepResult is one executed step: the exact command, its exit code and
// the output log capturing stdout+stderr.
type GateStepResult struct {
	Index      int    `json:"index"`
	Run        string `json:"run"`
	Exit       int    `json:"exit"`
	OutputFile string `json:"output_file"`
	StartedAt  string `json:"started_at"`
	EndedAt    string `json:"ended_at"`
	Skipped    bool   `json:"skipped,omitempty"` // later steps of a failed job
}

// GateRunRecord is the evidence of one executed gate (G2 or G3): every job,
// every step command, every exit code, every output log path.
type GateRunRecord struct {
	recordMeta
	Gate    string          `json:"gate"`
	Status  string          `json:"status"` // passed | failed
	Jobs    []GateJobResult `json:"jobs"`
	EndedAt string          `json:"ended_at"`
	Note    string          `json:"note,omitempty"`
}

// CollectRecord is the evidence of one collection (the G1 gate): the outcome
// and the collect-report path with the full check list.
type CollectRecord struct {
	recordMeta
	Status      string `json:"status"` // ok | rejected | failed
	ReportPath  string `json:"report_path"`
	ResultState string `json:"result_state"` // verification | rejected | worker_failed
	Summary     string `json:"summary"`
}

// AcceptRecord is the evidence of one acceptance decision with the gate
// statuses it rested on (G3 absent from a task reads not_required).
type AcceptRecord struct {
	recordMeta
	Status  string            `json:"status"` // accepted | refused
	Gates   map[string]string `json:"gates"`  // G1..G4 -> passed | failed | not_required
	Reasons []string          `json:"reasons,omitempty"`
}

// RejectRecord is the evidence of one rejection: the reasons and the paths of
// the records that back them (collect report, review verdict, gate run).
type RejectRecord struct {
	recordMeta
	Reasons  []string `json:"reasons"`
	Evidence []string `json:"evidence,omitempty"`
}

// NewRejectRecord builds a rejection evidence record from outside the package
// (rddev task reject): the CLI supplies the content, the engine stamps the
// common record header.
func NewRejectRecord(taskID, runID string, reasons, evidence []string) *RejectRecord {
	return &RejectRecord{
		recordMeta: recordMeta{RecordType: RecordReject, TaskID: taskID, RunID: runID, At: nowRFC3339()},
		Reasons:    reasons,
		Evidence:   evidence,
	}
}

// NewAcceptRecord builds an acceptance evidence record from outside the
// package (rddev task accept): status accepted|refused plus the gate statuses
// the decision rested on.
func NewAcceptRecord(taskID, runID, status string, gates map[string]string, reasons []string) *AcceptRecord {
	return &AcceptRecord{
		recordMeta: recordMeta{RecordType: RecordAccept, TaskID: taskID, RunID: runID, At: nowRFC3339()},
		Status:     status,
		Gates:      gates,
		Reasons:    reasons,
	}
}

// ReviewRecord is the evidence of one Review Worker verdict.
type ReviewRecord struct {
	recordMeta
	Verdict          string `json:"verdict"` // approve | request_changes
	Summary          string `json:"summary"`
	BlockingFindings int    `json:"blocking_findings"`
	MajorFindings    int    `json:"major_findings"`
	VerdictPath      string `json:"verdict_path"`
	// DiffSHA is the code identity the verdict is about (see codeIdentity).
	// The merge gate recomputes it and refuses when it differs: a verdict must
	// not outlive the code state it judged.
	DiffSHA           string `json:"diff_sha,omitempty"`
	ReviewerSessionID string `json:"reviewer_session_id,omitempty"`
}

// GitRecord is the evidence of one Supervisor git/PR action, with the gate
// assertion it passed (never recorded for a refused action — a refusal is a
// GateRunRecord/AcceptRecord, or simply no record at all).
type GitRecord struct {
	recordMeta
	Action      string `json:"action"` // commit | push | pr-open | pr-merge
	Branch      string `json:"branch"`
	CommitSHA   string `json:"commit_sha,omitempty"`
	PRNumber    string `json:"pr_number,omitempty"`
	MergeCommit string `json:"merge_commit,omitempty"`
	GateStatus  string `json:"gate_status"` // the four-gate assertion result at the time
}

// LatestRecord reads the newest record of recordType for the task into out;
// ok=false when no such record exists (e.g. G2 never ran).
func LatestRecord[T any](repoRoot, taskID, recordType string) (T, bool, error) {
	var zero T
	entries, err := os.ReadDir(GatesDir(repoRoot, taskID))
	if os.IsNotExist(err) {
		return zero, false, nil
	}
	if err != nil {
		return zero, false, fmt.Errorf("scanning gate records for %s: %w", taskID, err)
	}
	prefix := recordType + "-"
	var best string
	var bestAt string
	var bestMtime int64
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(GatesDir(repoRoot, taskID), name))
		if err != nil {
			return zero, false, err
		}
		var meta recordMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			continue // a corrupt record is skipped; the valid ones still count
		}
		if meta.RecordType != recordType {
			continue
		}
		// Equal At (same millisecond) is resolved by modification time — the
		// later write wins — and only then by name, so the newest record is
		// deterministic even under same-millisecond writes.
		info, _ := e.Info()
		mtime := int64(0)
		if info != nil {
			mtime = info.ModTime().UnixNano()
		}
		if meta.At > bestAt || (meta.At == bestAt && mtime > bestMtime) || (meta.At == bestAt && mtime == bestMtime && name > best) {
			bestAt = meta.At
			bestMtime = mtime
			best = name
		}
	}
	if best == "" {
		return zero, false, nil
	}
	data, err := os.ReadFile(filepath.Join(GatesDir(repoRoot, taskID), best))
	if err != nil {
		return zero, false, err
	}
	var out T
	if err := json.Unmarshal(data, &out); err != nil {
		return zero, false, fmt.Errorf("parsing gate record %s: %w", best, err)
	}
	return out, true, nil
}

// GateOutputDir returns the per-run output directory for gate step logs.
func GateOutputDir(repoRoot, taskID, runID string) string {
	return filepath.Join(GatesDir(repoRoot, taskID), "output", runID)
}

// nowRFC3339 returns the current UTC time in the fixed-width RFC 3339 form
// used by every record (lexicographic order == chronological order).
// Millisecond precision: records written within the same wall-clock second
// must still order correctly — LatestRecord picks the max At and a
// second-granularity tie was resolved by random run-id name order (the
// flake where a rejected collect in the same second as an ok collect won).
func nowRFC3339() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// SortedRecordNames lists all record files of the given type, oldest first,
// for evidence listing (`rddev gate records`).
func SortedRecordNames(repoRoot, taskID, recordType string) ([]string, error) {
	entries, err := os.ReadDir(GatesDir(repoRoot, taskID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scanning gate records for %s: %w", taskID, err)
	}
	var names []string
	prefix := recordType + "-"
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		// An empty recordType lists every record (the output/ dir is already
		// excluded by IsDir).
		if recordType != "" && !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}
