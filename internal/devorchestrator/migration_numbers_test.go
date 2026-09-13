package devorchestrator

import (
	"os"
	"path/filepath"
	"testing"
)

// Two P1 Workers ran in parallel and both created
// infra/migrations/00017_*.sql. Neither was careless: both followed the
// repository's own instruction against an index that had stopped five
// migrations earlier. A number is a shared resource across concurrent Workers,
// so it is allocated, not inferred.
func TestMigrationNumbersAreAllocatedNotInferred(t *testing.T) {
	repoRoot := t.TempDir()
	dir := filepath.Join(repoRoot, "infra", "migrations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"00016_auth.sql", "00019_project_provisioning.sql", "README.md", "migrations.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("-- x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Three tasks dispatched before ANY of their migrations exist must still
	// receive three different numbers.
	seen := map[int]string{}
	for _, taskID := range []string{"T0201", "T0301", "T0302"} {
		n, err := AllocateMigrationNumber(repoRoot, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if other, dup := seen[n]; dup {
			t.Fatalf("tasks %s and %s were both given migration number %05d — this is the collision that produced two 00017 files", other, taskID, n)
		}
		seen[n] = taskID
		if n <= 19 {
			t.Errorf("task %s got %05d, which is not past the highest migration on disk (00019)", taskID, n)
		}
	}

	// Idempotent: a rework or respawn keeps the number the task was contracted
	// with, so a Worker that already wrote its file is not asked to rename it.
	first, err := AllocateMigrationNumber(repoRoot, "T0201")
	if err != nil {
		t.Fatal(err)
	}
	again, err := AllocateMigrationNumber(repoRoot, "T0201")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Errorf("re-dispatch changed the task's migration number: %05d then %05d", first, again)
	}

	// A number freed by a cancelled task is not reused: another branch may
	// already have written that file.
	if _, err := AllocateMigrationNumber(repoRoot, "T0999"); err != nil {
		t.Fatal(err)
	}
	ledger, err := readMigrationNumbers(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	delete(ledger, "T0999")
	if err := writeMigrationNumbers(repoRoot, ledger); err != nil {
		t.Fatal(err)
	}
	after, err := AllocateMigrationNumber(repoRoot, "T0401")
	if err != nil {
		t.Fatal(err)
	}
	if after <= 19 {
		t.Errorf("a released number was reused (%05d) — the ledger must stay monotonic", after)
	}
}
