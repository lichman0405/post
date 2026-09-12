package devorchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeGateSpec writes a minimal gate spec into a scratch repo and returns
// the repo root and the spec path (absolute).
func writeGateSpec(t *testing.T, content string) (repoRoot, specPath string) {
	t.Helper()
	repoRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, "specs", "orchestrator"), 0o755); err != nil {
		t.Fatal(err)
	}
	specPath = filepath.Join(repoRoot, "specs", "orchestrator", "gates.json")
	if err := os.WriteFile(specPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return repoRoot, specPath
}

const miniGateSpec = `{
  "version": 1,
  "required_jobs": ["job-a", "job-b"],
  "gates": {
    "G1": {"name": "worker", "description": "", "runs_jobs": [], "asserts_jobs": []},
    "G2": {"name": "accept", "description": "", "runs_jobs": ["job-a", "job-b"], "asserts_jobs": []},
    "G3": {"name": "e2e", "description": "", "runs_jobs": [], "asserts_jobs": []},
    "G4": {"name": "merge", "description": "", "runs_jobs": [], "asserts_jobs": ["job-a", "job-b"]}
  },
  "jobs": {
    "job-a": {"steps": [{"run": "echo a-ok"}]},
    "job-b": {"steps": [{"run": "echo b-ok"}, {"run": "false"}]}
  },
  "review": {"required_for_merge": true},
  "task_overrides": {}
}`

