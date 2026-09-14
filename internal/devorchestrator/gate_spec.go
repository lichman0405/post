package devorchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// The four-gate loop (T0012, docs/67) is executable, not procedural: every
// gate is defined here (specs/orchestrator/gates.json, the same file the
// executor reads — no embedded copy to drift), every gate run records
// commands/exit/output to disk, and the merge gate refuses while any
// required CI job is red or missing. G2 runs CI's EXACT steps: the jobs
// of .github/workflows/ci.yml with their steps verbatim, and
// TestGatesSpecSyncsWithCIWorkflow fails the build when the two drift apart.
// The earlier failures this fixes: a Supervisor merged a red PR because G2
// ran a similar-looking subset, and reported green from `make check`, which
// does not include staticcheck — the gate spec makes both impossible.

// DefaultGatesPath is the gate spec read by the executor (repo-relative).
const DefaultGatesPath = "specs/orchestrator/gates.json"

// GateStep is one command of a CI job, verbatim from ci.yml.
type GateStep struct {
	Run string            `json:"run"`
	Env map[string]string `json:"env,omitempty"`
}

// GateJob is one named CI job with its exact step list.
type GateJob struct {
	Name  string     `json:"name"`
	Steps []GateStep `json:"steps"`
	// RequiresTasks names the tasks whose work the job asserts: the job runs
	// against the task's tree, so a task may only carry this job when every
	// named task is in its dependency closure (or is the task itself). It must
	// name the tasks whose deliverables the job's assertions are about — an
	// incomplete list is a check that passes on a tree which does not serve
	// what the job asserts. A job that grades the repository or the dev stack
	// rather than the product path (the Gitea instance script checks the dev
	// stack's capabilities, not the task's own work) leaves it empty and is
	// listed, with that reason, in TestEveryG3JobIsSatisfiableByTheTaskThatCarriesIt:
	// "declares nothing" is not itself an exemption, or a new job could run a
	// product script without ever being checked.
	//
	// This exists because a chain gate wired onto tasks that cannot satisfy it
	// is red by construction: the task stays un-acceptable however good its
	// work is, and the failure is indistinguishable from a real one.
	//
	// It is meaningful only for the jobs that can be wired onto a task. The
	// required jobs (spec.RequiredJobs) run on every push against the
	// repository, never against a task's tree, so they leave it empty by
	// definition and the test asserts that they do.
	RequiresTasks []string `json:"requires_tasks,omitempty"`
}

// GateDef describes one of G1..G4.
type GateDef struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	RunsJobs    []string `json:"runs_jobs"`
	AssertsJobs []string `json:"asserts_jobs"`
}

// TaskGateOverride allows a task to define G3 integration jobs (and opt out
// of the review requirement for merge). Everything else is global: G2 is
// always CI's exact jobs — a per-task narrowing would reintroduce the
// subset-G2 defect this task exists to kill.
type TaskGateOverride struct {
	G3Jobs              []string `json:"g3_jobs"`
	ReviewRequiredMerge *bool    `json:"review_required_for_merge,omitempty"`
}

// GateSpec is the loaded gate definition file.
type GateSpec struct {
	Version      int                `json:"version"`
	RequiredJobs []string           `json:"required_jobs"`
	Gates        map[string]GateDef `json:"gates"`
	Jobs         map[string]GateJob `json:"jobs"`
	Review       struct {
		RequiredForMerge bool `json:"required_for_merge"`
	} `json:"review"`
	TaskOverrides map[string]TaskGateOverride `json:"task_overrides"`
}

// LoadGateSpec reads and validates the gate spec at path (absolute or
// repo-relative).
func LoadGateSpec(path string) (*GateSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading gate spec %s: %w", path, err)
	}
	var g GateSpec
	if err := json.Unmarshal(data, &g); err != nil {
		return nil, fmt.Errorf("parsing gate spec %s: %w", path, err)
	}
	if g.Version != 1 {
		return nil, fmt.Errorf("gate spec %s has version %d, want 1", path, g.Version)
	}
	for _, name := range []string{"G1", "G2", "G3", "G4"} {
		if _, ok := g.Gates[name]; !ok {
			return nil, fmt.Errorf("gate spec %s is missing gate %s", path, name)
		}
	}
	if len(g.RequiredJobs) == 0 {
		return nil, fmt.Errorf("gate spec %s declares no required_jobs — the merge gate would assert nothing", path)
	}
	for _, job := range g.RequiredJobs {
		j, ok := g.Jobs[job]
		if !ok {
			return nil, fmt.Errorf("gate spec %s: required job %q has no definition", path, job)
		}
		if len(j.Steps) == 0 {
			return nil, fmt.Errorf("gate spec %s: job %q has no steps", path, job)
		}
	}
	// Every gate must be internally consistent: runs_jobs and asserts_jobs
	// name defined jobs.
	for gateName, def := range g.Gates {
		for _, job := range def.RunsJobs {
			if _, ok := g.Jobs[job]; !ok {
				return nil, fmt.Errorf("gate spec %s: %s runs_jobs names undefined job %q", path, gateName, job)
			}
		}
		for _, job := range def.AssertsJobs {
			if _, ok := g.Jobs[job]; !ok {
				return nil, fmt.Errorf("gate spec %s: %s asserts_jobs names undefined job %q", path, gateName, job)
			}
		}
	}
	return &g, nil
}

// gateSpecAt resolves path against repoRoot when relative (rddev is run from
// the repo root).
func gateSpecAt(repoRoot, path string) (*GateSpec, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(repoRoot, path)
	}
	return LoadGateSpec(path)
}

// GateSpecPath resolves a possibly-relative gate spec path against repoRoot
// (the CLI and e2e harness need the same resolution as the engine).
func GateSpecPath(repoRoot, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(repoRoot, path)
}

// JobsForGate returns the job names one gate executes: G1 runs none (the
// collect checks are G1), G2 runs its runs_jobs, G3 runs the task override
// (empty = not required), G4 asserts its asserts_jobs against the latest G2
// record rather than running.
func (g *GateSpec) JobsForGate(gate string, taskID string) ([]string, error) {
	def, ok := g.Gates[gate]
	if !ok {
		return nil, fmt.Errorf("unknown gate %q (valid: G1, G2, G3, G4)", gate)
	}
	switch gate {
	case "G2":
		return def.RunsJobs, nil
	case "G3":
		ov := g.TaskOverrides[taskID]
		return ov.G3Jobs, nil
	case "G1", "G4":
		return nil, nil // G1 = collect checks; G4 = assert against G2 records
	default:
		return def.RunsJobs, nil
	}
}

// ReviewRequiredForMerge reports whether pr merge demands an approving
// review verdict for the task (global default, per-task override).
func (g *GateSpec) ReviewRequiredForMerge(taskID string) bool {
	if ov, ok := g.TaskOverrides[taskID]; ok && ov.ReviewRequiredMerge != nil {
		return *ov.ReviewRequiredMerge
	}
	return g.Review.RequiredForMerge
}

// RequiredJobSet returns the required CI job names in canonical order.
func (g *GateSpec) RequiredJobSet() []string {
	out := make([]string, len(g.RequiredJobs))
	copy(out, g.RequiredJobs)
	sort.Strings(out)
	return out
}
