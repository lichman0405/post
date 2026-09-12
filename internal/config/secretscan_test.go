package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScanDetectsPlantedSecrets is the demonstrated FAILING mode of the
// detector: planted secret-shaped values in example-file content must
// produce findings. A scanner that has only ever been observed passing is
// not evidence; this test is the observation that it fails.
func TestScanDetectsPlantedSecrets(t *testing.T) {
	content := `# a broken example file
POST_ENV=dev
POST_DB_PASSWORD=supersecretplantedpw
POST_DB_PASSWORD=postgres_dev_pw
POST_BLOB_ENDPOINT=https://minio:pw@127.0.0.1:9000
POST_GITEA_TOKEN=''
`
	findings := ScanExampleContent(content, "planted.env.example")
	if len(findings) == 0 {
		t.Fatal("scanner produced no findings for planted secrets — detector is dead")
	}
	want := []string{
		"secret-shaped value for POST_DB_PASSWORD",
		"real dev credential (deny-list)",
		"credential-bearing URL",
	}
	for _, w := range want {
		found := false
		for _, f := range findings {
			if strings.Contains(f.What, w) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing finding containing %q; got %v", w, findings)
		}
	}
	// The secret value itself must never appear in a finding.
	for _, f := range findings {
		if strings.Contains(f.String(), "supersecretplantedpw") ||
			strings.Contains(f.String(), "postgres_dev_pw") {
			t.Errorf("finding leaks the secret value: %s", f)
		}
	}
}

func TestScanPassesCleanExampleContent(t *testing.T) {
	content := `# POST environment reference — placeholders only, no real values.
POST_ENV=dev
POST_DB_PASSWORD=change-me
POST_BLOB_ENDPOINT=http://127.0.0.1:9000
POST_DB_SSLMODE=disable
POST_GITEA_TOKEN=<token>
`
	if findings := ScanExampleContent(content, "clean.env.example"); len(findings) != 0 {
		t.Errorf("clean content produced findings: %v", findings)
	}
}

// TestScanRepoExampleFilesFailsOnPlantedFile is the end-to-end failing mode
// of the repository gate: a planted .env.example with a secret must make the
// sweep fail (here, produce findings). The same sweep over the real
// repository must come back clean — see TestRepoExampleFilesAreSecretFree.
func TestScanRepoExampleFilesFailsOnPlantedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env.example"),
		[]byte("POST_GITEA_TOKEN=ghp_plantedtoken1234\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := ScanRepoExampleFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 {
		t.Fatal("sweep did not flag the planted secret — the gate would be vacuous")
	}
	if findings[0].File != filepath.Join(dir, ".env.example") {
		t.Errorf("finding should name the file: %+v", findings[0])
	}
}

// TestRepoExampleFilesAreSecretFree is the live repository gate: every
// committed *.env.example in the repository must be free of secret-shaped
// values. This runs on every `go test ./...` and `make check`.
func TestRepoExampleFilesAreSecretFree(t *testing.T) {
	root := repoRoot(t)
	findings, err := ScanRepoExampleFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		var b strings.Builder
		for _, f := range findings {
			b.WriteString("\n  ")
			b.WriteString(f.String())
		}
		t.Fatalf("secret-shaped values in committed example files:%s", b.String())
	}
}

// repoRoot walks up from the package directory to the repository root
// (the directory containing go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root (go.mod) not found")
		}
		dir = parent
	}
}
