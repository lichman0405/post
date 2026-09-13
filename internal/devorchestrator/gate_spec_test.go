package devorchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
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
	if len(ci) != 7 {
		t.Fatalf("ci.yml declares %d jobs, want 7", len(ci))
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
	if !equalStrings(spec.RequiredJobs, []string{"spec-validation", "task-state", "go", "web", "python", "migration-integration", "acceptance"}) {
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
		"no-required-jobs.json":     `{"version":1,"required_jobs":["j"],"gates":{"G1":{},"G2":{},"G3":{},"G4":{}},"jobs":{"go":{"steps":[{"run":"true"}]}}}`,
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
// cross-boundary work. "There is no G3 job yet" must never be the default a
// phase is developed under - that is how a whole phase ships with no
// integration gate at all.
//
// Asserted against the REAL DAG and the REAL spec rather than fixtures,
// because the failure mode is silence: a missing override simply makes G3
// disappear and nothing else notices.
func TestEveryTaskOfThePhasesUnderDevelopmentHasG3(t *testing.T) {
	root := repoRootOf(t)
	spec, err := LoadGateSpec(filepath.Join(root, DefaultGatesPath))
	if err != nil {
		t.Fatal(err)
	}
	dag, err := LoadDAG(filepath.Join(root, DefaultDAGPath))
	if err != nil {
		t.Fatal(err)
	}
	// Every phase that still has work in it. The rule is "a phase has its
	// real-services gate before its tasks run", so it binds on phases that can
	// still run and stops binding on phases that are finished — wiring a gate
	// onto T0000's environment preflight today would be ceremony, not
	// verification, and the tasks that needed real services in P0 carried them
	// in their own G2 steps at the time.
	//
	// The first version of this test named P1-P3 explicitly. That is the same
	// discovery-by-hand the rule exists to remove: the driver hit it for P6
	// within the hour, and would have hit it again for P4, P5 and P7 to P12.
	merged := map[string]bool{}
	if raw, err := os.ReadFile(filepath.Join(root, DefaultStatePath)); err == nil {
		var doc struct {
			Tasks map[string]struct {
				Status string `json:"status"`
			} `json:"tasks"`
		}
		if err := json.Unmarshal(raw, &doc); err == nil {
			for id, e := range doc.Tasks {
				merged[id] = e.Status == string(StateMerged)
			}
		}
	}
	livePhases := map[string]bool{}
	for _, task := range dag.Tasks {
		if !merged[task.ID] {
			livePhases[task.Phase] = true
		}
	}
	covered := 0
	for _, task := range dag.Tasks {
		if !livePhases[task.Phase] {
			continue // the phase is finished; its gate no longer binds
		}
		jobs, err := spec.JobsForGate("G3", task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 0 {
			t.Errorf("task %s (%s, %s) has no G3 jobs — it would be accepted with G3 recorded as not_required, which is how a phase ships with no integration gate",
				task.ID, task.Phase, task.Title)
			continue
		}
		covered++
		for _, j := range jobs {
			if _, ok := spec.Jobs[j]; !ok {
				t.Errorf("task %s names G3 job %q which is not defined in %s", task.ID, j, DefaultGatesPath)
			}
		}
	}
	if covered == 0 {
		t.Fatal("no task carries a G3 job — task_overrides has gone vacuous again")
	}
	// A G3 job is not a CI job: it must not leak into the G4 assertion, which
	// checks the required CI jobs against every task's G2 record.
	for _, j := range []string{"auth-real-services", "rsg-real-services", "gitea-real-services"} {
		for _, req := range spec.RequiredJobs {
			if req == j {
				t.Errorf("the G3-only job %q leaked into required_jobs (%v)", j, spec.RequiredJobs)
			}
		}
		if _, ok := spec.Jobs[j]; !ok {
			t.Errorf("G3 job %q is referenced by an override but not defined", j)
		}
	}
	// A job naming a script that does not exist fails at accept time, which is
	// late - the whole point of wiring it now is to fail early.
	for _, job := range []string{"auth-real-services", "rsg-real-services", "gitea-real-services"} {
		def, ok := spec.Jobs[job]
		if !ok {
			continue
		}
		for _, st := range def.Steps {
			for _, word := range strings.Fields(st.Run) {
				if strings.HasSuffix(word, ".sh") {
					if _, err := os.Stat(filepath.Join(root, word)); err != nil {
						t.Errorf("G3 job %s runs %q, which does not exist: %v", job, word, err)
					}
				}
			}
		}
	}
}

// "There is no G3 job yet" must not be the default a phase is developed under.
// G3 was vacuous for all 132 tasks because task_overrides started empty, and
// nothing noticed - every task was simply accepted with G3 recorded as
// not_required. A whole phase can ship that way, invisibly, because each
// task's own evidence looks complete.
func TestDispatchRefusesAPhaseWithNoRealServicesGate(t *testing.T) {
	repoRoot := t.TempDir()
	writeFile := func(rel, content string) {
		t.Helper()
		full := filepath.Join(repoRoot, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("tasks/tasks.json", `{"version":1,"task_count":2,"phases":{},"tasks":[
		{"id":"T0201","phase":"P2","title":"a","dependencies":[],"requirements":[],"acceptance_criteria":[],
		 "allowed_scope":["x/**"],"decision_level_max":"L1"},
		{"id":"T0202","phase":"P2","title":"b","dependencies":["T0201"],"requirements":[],"acceptance_criteria":[],
		 "allowed_scope":["x/**"],"decision_level_max":"L1"}]}`)
	dagPath := filepath.Join(repoRoot, DefaultDAGPath)
	dag, err := LoadDAG(dagPath)
	if err != nil {
		t.Fatal(err)
	}
	task := dag.Get("T0201")

	// No G3 anywhere in P2: refuse.
	writeFile(DefaultGatesPath, `{"version":1,"required_jobs":["j"],"gates":{"G1":{"name":"","runs_jobs":[]},"G2":{"name":"","runs_jobs":[]},"G3":{"name":"","runs_jobs":[]},"G4":{"name":"","asserts_jobs":["j"]}},"jobs":{"j":{"steps":[{"run":"true"}]}},
		"review":{"required_for_merge":false},"task_overrides":{}}`)
	err = requirePhaseG3Coverage(repoRoot, DefaultGatesPath, dagPath, task)
	if err == nil {
		t.Fatal("dispatch was allowed for a phase where no task carries a G3 — the whole phase would be developed against mocks")
	}
	if !strings.Contains(err.Error(), "no G3 job") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}

	// A G3 on a SIBLING task is enough: the gate is per phase, and T0201 needs
	// no G3 of its own to benefit from the phase having one.
	writeFile(DefaultGatesPath, `{"version":1,"required_jobs":["j"],"gates":{"G1":{"name":"","runs_jobs":[]},"G2":{"name":"","runs_jobs":[]},"G3":{"name":"","runs_jobs":[]},"G4":{"name":"","asserts_jobs":["j"]}},
		"jobs":{"j":{"steps":[{"run":"true"}]},"rsg":{"steps":[{"run":"true"}]}},
		"review":{"required_for_merge":false},
		"task_overrides":{"T0202":{"g3_jobs":["rsg"]}}}`)
	if err := requirePhaseG3Coverage(repoRoot, DefaultGatesPath, dagPath, task); err != nil {
		t.Fatalf("dispatch refused although a sibling task carries the phase's G3: %v", err)
	}
}

// Every task whose scope covers a spec file must also cover the marker derived
// from it (specs/orchestrator/derived-artifacts.json). 116 tasks did not, and
// nothing noticed because the rule is only enforced at dispatch — the first P2
// dispatch found it, which is a checkpoint too late to be cheap. A test over
// the real DAG finds it here instead.
func TestEveryTaskScopeSatisfiesTheDerivedArtifactRule(t *testing.T) {
	root := repoRootOf(t)
	dag, err := LoadDAG(filepath.Join(root, DefaultDAGPath))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := LoadDerivedArtifacts(filepath.Join(root, DefaultDerivedArtifactsPath))
	if err != nil {
		t.Fatal(err)
	}
	// The same matcher the dispatch-time validation uses, so the test cannot
	// disagree with the gate it is standing in for.
	// The same matcher the dispatch-time validation uses (nil derived: this is
	// the plain glob check), so the test cannot disagree with the gate it
	// stands in for.
	covers := func(entry, scope string) bool {
		return ScopeMatchesPathWithDerived(entry, []string{scope}, nil)
	}
	for _, task := range dag.Tasks {
		for _, rule := range rules.Rules {
			markerCovered := false
			for _, s := range task.AllowedScope {
				if covers(rule.Marker, s) {
					markerCovered = true
					break
				}
			}
			if !markerCovered {
				continue
			}
			covered := false
			for _, s := range task.AllowedScope {
				if covers(rule.Derived, s) {
					covered = true
					break
				}
			}
			if !covered {
				t.Errorf("task %s: allowed_scope covers %q but not its derived artifact %q — dispatch refuses this, and the Worker cannot regenerate the artifact it must keep consistent",
					task.ID, rule.Marker, rule.Derived)
			}
		}
	}
}

// A G3 job that asserts the product must be satisfiable by the task that
// carries it. The job runs against the task's own tree, so a job whose
// subject only comes into existence with some other task can pass only once
// that task is merged, and the DAG says when that is: it must be in the
// task's dependency closure, or the run has to be ordered after it by hand
// every time.
//
// A gate wired onto a task that cannot satisfy it is red by construction.
// No amount of correct work turns it green, and the red is indistinguishable
// from a real integration failure — so the task sits in verification while
// the failure report names a path the task was never supposed to serve.
//
// It is not hypothetical. `rsg-real-services` drives the RSG core path
// (object -> version -> relation -> :validate) and was wired onto every task
// of P2 from T0204 onward and onto P4..P12, on the recorded belief that "the
// chain completes at T0204" (decisions.md L1-20260913-11). T0204 does not
// complete it: the paths the script asserts are served by T0203 (relations),
// T0207 (:validate) and T0208 (objects and versions). None of those three was
// in anybody's dependency closure, so the first task to reach acceptance was
// refused with eight unserved paths (T0603) and every phase after P2 was
// queued to hit the same wall.
//
// Fixed here in two halves:
//
//   - tasks.json gained the missing edges (T0208 += T0207 for the chain
//     itself, T0206 += T0208 for the one task that read the chain's result
//     without waiting for it, and each phase entry task += T0208), so a task
//     carrying the RSG gate now waits for the task that completes the chain.
//     That is 93 of the 96 carriers, and this test is what keeps it true.
//
//   - Three tasks cannot be repaired by an edge at all; they are pinned below
//     with the reason that makes each one structural. The chain's completion
//     depends on their own work, so the edge that would add the requirement
//     closes a cycle.
//
// The check has two ways to be wrong, and both are checked rather than
// assumed: a task carrying a job it cannot satisfy (the loop), and a job that
// declares nothing at all (the pin after it). The second is the quieter one —
// a new job running the very same product script, wired onto any task, with no
// requires_tasks would leave the loop nothing to compare and pass by vacuity.
func TestEveryG3JobIsSatisfiableByTheTaskThatCarriesIt(t *testing.T) {
	root := repoRootOf(t)
	spec, err := LoadGateSpec(filepath.Join(root, DefaultGatesPath))
	if err != nil {
		t.Fatal(err)
	}
	dag, err := LoadDAG(filepath.Join(root, DefaultDAGPath))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]TaskSpec{}
	for _, task := range dag.Tasks {
		byID[task.ID] = task
	}
	// The walk below answers "what does this task transitively depend on", and
	// on a cyclic graph that question has no answer: the memo returns a
	// half-built closure, and the verdicts computed from it are about the wrong
	// tasks. LoadDAG does NOT reject cycles (it checks duplicate ids and unknown
	// dependencies); scripts/validate_specs.py's TASKS-ACYCLIC check does, in
	// the spec-validation job. So this test refuses rather than guesses — a
	// cycle that got past that check fails here by name.
	closure := map[string]map[string]bool{}
	visiting := map[string]bool{}
	var reach func(id string) map[string]bool
	reach = func(id string) map[string]bool {
		if visiting[id] {
			t.Fatalf("the task DAG has a dependency cycle through %s, so no dependency closure exists — scripts/validate_specs.py (TASKS-ACYCLIC) should have refused this spec first, and satisfiability cannot be judged on a cyclic graph", id)
		}
		if c, ok := closure[id]; ok {
			return c
		}
		visiting[id] = true
		defer delete(visiting, id)
		c := map[string]bool{}
		closure[id] = c
		for _, dep := range byID[id].Dependencies {
			c[dep] = true
			for transitive := range reach(dep) {
				c[transitive] = true
			}
		}
		return c
	}
	// The carriers wiring cannot help, pinned so the set cannot grow. Each is a
	// knot rather than an oversight: the work the job asserts is downstream of
	// the carrier itself, so the edge that would add the requirement closes a
	// cycle. Fixing one is a decision about what the gate means (change the
	// task's gate, or split the work so the asserted paths exist earlier), not
	// a missing edge — L1-20260913-19.
	carriersTheChainTraps := map[string]string{
		"T0204": "Project State & State Commit: T0208 (objects and versions) needs T0205 and T0207, and both need this task — an edge to T0208 closes a cycle",
		"T0205": "Research Branch Domain: T0208 needs this task's branches directly",
		"T0207": "Progressive Validation Gates: T0208 needs this task's :validate directly",
	}
	unsatisfiable := map[string]bool{}
	for id, override := range spec.TaskOverrides {
		task, ok := byID[id]
		if !ok {
			t.Errorf("task_overrides names task %q, which %s does not contain", id, DefaultDAGPath)
			continue
		}
		for _, jobName := range override.G3Jobs {
			job, ok := spec.Jobs[jobName]
			if !ok {
				t.Errorf("task %s names G3 job %q, which is not defined in %s", id, jobName, DefaultGatesPath)
				continue
			}
			for _, needed := range job.RequiresTasks {
				if _, known := byID[needed]; !known && needed != id {
					t.Errorf("G3 job %q requires task %q, which %s does not contain", jobName, needed, DefaultDAGPath)
					continue
				}
				if needed == id || reach(id)[needed] {
					continue
				}
				if _, trapped := carriersTheChainTraps[id]; trapped {
					unsatisfiable[id] = true
					continue
				}
				t.Errorf("task %s (%s) carries G3 job %q, which asserts %s's work, but %s is not among its dependencies (%v) — the gate is red by construction: the task can never be accepted, however correct its work is, and the refusal will read like a real integration failure",
					id, task.Phase, jobName, needed, needed, task.Dependencies)
			}
		}
	}
	// A job that declares nothing is not exempt, it is unchecked: the loop
	// above has nothing to compare, so wiring it onto any task passes. Every
	// job must therefore either name the tasks whose work it asserts, or be
	// listed here with the reason it grades something other than the product
	// path a task builds. The list is exact in both directions, so a stale
	// entry cannot quietly exempt a future job that reuses the name.
	jobsThatGradeNoProductWork := map[string]string{
		"spec-validation":       "machine-readable specs and the task DAG — repository files",
		"task-state":            "DAG/state/test coverage — repository files",
		"go":                    "fmt/vet/staticcheck/unit — the Go source, no running product",
		"web":                   "typecheck/lint/unit/build — the frontend source",
		"python":                "lint/type/test — the scientific adapter source",
		"migration-integration": "migrations against a bare PostgreSQL — the schema, not the API",
		"acceptance":            "the gate machinery's own e2e — scratch repos and fakes, no product instance",
		"gitea-real-services":   "the Gitea INSTANCE's capabilities (provisioning, protection, webhooks) — the dev stack, not the task's API",
	}
	for name := range jobsThatGradeNoProductWork {
		if _, defined := spec.Jobs[name]; !defined {
			t.Errorf("jobsThatGradeNoProductWork names %q, which %s does not define — an exemption for a job that no longer exists, or a typo", name, DefaultGatesPath)
		}
	}
	for name, job := range spec.Jobs {
		why, exempt := jobsThatGradeNoProductWork[name]
		switch {
		case len(job.RequiresTasks) == 0 && !exempt:
			t.Errorf("G3 job %q declares no requires_tasks, so nothing checks that the tasks carrying it can satisfy it: it would run against a tree that does not serve what it asserts, and this test would pass it. Name the tasks whose work it asserts, or add it to jobsThatGradeNoProductWork with the reason it grades something other than the product path", name)
		case len(job.RequiresTasks) > 0 && exempt:
			t.Errorf("G3 job %q declares requires_tasks %v and is also listed in jobsThatGradeNoProductWork (%s) — one of the two is wrong", name, job.RequiresTasks, why)
		}
	}

	// The exemption set is pinned, not merely tolerated. A name in `unsatisfiable`
	// that is not pinned is the defect this test exists to catch, and a pinned
	// name that is no longer unsatisfiable means the knot moved: the recorded
	// reason is then about a task that can satisfy its job, and the pin has to
	// be updated deliberately rather than rot into a stale allowance.
	want := make([]string, 0, len(carriersTheChainTraps))
	for id := range carriersTheChainTraps {
		want = append(want, id)
	}
	sort.Strings(want)
	got := make([]string, 0, len(unsatisfiable))
	for id := range unsatisfiable {
		got = append(got, id)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tasks carrying a gate they cannot satisfy = %v, pinned as %v. A name in the first list that is not in the second is a wiring defect: give the task the edge to the work its job asserts. A name in the second that is not in the first means the knot was resolved — remove it from carriersTheChainTraps and record why (L1-20260913-19)", got, want)
	}
}
