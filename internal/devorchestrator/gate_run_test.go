package devorchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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

// A gate must verify what MERGING would produce: current main with the task's
// change applied. Running in the task's own worktree reported green for trees
// that cannot be composed with main at all — five such collisions in P1, each
// of which git called MERGEABLE while the merge did not compile.
//
// This builds the exact shape: the task's branch adds a declaration that main
// has since added too. It compiles on the branch, and cannot compile merged.
func TestGateVerifiesMainPlusTheTaskChange(t *testing.T) {
	repoRoot := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v (in %s): %v\n%s", args, dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(repoRoot, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoRoot, "go.mod"), []byte("module example.test/x\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "base.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repoRoot, "add", "-A")
	git(repoRoot, "commit", "-q", "-m", "base")
	base := git(repoRoot, "rev-parse", "HEAD")

	// The task's branch: a declaration of its own, uncommitted, as a Worker
	// leaves it.
	wt := filepath.Join(t.TempDir(), "wt")
	git(repoRoot, "worktree", "add", "-q", "-b", "task/T0001-x", wt, base)
	if err := os.WriteFile(filepath.Join(wt, "task.go"), []byte("package x\n\ntype Collision struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// main moves on and adds the same declaration.
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte("package x\n\ntype Collision struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repoRoot, "add", "-A")
	git(repoRoot, "commit", "-q", "-m", "main adds the same name")

	specPath := filepath.Join(repoRoot, "gates.json")
	if err := os.WriteFile(specPath, []byte(`{
  "version": 1,
  "required_jobs": ["j"],
  "gates": {
    "G1": {"name": "", "description": "", "runs_jobs": []},
    "G2": {"name": "", "description": "", "runs_jobs": ["j"]},
    "G3": {"name": "", "description": "", "runs_jobs": []},
    "G4": {"name": "", "description": "", "asserts_jobs": ["j"]}
  },
  "jobs": {"j": {"steps": [{"run": "go build ./..."}]}},
  "review": {"required_for_merge": false},
  "task_overrides": {}
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveRegistry(repoRoot, &WorkerRecord{
		TaskID: "T0001", RunID: "run-1", SessionID: "s", ClaudeVersion: "v",
		PID: 1, StartTime: 1, Worktree: wt, Branch: "task/T0001-x",
		BaselineSHA: base, RefsBefore: []string{},
		LogPath: filepath.Join(wt, "worker.log"), ResultDir: wt, StartedAt: "t",
	}); err != nil {
		t.Fatal(err)
	}

	res, err := RunGate(&GateRunOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001", Gate: "G2"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" {
		t.Fatalf("G2 passed on a change that compiles alone and cannot compile merged — MERGEABLE is not compilable:\n%+v", res.Jobs)
	}
	// And the same change alone DOES build, so the gate is distinguishing the
	// composition rather than failing for some unrelated reason.
	if out, err := exec.Command("go", "build", "./...").CombinedOutput(); err == nil {
		_ = out
	}
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = wt
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the fixture is wrong: the change does not even build on its own branch:\n%s", out)
	}
}

// Two gates run by ONE command must not collide on their record filename.
// `task accept` runs G2 and then G3 with the same caller-supplied run id, and
// the record is gate-run-<runID>.json — so G3's write silently replaced G2's,
// and the merge gate then refused with "no G2 gate-run record exists" for a
// task whose G2 had just passed. Every task with a G3 override was unmergeable.
func TestG2AndG3DoNotCollideOnOneRunID(t *testing.T) {
	repoRoot, specPath := writeGateSpec(t, `{
  "version": 1,
  "required_jobs": ["job-a"],
  "gates": {
    "G1": {"name": "", "description": "", "runs_jobs": []},
    "G2": {"name": "", "description": "", "runs_jobs": ["job-a"], "asserts_jobs": []},
    "G3": {"name": "", "description": "", "runs_jobs": ["job-b"], "asserts_jobs": []},
    "G4": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": ["job-a"]}
  },
  "jobs": {
    "job-a": {"steps": [{"run": "true"}]},
    "job-b": {"steps": [{"run": "true"}]}
  },
  "review": {"required_for_merge": false},
  "task_overrides": {"T0001": {"g3_jobs": ["job-b"]}}
}`)
	const shared = "run-shared-by-both-gates"
	for _, gate := range []string{"G2", "G3"} {
		res, err := RunGate(&GateRunOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001", Gate: gate, RunID: shared})
		if err != nil {
			t.Fatalf("%s: %v", gate, err)
		}
		if res.Status != "passed" {
			t.Fatalf("%s status = %s", gate, res.Status)
		}
	}
	g2, ok, err := LatestGateRunRecord(repoRoot, "T0001", "G2")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the G2 record is gone — running G3 under the same run id overwrote it, and the merge gate would refuse this task")
	}
	if g2.Status != "passed" {
		t.Errorf("G2 record status = %s, want passed", g2.Status)
	}
	if _, ok, err := LatestGateRunRecord(repoRoot, "T0001", "G3"); err != nil || !ok {
		t.Errorf("the G3 record is missing (ok=%v err=%v)", ok, err)
	}
}

// A G3 step runs in the integration tree — a worktree of main holding only what
// Git tracks — so the credentials that live only in .env.dev are not there. The
// Gitea probe is the job that needs one and has no substitute for it: it stops
// on "POST_GITEA_TOKEN is not set" before any assertion, which makes every task
// carrying gitea-real-services un-acceptable whatever its work. G3 therefore
// gets the keys listed in devStackEnvKeys. G2 must not: CI has no .env.dev, and a
// local G2 more permissive than the pull request's is the subset-G2 defect
// arriving through the environment instead of through the job list.
//
// Both steps compare against the literal the file holds, never against "empty".
// The key is a real one and the machine running this may already have it in the
// environment — an `-z` assertion would then pass without the file being read at
// all, and a failure message would blame the wrong thing. Comparing to the
// literal is also what pins the documented precedence: the file's value is read
// at run time and must beat whatever the driver's shell was started with.
func TestG3GetsTheDevStackEnvironmentAndG2DoesNot(t *testing.T) {
	repoRoot, specPath := writeGateSpec(t, `{
  "version": 1,
  "required_jobs": ["job-a"],
  "gates": {
    "G1": {"name": "", "description": "", "runs_jobs": []},
    "G2": {"name": "", "description": "", "runs_jobs": ["job-a"], "asserts_jobs": []},
    "G3": {"name": "", "description": "", "runs_jobs": ["job-b"], "asserts_jobs": []},
    "G4": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": ["job-a"]}
  },
  "jobs": {
    "job-a": {"steps": [{"run": "test \"${POST_GITEA_TOKEN:-}\" != from-the-file"}]},
    "job-b": {"steps": [{"run": "test \"${POST_GITEA_TOKEN:-}\" = from-the-file"}]}
  },
  "review": {"required_for_merge": false},
  "task_overrides": {"T0001": {"g3_jobs": ["job-b"]}}
}`)
	if err := os.WriteFile(filepath.Join(repoRoot, ".env.dev"), []byte("POST_GITEA_TOKEN=from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The step's own command is the assertion, so a failure here is the exit
	// code; the log is read back only to say which way it went.
	stepOutput := func(res *GateRunResult) string {
		t.Helper()
		data, err := os.ReadFile(res.Jobs[0].Steps[0].OutputFile)
		if err != nil {
			return "<no output file: " + err.Error() + ">"
		}
		return string(data)
	}

	g2, err := RunGate(&GateRunOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001", Gate: "G2", RunID: "r-g2"})
	if err != nil {
		t.Fatal(err)
	}
	if g2.Status != "passed" {
		t.Errorf("G2 must NOT see .env.dev — CI has no such file, so a local G2 that has it is a different, more permissive gate than the one that grades the pull request:\n%s", stepOutput(g2))
	}

	g3, err := RunGate(&GateRunOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001", Gate: "G3", RunID: "r-g3"})
	if err != nil {
		t.Fatal(err)
	}
	if g3.Status != "passed" {
		t.Errorf("G3 must see .env.dev — a job whose script needs a credential that lives only there cannot run at all, and the gate then refuses a task for work it never checked:\n%s", stepOutput(g3))
	}
}

// G2 re-runs CI's steps, and CI has no dev stack. The shell that starts the
// driver does: the dev-stack variables are exported there, because that is how
// the stack is reachable at all. `cmd.Env = os.Environ()` handed them to every
// gate step regardless, so a G2 step saw credentials CI never provides — the
// defect devStackEnv refuses to introduce through the job list, arriving through
// the environment instead.
//
// That is not hypothetical, and this is the test that was missing. When the
// variable is present, TestG3RunsWithoutADevStackEnvironment — whose whole
// assertion is `test -z "${POST_GITEA_TOKEN:-}"`, on the premise that a missing
// .env.dev means nothing to inject — goes red, and every task accepted from that
// driver is refused for it.
//
// The sentinel is SET here rather than merely observed. The suite passes on a
// shell that exports nothing, which is exactly why this survived: the leak was
// visible only from the one environment the gate actually runs in.
func TestG2StepsDoNotInheritTheShellsDevStackEnvironment(t *testing.T) {
	const sentinel = "left-in-the-shell-that-started-the-driver"
	t.Setenv("POST_GITEA_TOKEN", sentinel)
	// A key that is in .env.dev but is not one of the two devStackEnvKeys, so the
	// exclusion is pinned to "the local dev stack", not to the pair G3 needs.
	t.Setenv("POST_DB_PASSWORD", sentinel)

	repoRoot, specPath := writeGateSpec(t, `{
  "version": 1,
  "required_jobs": ["job-a"],
  "gates": {
    "G1": {"name": "", "description": "", "runs_jobs": []},
    "G2": {"name": "", "description": "", "runs_jobs": ["job-a"], "asserts_jobs": []},
    "G3": {"name": "", "description": "", "runs_jobs": ["job-b"], "asserts_jobs": []},
    "G4": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": ["job-a"]}
  },
  "jobs": {
    "job-a": {"steps": [{"run": "test -z \"${POST_GITEA_TOKEN:-}\" && test -z \"${POST_DB_PASSWORD:-}\""}]},
    "job-b": {"steps": [{"run": "test \"${POST_GITEA_TOKEN:-}\" = from-the-file"}]}
  },
  "review": {"required_for_merge": false},
  "task_overrides": {"T0001": {"g3_jobs": ["job-b"]}}
}`)
	if err := os.WriteFile(filepath.Join(repoRoot, ".env.dev"),
		[]byte("POST_GITEA_TOKEN=from-the-file\nPOST_DB_PASSWORD=from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	g2, err := RunGate(&GateRunOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001", Gate: "G2", RunID: "r-shell"})
	if err != nil {
		t.Fatal(err)
	}
	if g2.Status != "passed" {
		data, _ := os.ReadFile(g2.Jobs[0].Steps[0].OutputFile)
		t.Errorf("a G2 step inherited a dev-stack variable from the shell that started the gate — CI has no .env.dev, so this G2 graded an environment the pull request will never be graded in:\n%s", data)
	}

	// G3 is the other half: it is the LOCAL dev-stack gate, and scrubbing must not
	// have taken the file's value away from it. Asserting the positive value —
	// rather than "not the sentinel" — is what keeps this from passing on a G3
	// that simply saw nothing.
	g3, err := RunGate(&GateRunOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001", Gate: "G3", RunID: "r-shell-g3"})
	if err != nil {
		t.Fatal(err)
	}
	if g3.Status != "passed" {
		data, _ := os.ReadFile(g3.Jobs[0].Steps[0].OutputFile)
		t.Errorf("G3 must still get the dev stack: the file is read at run time and its value must beat the shell's, sentinel or not:\n%s", data)
	}
}

// The file's other keys must not reach the step. A gate step runs a script out
// of the tree under test, and whatever is in its environment is readable by that
// script and lands in the step's captured output the first time anything prints,
// fails or dumps state — so exposing the whole file to satisfy one job's need
// for a token leaks the database password and the blob secret by accident rather
// than by intent.
//
// This is the regression test for the shape that shipped first: devStackEnv
// returned every parsed key, and `gitea-real-services` got POST_DB_PASSWORD
// along with the token it actually needed.
func TestG3DoesNotGetTheRestOfTheDevStackFile(t *testing.T) {
	const secret = "super-secret-from-the-file"
	repoRoot, specPath := writeGateSpec(t, `{
  "version": 1,
  "required_jobs": ["job-a"],
  "gates": {
    "G1": {"name": "", "description": "", "runs_jobs": []},
    "G2": {"name": "", "description": "", "runs_jobs": ["job-a"], "asserts_jobs": []},
    "G3": {"name": "", "description": "", "runs_jobs": ["job-b"], "asserts_jobs": []},
    "G4": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": ["job-a"]}
  },
  "jobs": {
    "job-a": {"steps": [{"run": "true"}]},
    "job-b": {"steps": [
      {"run": "test \"${POST_GITEA_TOKEN:-}\" = from-the-file"},
      {"run": "test \"${POST_DB_PASSWORD:-}\" != super-secret-from-the-file"},
      {"run": "test \"${POST_BLOB_SECRET_KEY:-}\" != super-secret-from-the-file"}
    ]}
  },
  "review": {"required_for_merge": false},
  "task_overrides": {"T0001": {"g3_jobs": ["job-b"]}}
}`)
	env := "POST_GITEA_TOKEN=from-the-file\n" +
		"POST_DB_PASSWORD=" + secret + "\n" +
		"POST_BLOB_SECRET_KEY=" + secret + "\n"
	if err := os.WriteFile(filepath.Join(repoRoot, ".env.dev"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := RunGate(&GateRunOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001", Gate: "G3", RunID: "r-scope"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "passed" {
		for i, s := range res.Jobs[0].Steps {
			if s.Exit != 0 {
				data, _ := os.ReadFile(s.OutputFile)
				t.Errorf("step %d (%s) exit=%d — a G3 step must get devStackEnvKeys and nothing else from .env.dev:\n%s", i, s.Run, s.Exit, data)
			}
		}
	}
}

// The exposed set is pinned, so widening it is a deliberate edit with a test
// that has to be changed too — not a line that quietly rides along with the next
// key someone adds to .env.dev.
func TestDevStackEnvKeysIsPinned(t *testing.T) {
	want := []string{"POST_GITEA_BASE_URL", "POST_GITEA_TOKEN"}
	if len(devStackEnvKeys) != len(want) {
		t.Fatalf("devStackEnvKeys = %v, want exactly %v — widening what a G3 step may see needs a reason recorded next to it, not just an entry", devStackEnvKeys, want)
	}
	for i, k := range want {
		if devStackEnvKeys[i] != k {
			t.Fatalf("devStackEnvKeys = %v, want %v", devStackEnvKeys, want)
		}
	}
}

// JobEnv is documented as "extra environment for every job", so a caller that
// sets one is stating the value it wants. The dev-stack file is read by the
// executor on the caller's behalf, not named by it, so it must not overrule
// what the caller did name — otherwise the same task grades differently in the
// driver and in a test that passes JobEnv, with nothing to say which is right.
func TestG3JobEnvWinsOverTheDevStackFile(t *testing.T) {
	repoRoot, specPath := writeGateSpec(t, `{
  "version": 1,
  "required_jobs": ["job-a"],
  "gates": {
    "G1": {"name": "", "description": "", "runs_jobs": []},
    "G2": {"name": "", "description": "", "runs_jobs": ["job-a"], "asserts_jobs": []},
    "G3": {"name": "", "description": "", "runs_jobs": ["job-b"], "asserts_jobs": []},
    "G4": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": ["job-a"]}
  },
  "jobs": {
    "job-a": {"steps": [{"run": "true"}]},
    "job-b": {"steps": [{"run": "test \"${POST_GITEA_TOKEN:-}\" = from-the-caller"}]}
  },
  "review": {"required_for_merge": false},
  "task_overrides": {"T0001": {"g3_jobs": ["job-b"]}}
}`)
	if err := os.WriteFile(filepath.Join(repoRoot, ".env.dev"), []byte("POST_GITEA_TOKEN=from-the-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := RunGate(&GateRunOpts{
		RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001", Gate: "G3", RunID: "r-override",
		JobEnv: map[string]string{"POST_GITEA_TOKEN": "from-the-caller"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "passed" {
		data, readErr := os.ReadFile(res.Jobs[0].Steps[0].OutputFile)
		t.Errorf("the caller's JobEnv must win over .env.dev; the step neither saw the caller's value nor errored: %v\n%s", readErr, data)
	}
}

// A missing file is not a broken one, and must not be: .env.dev is gitignored,
// so a fresh clone has none, and refusing the gate outright would take G3 from
// "one job cannot run" to "no task can be graded" for everyone who has not
// configured the dev stack. The step is left to say what it is missing — the
// Gitea probe already names the variable, how to mint it and where to put it,
// which is more than a parse error would.
func TestG3RunsWithoutADevStackEnvironment(t *testing.T) {
	repoRoot, specPath := writeGateSpec(t, `{
  "version": 1,
  "required_jobs": ["job-a"],
  "gates": {
    "G1": {"name": "", "description": "", "runs_jobs": []},
    "G2": {"name": "", "description": "", "runs_jobs": ["job-a"], "asserts_jobs": []},
    "G3": {"name": "", "description": "", "runs_jobs": ["job-b"], "asserts_jobs": []},
    "G4": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": ["job-a"]}
  },
  "jobs": {
    "job-a": {"steps": [{"run": "true"}]},
    "job-b": {"steps": [{"run": "test -z \"${POST_GITEA_TOKEN:-}\""}]}
  },
  "review": {"required_for_merge": false},
  "task_overrides": {"T0001": {"g3_jobs": ["job-b"]}}
}`)
	if _, err := os.Stat(filepath.Join(repoRoot, ".env.dev")); err == nil {
		t.Fatal("the fixture has a .env.dev — this test is about there being none")
	}

	// An error here would be the whole gate refusing over an unconfigured dev
	// stack; a failed job would be the step seeing a value that is not on disk.
	res, err := RunGate(&GateRunOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001", Gate: "G3", RunID: "r-none"})
	if err != nil {
		t.Fatalf("G3 must still run when there is no .env.dev: %v", err)
	}
	if res.Status != "passed" {
		t.Errorf("G3 status = %s, want passed — with no .env.dev there is nothing to inject and the step's own command decides the outcome", res.Status)
	}
}

// The file is parsed with the repository's own reader, so a malformed one fails
// the gate loudly rather than being half-applied: a gate that silently drops the
// credentials it could not parse is the same un-acceptable-every-task failure as
// not reading the file at all, with no error to explain it.
func TestG3RefusesAMalformedDevStackEnvironment(t *testing.T) {
	repoRoot, specPath := writeGateSpec(t, `{
  "version": 1,
  "required_jobs": ["job-a"],
  "gates": {
    "G1": {"name": "", "description": "", "runs_jobs": []},
    "G2": {"name": "", "description": "", "runs_jobs": ["job-a"], "asserts_jobs": []},
    "G3": {"name": "", "description": "", "runs_jobs": ["job-b"], "asserts_jobs": []},
    "G4": {"name": "", "description": "", "runs_jobs": [], "asserts_jobs": ["job-a"]}
  },
  "jobs": {
    "job-a": {"steps": [{"run": "true"}]},
    "job-b": {"steps": [{"run": "true"}]}
  },
  "review": {"required_for_merge": false},
  "task_overrides": {"T0001": {"g3_jobs": ["job-b"]}}
}`)
	if err := os.WriteFile(filepath.Join(repoRoot, ".env.dev"), []byte("POST_GITEA_TOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := RunGate(&GateRunOpts{RepoRoot: repoRoot, GatesPath: specPath, TaskID: "T0001", Gate: "G3", RunID: "r-bad"})
	if err == nil {
		t.Fatal("a malformed .env.dev was accepted — the G3 job would have run without the credentials it needs and the failure would be reported as the task's")
	}
	if !strings.Contains(err.Error(), ".env.dev") {
		t.Errorf("the refusal does not name the file: %v", err)
	}
}

// A verdict must not outlive the code it judged. Writing approve for one code
// state and then changing the code — an edit, a rebase, a baseline advance, a
// merge repair — must invalidate it, not silently carry it forward. The
// identity is base-sensitive AND stable across the commit pr open makes, so it
// survives the commit the verdict authorised and nothing else.
func TestReviewVerdictIsBoundToTheCodeItJudged(t *testing.T) {
	repoRoot := t.TempDir()
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v (in %s): %v\n%s", args, dir, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(repoRoot, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoRoot, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repoRoot, "add", "-A")
	git(repoRoot, "commit", "-q", "-m", "base")
	base := git(repoRoot, "rev-parse", "HEAD")

	wt := filepath.Join(t.TempDir(), "wt")
	git(repoRoot, "worktree", "add", "-q", "-b", "task/T0001-x", wt, base)
	if err := os.WriteFile(filepath.Join(wt, "deliverable.txt"), []byte("the task's work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := &WorkerRecord{
		TaskID: "T0001", RunID: "run-1", SessionID: "s", ClaudeVersion: "v",
		PID: 1, StartTime: 1, Worktree: wt, Branch: "task/T0001-x",
		BaselineSHA: base, RefsBefore: []string{},
		LogPath: filepath.Join(wt, "worker.log"), ResultDir: wt, StartedAt: "t",
	}
	if err := SaveRegistry(repoRoot, rec); err != nil {
		t.Fatal(err)
	}
	specPath := filepath.Join(repoRoot, "gates.json")
	if err := os.WriteFile(specPath, []byte(`{
  "version": 1, "required_jobs": ["j"],
  "gates": {"G1": {"name":"","description":"","runs_jobs":[]},
            "G2": {"name":"","description":"","runs_jobs":["j"]},
            "G3": {"name":"","description":"","runs_jobs":[]},
            "G4": {"name":"","description":"","asserts_jobs":["j"]}},
  "jobs": {"j": {"steps": [{"run": "true"}]}},
  "review": {"required_for_merge": true}, "task_overrides": {}
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCollect(t, repoRoot, "T0001", "coll-1", "ok", "2020-01-01T00:00:00Z")
	green := &GateRunRecord{
		recordMeta: recordMeta{RecordType: RecordGateRun, TaskID: "T0001", RunID: "g2-1", At: "2020-01-02T00:00:00Z"},
		Gate:       "G2", Status: "passed",
		Jobs: []GateJobResult{{Job: "j", Status: "passed"}},
	}
	if _, err := WriteRecord(repoRoot, "T0001", RecordGateRun, "g2-1", green); err != nil {
		t.Fatal(err)
	}
	writeVerdict := func(diffSHA string) {
		t.Helper()
		if _, err := WriteRecord(repoRoot, "T0001", RecordReview, "rev-"+diffSHA[:6], &ReviewRecord{
			recordMeta: recordMeta{RecordType: RecordReview, TaskID: "T0001", RunID: "rev-" + diffSHA[:6], At: "2030-01-01T00:00:00Z"},
			Verdict:    "approve", Summary: "fixture", DiffSHA: diffSHA,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// A verdict from another code state is refused.
	writeVerdict("0000000000000000000000000000000000000000000000000000000000000000")
	res, err := CheckMergeGate(repoRoot, specPath, "T0001")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" {
		t.Fatalf("the merge gate accepted a verdict written for a different code state: %v", res.Checks)
	}
	if !containsAny(strings.Join(res.Reasons, " "), "DIFFERENT code state") {
		t.Errorf("refusal does not name the mismatch: %v", res.Reasons)
	}

	// The verdict for the CURRENT state passes — otherwise the rule would
	// refuse every verdict, which is its own kind of broken.
	want, err := codeIdentity(rec)
	if err != nil {
		t.Fatal(err)
	}
	writeVerdict(want)
	if res, err = CheckMergeGate(repoRoot, specPath, "T0001"); err != nil {
		t.Fatal(err)
	}
	if res.Status != "passed" {
		t.Fatalf("the merge gate refused a verdict bound to the current code, reasons %v", res.Reasons)
	}

	// Committing exactly that content must NOT invalidate the verdict: it is
	// still the code the reviewer read, and `rddev pr open` commits it as part
	// of authorising the merge. Hashing the rendered diff broke this — a new
	// file renders differently untracked vs committed — which the four-gate e2e
	// caught. The identity hashes content, so it survives.
	git(wt, "add", "-A")
	git(wt, "commit", "-q", "-m", "the task's work, committed")
	if committed, err := codeIdentity(rec); err != nil {
		t.Fatal(err)
	} else if committed != want {
		t.Fatalf("committing the reviewed content changed the code identity:\n  before %s\n  after  %s", want, committed)
	}
	if res, err = CheckMergeGate(repoRoot, specPath, "T0001"); err != nil {
		t.Fatal(err)
	}
	if res.Status != "passed" {
		t.Fatalf("the merge gate refused after the reviewed content was committed: %v", res.Reasons)
	}

	// Main moving under the branch changes the composition, so the same
	// verdict is now stale — this is the baseline-drift case specifically.
	if err := os.WriteFile(filepath.Join(repoRoot, "main-moved.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(repoRoot, "add", "-A")
	git(repoRoot, "commit", "-q", "-m", "main moves")
	git(wt, "merge", "-q", "--no-edit", "main")
	if res, err = CheckMergeGate(repoRoot, specPath, "T0001"); err != nil {
		t.Fatal(err)
	}
	if res.Status != "failed" {
		t.Fatalf("a baseline advance left the old verdict valid — the composition changed and the verdict never saw it: %v", res.Checks)
	}
}
