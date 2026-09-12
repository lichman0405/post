package devorchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// TaskSpec is one task of the DAG in tasks/tasks.json. Only the fields rddev
// consumes are modeled; the file is never rewritten by rddev.
type TaskSpec struct {
	ID                 string   `json:"id"`
	Phase              string   `json:"phase"`
	PhaseName          string   `json:"phase_name"`
	Title              string   `json:"title"`
	V1Required         bool     `json:"v1_required"`
	Dependencies       []string `json:"dependencies"`
	Requirements       []string `json:"requirements"`
	Deliverables       []string `json:"deliverables"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Tests              []string `json:"tests"`
	AllowedScope       []string `json:"allowed_scope"`
	ForbiddenScope     []string `json:"forbidden_scope"`
	DecisionLevelMax   string   `json:"decision_level_max"`
	RelevantSpecs      []string `json:"relevant_specs"`
}

// DAG is the task dependency graph.
type DAG struct {
	Tasks []TaskSpec `json:"tasks"`
	byID  map[string]*TaskSpec
}

// LoadDAG reads and validates the task DAG at path.
func LoadDAG(path string) (*DAG, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading task DAG %s: %w", path, err)
	}
	var raw struct {
		Tasks []TaskSpec `json:"tasks"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing task DAG %s: %w", path, err)
	}
	d := &DAG{Tasks: raw.Tasks, byID: make(map[string]*TaskSpec, len(raw.Tasks))}
	for i := range d.Tasks {
		t := &d.Tasks[i]
		if t.ID == "" {
			return nil, fmt.Errorf("parsing task DAG %s: task at index %d has no id", path, i)
		}
		if _, dup := d.byID[t.ID]; dup {
			return nil, fmt.Errorf("parsing task DAG %s: duplicate task id %s", path, t.ID)
		}
		if t.Dependencies == nil {
			t.Dependencies = []string{} // JSON outputs never emit null for dependencies
		}
		d.byID[t.ID] = t
	}
	for _, t := range d.Tasks {
		for _, dep := range t.Dependencies {
			if _, ok := d.byID[dep]; !ok {
				return nil, fmt.Errorf("parsing task DAG %s: task %s depends on unknown task %s", path, t.ID, dep)
			}
		}
	}
	return d, nil
}

// Get returns the task with the given id, or nil.
func (d *DAG) Get(id string) *TaskSpec { return d.byID[id] }

// IDs returns all task ids sorted (deterministic order).
func (d *DAG) IDs() []string {
	ids := make([]string, 0, len(d.byID))
	for id := range d.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
