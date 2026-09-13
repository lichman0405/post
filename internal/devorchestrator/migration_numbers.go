package devorchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// Migration number allocation.
//
// Two Workers ran in parallel in P1 and both added
// infra/migrations/00017_*.sql. Each had followed the repository's own
// instruction ("add NNNNN_description.sql (next number)") against a README
// whose index had stopped at 00013, so neither was careless - the instruction
// was. Choosing a number is a shared-resource allocation, and anything shared
// across concurrent Workers has to be allocated by the Supervisor, not
// inferred by each of them from a document that is stale by construction.
//
// Allocation happens at dispatch: the number is reserved for the task, travels
// in its task package, and survives rework and respawn unchanged, so a task
// keeps the number it was contracted with.
const migrationNumbersFile = "migration-numbers.json"

var migrationFileRe = regexp.MustCompile(`^([0-9]{5})_.*\.sql$`)

// MigrationNumbersPath is the Supervisor-owned reservation ledger.
func MigrationNumbersPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".rddev", "runtime", migrationNumbersFile)
}

// highestMigrationOnDisk returns the highest numbered migration present, or 0.
func highestMigrationOnDisk(repoRoot string) int {
	entries, err := os.ReadDir(filepath.Join(repoRoot, "infra", "migrations"))
	if err != nil {
		return 0
	}
	highest := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		if n, err := strconv.Atoi(m[1]); err == nil && n > highest {
			highest = n
		}
	}
	return highest
}

func readMigrationNumbers(repoRoot string) (map[string]int, error) {
	data, err := os.ReadFile(MigrationNumbersPath(repoRoot))
	if os.IsNotExist(err) {
		return map[string]int{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the migration number ledger: %w", err)
	}
	out := map[string]int{}
	if len(data) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parsing the migration number ledger %s: %w", MigrationNumbersPath(repoRoot), err)
	}
	return out, nil
}

// AllocateMigrationNumber returns the migration number reserved for taskID,
// reserving one if this is the first time the task is dispatched.
//
// The next number is one past BOTH the highest migration on disk and the
// highest reservation ever made, so a number is never handed out twice even
// before any of the reserved migrations exist. Monotonic: a number freed by a
// cancelled task is not reused, because a released number may already be in
// someone's branch.
func AllocateMigrationNumber(repoRoot, taskID string) (int, error) {
	if taskID == "" {
		return 0, fmt.Errorf("allocating a migration number requires a task id")
	}
	ledger, err := readMigrationNumbers(repoRoot)
	if err != nil {
		return 0, err
	}
	if n, ok := ledger[taskID]; ok {
		return n, nil // idempotent: a rework keeps the number it was contracted with
	}
	highest := highestMigrationOnDisk(repoRoot)
	for _, n := range ledger {
		if n > highest {
			highest = n
		}
	}
	next := highest + 1
	ledger[taskID] = next
	if err := writeMigrationNumbers(repoRoot, ledger); err != nil {
		return 0, err
	}
	return next, nil
}

// writeMigrationNumbers persists the ledger atomically, so two concurrent
// spawns cannot interleave and hand out the same number.
func writeMigrationNumbers(repoRoot string, ledger map[string]int) error {
	path := MigrationNumbersPath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the migration ledger directory: %w", err)
	}
	keys := make([]string, 0, len(ledger))
	for k := range ledger {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]int, len(ledger))
	for _, k := range keys {
		ordered[k] = ledger[k]
	}
	return writeFileAtomic(path, marshalIndentBytes(ordered))
}
