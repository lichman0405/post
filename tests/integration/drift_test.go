package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSQLCGenerationDrift runs the same drift check as
// tests/integration/check-sqlc-drift.sh (single implementation, invoked
// here so `go test ./...` covers it in CI). It skips only when the sqlc
// binary is genuinely unavailable; in that case run the script directly or
// install sqlc: go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1.
func TestSQLCGenerationDrift(t *testing.T) {
	sqlcBin := os.Getenv("SQLC_BIN")
	if sqlcBin == "" {
		p, err := exec.LookPath("sqlc")
		if err != nil {
			t.Skipf("sqlc not installed (%v) — drift check requires the generator; "+
				"install with: go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1", err)
		}
		sqlcBin = p
	}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	cmd := exec.Command(filepath.Join(root, "tests", "integration", "check-sqlc-drift.sh"))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "SQLC_BIN="+sqlcBin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sqlc drift check failed:\n%s", out)
	}
	t.Logf("%s", out)
}
