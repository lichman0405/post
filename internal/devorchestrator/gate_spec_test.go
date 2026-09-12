package devorchestrator

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// TestGatesSpecSyncsWithCIWorkflow is the anti-subset guarantee: G2 runs
// CI's EXACT steps, so the job list and every step command in
// specs/orchestrator/gates.json must match .github/workflows/ci.yml
// verbatim (including per-step env). When someone edits ci.yml without the
// gate spec — or vice versa — this test fails the build instead of the merge
// gate silently running a similar-looking subset (the defect that let a red
// PR merge).
func TestGatesSpecSyncsWithCIWorkflow(t *testing.T) {
	root := repoRootOf(t)
	spec, err := LoadGateSpec(filepath.Join(root, DefaultGatesPath))
	if err != nil {
		t.Fatal(err)
	}

	ci := parseCIWorkflow(t, filepath.Join(root, ".github", "workflows", "ci.yml"))
	if len(ci) != 6 {
		t.Fatalf("ci.yml declares %d jobs, want 6", len(ci))
	}
	for job, steps := range ci {
		gj, ok := spec.Jobs[job]
		if !ok {
			t.Errorf("ci.yml job %q has no definition in %s", job, DefaultGatesPath)
			continue
		}
		if len(gj.Steps) != len(steps) {
			t.Errorf("job %q: gates.json has %d steps, ci.yml has %d", job, len(gj.Steps), len(steps))
		}
		for i := range gj.Steps {
			if i >= len(steps) {
				break
			}
			if gj.Steps[i].Run != steps[i].run {
				t.Errorf("job %q step %d: gates.json runs %q, ci.yml runs %q", job, i, gj.Steps[i].Run, steps[i].run)
			}
			if !reflect.DeepEqual(gj.Steps[i].Env, steps[i].env) {
				t.Errorf("job %q step %d: env %v != ci.yml env %v", job, i, gj.Steps[i].Env, steps[i].env)
			}
		}
	}

	// The required-jobs list is the G4 assertion's backbone: every CI job,
	// exactly the CI jobs, in canonical order.
	if !equalStrings(spec.RequiredJobs, []string{"spec-validation", "task-state", "go", "web", "python", "migration-integration"}) {
		t.Errorf("required_jobs = %v, want the six CI jobs in canonical order", spec.RequiredJobs)
	}
}

// ciStep is one run step of ci.yml (uses/ actions and name lines are
// skipped; only `run` steps are executable, which is what the gate executor
// executes).
type ciStep struct {
	run string
	env map[string]string
}

// parseCIWorkflow extracts job -> run steps from ci.yml. The YAML shape is
// the file's own: jobs.<name>.steps[] with `run` strings and optional `env`
// maps; `uses:` steps are not executable and are skipped.
func parseCIWorkflow(t *testing.T, path string) map[string][]ciStep {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string            `yaml:"run"`
				Env map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	out := map[string][]ciStep{}
	for name, job := range doc.Jobs {
		for _, s := range job.Steps {
			if s.Run == "" {
				continue // uses: / with: — not an executable step
			}
			out[name] = append(out[name], ciStep{run: s.Run, env: s.Env})
		}
	}
	return out
}

// repoRootOf resolves the repository root from the test's working directory
// (the package dir — repoRoot/../../).
func repoRootOf(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("cannot locate the repo root from %q: %v", root, err)
	}
	return root
}

