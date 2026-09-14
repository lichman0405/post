package devorchestrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRejectRecordAt writes one RejectRecord with an explicit timestamp, so a
// test can control which record LatestRecord considers newest.
func writeRejectRecordAt(t *testing.T, repoRoot, taskID, runID, at, reason string) {
	t.Helper()
	rec := NewRejectRecord(taskID, runID, []string{reason}, nil)
	rec.At = at
	if _, err := WriteRecord(repoRoot, taskID, RecordReject, runID, rec); err != nil {
		t.Fatal(err)
	}
}

func writeReasonFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "reason.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A rework without --reason-file behaves exactly as before: the newest
// RejectRecord is the instruction, and the state file is the fallback.
func TestReworkReasonWithoutAFileIsTheRecordedOne(t *testing.T) {
	repoRoot := t.TempDir()
	writeRejectRecordAt(t, repoRoot, "T0001", "run-old", "2026-09-14T01:00:00Z", "the older reason")
	writeRejectRecordAt(t, repoRoot, "T0001", "run-new", "2026-09-14T02:00:00Z", "the newest reason")

	got, err := reworkReasonFrom(&SpawnOpts{RepoRoot: repoRoot, TaskID: "T0001"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "the newest reason") || strings.Contains(got, "the older reason") {
		t.Fatalf("reworkReasonFrom did not pick the newest record:\n%s", got)
	}
}

// The point of #126: the Supervisor's file becomes the reason the Worker reads,
// even though a newer-by-record rule would have picked the machine's text — and
// the text is recorded, so the trail stays append-only and the next rework reads
// what this Worker read.
func TestReworkReasonFileBecomesTheReasonAndIsRecorded(t *testing.T) {
	repoRoot := t.TempDir()
	writeRejectRecordAt(t, repoRoot, "T0001", "run-collect", "2026-09-14T01:00:00Z",
		"result-consistency: a completed claim with failing tests is a contradiction")
	before, err := os.ReadDir(GatesDir(repoRoot, "T0001"))
	if err != nil {
		t.Fatal(err)
	}

	path := writeReasonFile(t, "\n\n用 make test-integration 跑，别用 go test ./tests/integration\n\n")
	opts := &SpawnOpts{RepoRoot: repoRoot, TaskID: "T0001", ReasonFile: path}
	got, err := reworkReasonFrom(opts)
	if err != nil {
		t.Fatal(err)
	}
	if got != "用 make test-integration 跑，别用 go test ./tests/integration" {
		t.Fatalf("the file's content is not the reason (untrimmed?):\n%q", got)
	}
	if strings.Contains(got, "result-consistency") {
		t.Errorf("the file did not replace the recorded reason:\n%s", got)
	}
	if opts.RunID == "" {
		t.Error("no run id was assigned, so the record and the run it dispatched would disagree")
	}

	// The record is written, it carries the same run id, and it is the newest —
	// which is what makes the NEXT rework read the same sentence.
	rec, ok, err := LatestRecord[RejectRecord](repoRoot, "T0001", RecordReject)
	if err != nil || !ok {
		t.Fatalf("no RejectRecord after the rework reason was supplied (ok=%v err=%v)", ok, err)
	}
	if rec.RunID != opts.RunID {
		t.Errorf("record run id %q != the run it dispatched %q", rec.RunID, opts.RunID)
	}
	if len(rec.Reasons) != 1 || rec.Reasons[0] != got {
		t.Errorf("recorded reasons = %q", rec.Reasons)
	}

	// Append-only: the machine's rejection is still on disk. A trail that lost
	// the reason the Worker was actually rejected for would be worse than the
	// problem this flag solves.
	after, err := os.ReadDir(GatesDir(repoRoot, "T0001"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 {
		t.Errorf("gate records went from %d to %d — the old record was not preserved", len(before), len(after))
	}
	if _, err := os.Stat(filepath.Join(GatesDir(repoRoot, "T0001"), "reject-run-collect.json")); err != nil {
		t.Errorf("the collect rejection record is gone: %v", err)
	}
}

// A revokable refusal beats dispatching a Worker with nothing to go on.
func TestReworkReasonFileIsRefusedWhenEmpty(t *testing.T) {
	repoRoot := t.TempDir()
	writeRejectRecordAt(t, repoRoot, "T0001", "run-old", "2026-09-14T01:00:00Z", "the recorded reason")

	opts := &SpawnOpts{RepoRoot: repoRoot, TaskID: "T0001", ReasonFile: writeReasonFile(t, "   \n\n\t")}
	if _, err := reworkReasonFrom(opts); err == nil {
		t.Fatal("an empty reason file was accepted — the Worker would be reworked with no reason at all")
	} else if !strings.Contains(err.Error(), "empty") {
		t.Errorf("the refusal does not say the file is empty: %v", err)
	}

	// Nothing was recorded: a refused rework must not rewrite the evidence.
	rec, ok, err := LatestRecord[RejectRecord](repoRoot, "T0001", RecordReject)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(rec.Reasons) != 1 || rec.Reasons[0] != "the recorded reason" {
		t.Errorf("the refusal still wrote a record: ok=%v %q", ok, rec.Reasons)
	}
}

func TestReworkReasonFileIsRefusedWhenUnreadable(t *testing.T) {
	repoRoot := t.TempDir()
	opts := &SpawnOpts{
		RepoRoot:   repoRoot,
		TaskID:     "T0001",
		ReasonFile: filepath.Join(t.TempDir(), "does-not-exist.md"),
	}
	if _, err := reworkReasonFrom(opts); err == nil {
		t.Fatal("a missing reason file was accepted silently")
	} else if !strings.Contains(err.Error(), "reading --reason-file") {
		t.Errorf("the refusal does not name what could not be read: %v", err)
	}
}

// RejectEvidence names what exists and nothing else: the Worker can only read
// paths that are really there, and a rejection that points at a file which
// vanished sends it looking for a defect.
func TestRejectEvidenceNamesOnlyWhatExists(t *testing.T) {
	repoRoot := t.TempDir()
	if got := RejectEvidence(repoRoot, "T0001"); len(got) != 0 {
		t.Fatalf("evidence invented for an empty repo: %q", got)
	}

	resultDir := WorkerTaskDir(repoRoot, "T0001")
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := SaveRegistry(repoRoot, &WorkerRecord{
		TaskID: "T0001", RunID: "run-1", SessionID: "s", ClaudeVersion: "v",
		PID: 1, StartTime: 1, Worktree: repoRoot, Branch: "task/T0001-x",
		BaselineSHA: "0123456789abcdef", RefsBefore: []string{},
		LogPath: filepath.Join(repoRoot, "w.log"), ResultDir: resultDir, StartedAt: "t",
	}); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(resultDir, "collect-report.json")
	if err := os.WriteFile(report, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	verdict := filepath.Join(t.TempDir(), "verdict.md")
	if err := os.WriteFile(verdict, []byte("# verdict\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rv := &ReviewRecord{
		recordMeta:  recordMeta{RecordType: RecordReview, TaskID: "T0001", RunID: "rev-1", At: "2026-09-14T01:00:00Z"},
		Verdict:     "approve",
		Summary:     "fixture",
		VerdictPath: verdict,
	}
	if _, err := WriteRecord(repoRoot, "T0001", RecordReview, "rev-1", rv); err != nil {
		t.Fatal(err)
	}
	g2 := &GateRunRecord{
		recordMeta: recordMeta{RecordType: RecordGateRun, TaskID: "T0001", RunID: "run-1", At: "2026-09-14T01:30:00Z"},
		Gate:       "G2",
		Status:     "passed",
		Jobs:       []GateJobResult{{Job: "job-a", Status: "passed"}},
	}
	if _, err := WriteRecord(repoRoot, "T0001", RecordGateRun, "run-1", g2); err != nil {
		t.Fatal(err)
	}

	got := RejectEvidence(repoRoot, "T0001")
	want := map[string]bool{
		report:  true,
		verdict: true,
		filepath.Join(GatesDir(repoRoot, "T0001"), "gate-run-run-1.json"): true,
	}
	if len(got) != len(want) {
		t.Fatalf("evidence = %q, want 3 entries", got)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected evidence path %q", p)
		}
		if _, err := os.Stat(p); err != nil {
			t.Errorf("evidence names a path that does not exist: %q (%v)", p, err)
		}
	}
}
