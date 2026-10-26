package devorchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

// The V2 projection is carried on the DAG entry, not in a side file: a task
// whose v2_required flag rddev cannot read is a task the final gate cannot
// count. These two cases pin the read and the V1/V2 separation.
func TestLoadDAGReadsV2Projection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	body := `{"tasks":[{"id":"T1301","phase":"P14","title":"v2","v1_required":false,` +
		`"v2_required":true,"web_package":"WEB-00","dependencies":[]}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	dag, err := LoadDAG(path)
	if err != nil {
		t.Fatalf("LoadDAG: %v", err)
	}
	spec := dag.Get("T1301")
	if spec == nil {
		t.Fatal("T1301 missing from the loaded DAG")
	}
	if !spec.V2Required {
		t.Fatal("v2_required=true was not read into the task spec")
	}
	if spec.WebPackage != "WEB-00" {
		t.Fatalf("web_package = %q, want WEB-00", spec.WebPackage)
	}
	if spec.V1Required {
		t.Fatal("a V2 task was read as v1_required; the V1 denominator must stay untouched")
	}
}

func TestLoadDAGDefaultsV2ProjectionToFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	body := `{"tasks":[{"id":"T0001","phase":"P0","title":"v1","v1_required":true,"dependencies":[]}]}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	dag, err := LoadDAG(path)
	if err != nil {
		t.Fatalf("LoadDAG: %v", err)
	}
	spec := dag.Get("T0001")
	if spec == nil {
		t.Fatal("T0001 missing from the loaded DAG")
	}
	if spec.V2Required || spec.WebPackage != "" {
		t.Fatalf("a V1 task gained V2 projection fields: %+v", spec)
	}
}