// TestGateSpecLoadValidation: the spec must refuse to load when any gate is
// missing, a required job has no definition, or a gate names an undefined
// job — an unloadable gate spec must never degrade into "no gates".
func TestGateSpecLoadValidation(t *testing.T) {
	root := repoRootOf(t)
	spec, err := LoadGateSpec(filepath.Join(root, DefaultGatesPath))
	if err != nil {
		t.Fatal(err)
	}
	if spec.Version != 1 {
		t.Errorf("version = %d, want 1", spec.Version)
	}
	for _, g := range []string{"G1", "G2", "G3", "G4"} {
		if _, ok := spec.Gates[g]; !ok {
			t.Errorf("gate %s missing", g)
		}
	}

	// G2 must run the same six jobs (the exact-steps requirement).
	jobs, err := spec.JobsForGate("G2", "T0000")
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(jobs, spec.RequiredJobs) {
		t.Errorf("G2 jobs = %v, want the required CI jobs", jobs)
	}
	// G1/G4 run nothing (collect checks; assertion).
	for _, g := range []string{"G1", "G4"} {
		if jobs, err := spec.JobsForGate(g, "T0000"); err != nil || len(jobs) != 0 {
			t.Errorf("JobsForGate(%s) = %v, %v; want none", g, jobs, err)
		}
	}
	// Review required by default, overridable per task.
	if !spec.ReviewRequiredForMerge("T0000") {
		t.Error("review must be required for merge by default")
	}

	// A per-task G3 override supplies G3 jobs.
	withOverride := &GateSpec{
		Version:       1,
		RequiredJobs:  []string{"go"},
		Gates:         map[string]GateDef{"G1": {}, "G2": {}, "G3": {}, "G4": {}},
		Jobs:          map[string]GateJob{"go": {Steps: []GateStep{{Run: "true"}}}},
		TaskOverrides: map[string]TaskGateOverride{"T0001": {G3Jobs: []string{"go"}}},
	}
	if jobs, err := withOverride.JobsForGate("G3", "T0001"); err != nil || !equalStrings(jobs, []string{"go"}) {
		t.Errorf("G3 override jobs = %v, %v; want [go]", jobs, err)
	}
	if jobs, err := withOverride.JobsForGate("G3", "T0002"); err != nil || len(jobs) != 0 {
		t.Errorf("G3 without override jobs = %v, %v; want none", jobs, err)
	}

	// Corrupt specs must refuse to load.
	bad := t.TempDir()
	cases := map[string]string{
		"missing-gate.json":         `{"version":1,"required_jobs":["go"],"gates":{"G1":{}},"jobs":{"go":{"steps":[{"run":"true"}]}}}`,
		"missing-required-def.json": `{"version":1,"required_jobs":["go"],"gates":{"G1":{},"G2":{},"G3":{},"G4":{}},"jobs":{}}`,
		"empty-steps.json":          `{"version":1,"required_jobs":["go"],"gates":{"G1":{},"G2":{},"G3":{},"G4":{}},"jobs":{"go":{"steps":[]}}}`,
		"bad-version.json":          `{"version":2,"required_jobs":["go"],"gates":{"G1":{},"G2":{},"G3":{},"G4":{}},"jobs":{"go":{"steps":[{"run":"true"}]}}}`,
		"no-required-jobs.json":     `{"version":1,"required_jobs":[],"gates":{"G1":{},"G2":{},"G3":{},"G4":{}},"jobs":{"go":{"steps":[{"run":"true"}]}}}`,
	}
	for name, content := range cases {
		path := filepath.Join(bad, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadGateSpec(path); err == nil {
			t.Errorf("%s: LoadGateSpec succeeded, want refusal", name)
		}
	}
}

// G3 was vacuous for all 132 tasks: task_overrides was empty, so every task
// recorded G3 as not_required while docs/67 requires real services for
// cross-boundary work. This asserts the wiring against the REAL spec rather
// than a fixture, because the failure mode is silence — a missing or
// misspelled override simply makes G3 disappear again, and nothing else
// notices.
func TestG3IsWiredForTheTasksThatNeedIt(t *testing.T) {
	root := repoRootOf(t)
	spec, err := LoadGateSpec(filepath.Join(root, DefaultGatesPath))
	if err != nil {
		t.Fatal(err)
	}
	for _, taskID := range []string{"T0102", "T0103"} {
		jobs, err := spec.JobsForGate("G3", taskID)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 0 {
			t.Errorf("task %s has no G3 jobs — it would be accepted with G3 recorded as not_required", taskID)
			continue
		}
		for _, j := range jobs {
			if _, ok := spec.Jobs[j]; !ok {
				t.Errorf("task %s names G3 job %q which is not defined in %s", taskID, j, DefaultGatesPath)
			}
		}
	}
	// A G3 job is not a CI job: it must not leak into the G4 assertion.
	for _, j := range spec.RequiredJobs {
		if j == "auth-real-services" {
			t.Errorf("the G3-only job leaked into required_jobs (%v) — G4 would then demand it from every task's G2 record", spec.RequiredJobs)
		}
	}
}