// writeCollect writes a collect record with the given status and At.
func writeCollect(t *testing.T, repoRoot, taskID, runID, status, at string) {
	t.Helper()
	if _, err := WriteRecord(repoRoot, taskID, RecordCollect, runID, &CollectRecord{
		recordMeta: recordMeta{RecordType: RecordCollect, TaskID: taskID, RunID: runID, At: at},
		Status:     status, ReportPath: "/none", ResultState: "verification", Summary: "fixture",
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRunGateRecordsCommandsExitAndLogs: the executor runs each step as a
// subprocess, records the exact command, exit code and log file, and a
// failing step fails the job and the gate (later steps skipped, still
// recorded).
func TestRunGateRecordsCommandsExitAndLogs(t *testing.T) {
	repoRoot, specPath := writeGateSpec(t, miniGateSpec)
	res, err := RunGate(&GateRunOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001", Gate: "G2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" {
		t.Fatalf("gate status = %s, want failed (job-b step false)", res.Status)
	}
	if len(res.Jobs) != 2 {
		t.Fatalf("jobs = %d, want 2", len(res.Jobs))
	}
	if res.Jobs[0].Status != "passed" || res.Jobs[1].Status != "failed" {
		t.Errorf("job statuses = %s/%s, want passed/failed", res.Jobs[0].Status, res.Jobs[1].Status)
	}
	// Every step: exact command, exit code, log file with the output.
	if res.Jobs[1].Steps[0].Exit != 0 || res.Jobs[1].Steps[1].Exit != 1 {
		t.Errorf("job-b exits = %d/%d, want 0/1", res.Jobs[1].Steps[0].Exit, res.Jobs[1].Steps[1].Exit)
	}
	if res.Jobs[1].Steps[1].Run != "false" {
		t.Errorf("recorded command = %q, want the exact ci.yml step", res.Jobs[1].Steps[1].Run)
	}
	for _, j := range res.Jobs {
		for _, s := range j.Steps {
			if s.OutputFile == "" {
				t.Errorf("step %s has no output log", s.Run)
				continue
			}
			data, err := os.ReadFile(s.OutputFile)
			if err != nil {
				t.Fatalf("reading step log %s: %v", s.OutputFile, err)
			}
			if len(data) == 0 {
				t.Errorf("step log %s is empty — output was not captured", s.OutputFile)
			}
		}
	}
	// The evidence record landed on disk.
	rec, ok, err := LatestGateRunRecord(repoRoot, "T0001", "G2")
	if err != nil || !ok {
		t.Fatalf("no G2 record after the run: %v, %v", ok, err)
	}
	if rec.Status != "failed" || len(rec.Jobs) != 2 {
		t.Errorf("record = %+v, want failed with 2 jobs", rec)
	}
}

// TestRunGateRefusesG1AndG4: G1 runs at collect and G4 asserts — executing
// them by hand is refused with an explanation, never a silent no-op.
func TestRunGateRefusesG1AndG4(t *testing.T) {
	repoRoot, specPath := writeGateSpec(t, miniGateSpec)
	for _, gate := range []string{"G1", "G4"} {
		if _, err := RunGate(&GateRunOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001", Gate: gate}); err == nil {
			t.Errorf("RunGate(%s) succeeded, want refusal", gate)
		}
	}
}

// TestRunGateAllGreenPasses: with a green spec the gate passes and the
// record is green.
func TestRunGateAllGreenPasses(t *testing.T) {
	repoRoot, specPath := writeGateSpec(t, `{
  "version": 1,
  "required_jobs": ["job-a"],
  "gates": {
    "G1": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": []},
    "G2": {"name": "", "description": "", "runs_jobs": ["job-a"], "asserts_jobs": []},
    "G3": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": []},
    "G4": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": ["job-a"]}
  },
  "jobs": {"job-a": {"steps": [{"run": "echo ok"}]}},
  "review": {"required_for_merge": true},
  "task_overrides": {}
}`)
	res, err := RunGate(&GateRunOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001", Gate: "G2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "passed" {
		t.Errorf("status = %s, want passed", res.Status)
	}
}

// mergeGateFixture builds the evidence a green merge gate needs: an ok
// collect and an all-green G2 covering every required job, at/after the
// collect.
func mergeGateFixture(t *testing.T) (repoRoot, specPath string) {
	repoRoot, specPath = writeGateSpec(t, `{
  "version": 1,
  "required_jobs": ["job-a", "job-b"],
  "gates": {
    "G1": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": []},
    "G2": {"name": "", "description": "", "runs_jobs": ["job-a", "job-b"], "asserts_jobs": []},
    "G3": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": []},
    "G4": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": ["job-a", "job-b"]}
  },
  "jobs": {
    "job-a": {"steps": [{"run": "true"}]},
    "job-b": {"steps": [{"run": "true"}]}
  },
  "review": {"required_for_merge": false},
  "task_overrides": {}
}`)
	taskID := "T0001"
	writeCollect(t, repoRoot, taskID, "coll-1", "ok", "2026-09-12T10:00:00Z")
	green := &GateRunRecord{
		recordMeta: recordMeta{RecordType: RecordGateRun, TaskID: taskID, RunID: "g2-1", At: "2026-09-12T11:00:00Z"},
		Gate:       "G2",
		Status:     "passed",
		Jobs: []GateJobResult{
			{Job: "job-a", Status: "passed"},
			{Job: "job-b", Status: "passed"},
		},
	}
	if _, err := WriteRecord(repoRoot, taskID, RecordGateRun, "g2-1", green); err != nil {
		t.Fatal(err)
	}
	return repoRoot, specPath
}

// TestCheckMergeGateGreen: with an ok collect, an all-green G2 covering
// every required job (at/after the collect) and review not required, the
// merge gate passes.
func TestCheckMergeGateGreen(t *testing.T) {
	repoRoot, specPath := mergeGateFixture(t)
	res, err := CheckMergeGate(repoRoot, specPath, "T0001")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "passed" {
		t.Fatalf("merge gate = %s, reasons %v", res.Status, res.Reasons)
	}
}

// TestCheckMergeGateRedScenarios walks the failure matrix: each single
// defect must refuse with its named reason — missing G2, a subset G2
// (the shipped defect), a red required job, stale G2 evidence, a
// non-ok collect, a missing G3, and a missing review verdict.
func TestCheckMergeGateRedScenarios(t *testing.T) {
	taskID := "T0001"
	scenario := func(t *testing.T, name string, mutate func(repoRoot string)) []string {
		t.Helper()
		repoRoot, specPath := mergeGateFixture(t)
		mutate(repoRoot)
		res, err := CheckMergeGate(repoRoot, specPath, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != "failed" {
			t.Fatalf("%s: merge gate passed, want refusal", name)
		}
		return res.Reasons
	}

	t.Run("no G2 record", func(t *testing.T) {
		reasons := scenario(t, "missing G2", func(repoRoot string) {
			if err := os.Remove(filepath.Join(GatesDir(repoRoot, taskID), "gate-run-g2-1.json")); err != nil {
				t.Fatal(err)
			}
		})
		if len(reasons) == 0 || reasons[0] == "" {
			t.Errorf("no reason recorded: %v", reasons)
		}
	})

	t.Run("subset G2 — the shipped defect", func(t *testing.T) {
		// A green G2 that only ran job-a: the similar-looking subset that let
		// a red PR merge. The merge gate must refuse.
		reasons := scenario(t, "subset G2", func(repoRoot string) {
			subset := &GateRunRecord{
				recordMeta: recordMeta{RecordType: RecordGateRun, TaskID: taskID, RunID: "g2-subset", At: "2026-09-12T12:00:00Z"},
				Gate:       "G2", Status: "passed",
				Jobs: []GateJobResult{{Job: "job-a", Status: "passed"}},
			}
			if _, err := WriteRecord(repoRoot, taskID, RecordGateRun, "g2-subset", subset); err != nil {
				t.Fatal(err)
			}
		})
		joined := ""
		for _, r := range reasons {
			joined += r
		}
		if joined == "" || !containsAny(joined, "missing", "job-b") {
			t.Errorf("subset-G2 reasons = %v, want the missing job named", reasons)
		}
	})

	t.Run("red required job", func(t *testing.T) {
		reasons := scenario(t, "red job", func(repoRoot string) {
			red := &GateRunRecord{
				recordMeta: recordMeta{RecordType: RecordGateRun, TaskID: taskID, RunID: "g2-red", At: "2026-09-12T12:00:00Z"},
				Gate:       "G2", Status: "failed",
				Jobs: []GateJobResult{
					{Job: "job-a", Status: "passed"},
					{Job: "job-b", Status: "failed"},
				},
			}
			if _, err := WriteRecord(repoRoot, taskID, RecordGateRun, "g2-red", red); err != nil {
				t.Fatal(err)
			}
		})
		if len(reasons) == 0 {
			t.Error("red job produced no refusal")
		}
	})

	t.Run("stale G2 — collect after the gate run", func(t *testing.T) {
		reasons := scenario(t, "stale G2", func(repoRoot string) {
			writeCollect(t, repoRoot, taskID, "coll-2", "ok", "2026-09-12T13:00:00Z")
		})
		joined := ""
		for _, r := range reasons {
			joined += r
		}
		if joined == "" || !containsAny(joined, "newer", "re-run") {
			t.Errorf("stale-G2 reasons = %v, want the freshness refusal", reasons)
		}
	})

	t.Run("non-ok collect", func(t *testing.T) {
		reasons := scenario(t, "bad collect", func(repoRoot string) {
			// Newer than the ok collect (so it IS the latest) but not newer
			// than the G2 run (so staleness is not the trigger): the refusal
			// must come from the non-ok collect itself.
			writeCollect(t, repoRoot, taskID, "coll-3", "rejected", "2026-09-12T10:30:00Z")
		})
		if len(reasons) == 0 {
			t.Error("non-ok collect produced no refusal")
		}
	})

	t.Run("G3 required and missing", func(t *testing.T) {
		reasons := scenario(t, "missing G3", func(repoRoot string) {
			// Add a G3 override to the spec: now G3 is required and absent.
			spec, err := LoadGateSpec(filepath.Join(repoRoot, "specs", "orchestrator", "gates.json"))
			if err != nil {
				t.Fatal(err)
			}
			spec.TaskOverrides[taskID] = TaskGateOverride{G3Jobs: []string{"job-a"}}
			data, err := json.MarshalIndent(spec, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repoRoot, "specs", "orchestrator", "gates.json"), data, 0o644); err != nil {
				t.Fatal(err)
			}
		})
		joined := ""
		for _, r := range reasons {
			joined += r
		}
		if joined == "" || !containsAny(joined, "G3") {
			t.Errorf("missing-G3 reasons = %v, want the G3 refusal", reasons)
		}
	})
}

// TestCheckMergeGateReviewVerdictRequired: with review required for merge,
// a missing verdict refuses and a request_changes verdict refuses; an
// approve passes.
func TestCheckMergeGateReviewVerdictRequired(t *testing.T) {
	repoRoot, specPath := mergeGateFixture(t)
	// Turn review on in the spec.
	spec, err := LoadGateSpec(specPath)
	if err != nil {
		t.Fatal(err)
	}
	spec.Review.RequiredForMerge = true
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := CheckMergeGate(repoRoot, specPath, "T0001")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" {
		t.Fatal("merge gate passed without a review verdict while review is required")
	}

	writeReview := func(verdict string, blocking int, at string) {
		t.Helper()
		runID := fmt.Sprintf("rev-%s", verdict)
		if _, err := WriteRecord(repoRoot, "T0001", RecordReview, runID, &ReviewRecord{
			recordMeta:       recordMeta{RecordType: RecordReview, TaskID: "T0001", RunID: runID, At: at},
			Verdict:          verdict,
			Summary:          "fixture",
			BlockingFindings: blocking,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Distinct At values: the record timestamps order the verdicts, exactly
	// as production writers do (nowRFC3339, millisecond precision). Two
	// records sharing one At have no defined order — a fixture must not
	// depend on one.
	writeReview("request_changes", 2, "2026-09-12T11:30:00.000Z")
	res, err = CheckMergeGate(repoRoot, specPath, "T0001")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" {
		t.Fatal("merge gate passed on a request_changes verdict with blocking findings")
	}
	writeReview("approve", 0, "2026-09-12T11:30:00.001Z")
	res, err = CheckMergeGate(repoRoot, specPath, "T0001")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "passed" {
		t.Fatalf("merge gate = %s with an approving verdict, reasons %v", res.Status, res.Reasons)
	}
}

func containsAny(s string, parts ...string) bool {
	for _, p := range parts {
		if p != "" && strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// An approving verdict must not outlive the code it judged. The G2 checks in
// CheckMergeGate already compare evidence against the latest collect; the
// review clause did not, so a verdict written before a rework (or before a
// Supervisor edit to the worktree) still satisfied the merge gate — evidence
// for a tree nobody reviewed.
func TestCheckMergeGateReviewVerdictMustBeFresh(t *testing.T) {
	repoRoot, specPath := mergeGateFixture(t)
	spec, err := LoadGateSpec(specPath)
	if err != nil {
		t.Fatal(err)
	}
	spec.Review.RequiredForMerge = true
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(specPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	writeReview := func(at string) {
		t.Helper()
		if _, err := WriteRecord(repoRoot, "T0001", RecordReview, "rev-"+at, &ReviewRecord{
			recordMeta: recordMeta{RecordType: RecordReview, TaskID: "T0001", RunID: "rev-" + at, At: at},
			Verdict:    "approve", Summary: "fixture",
		}); err != nil {
			t.Fatal(err)
		}
	}

	// The fixture's collect is at 10:00. A verdict from before it approved a
	// tree that no longer exists.
	writeReview("2026-09-12T09:00:00.000Z")
	res, err := CheckMergeGate(repoRoot, specPath, "T0001")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" {
		t.Fatalf("merge gate passed on an approve verdict older than the latest collect: %v", res.Checks)
	}
	if !containsAny(strings.Join(res.Reasons, " "), "older than the latest collect") {
		t.Errorf("refusal does not name the staleness: %v", res.Reasons)
	}

	// A verdict written after the collect is fresh and must pass — without
	// this the rule could be "no verdict ever satisfies the gate".
	writeReview("2026-09-12T12:00:00.000Z")
	res, err = CheckMergeGate(repoRoot, specPath, "T0001")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "passed" {
		t.Fatalf("merge gate = %s on a fresh approving verdict, reasons %v", res.Status, res.Reasons)
	}
}
