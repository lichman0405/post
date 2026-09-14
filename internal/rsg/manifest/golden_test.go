package manifest

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/lichman0405/post/internal/domain"
)

// Golden manifest tests (required test "golden manifest tests"): the
// canonical export bytes and the state hash of the fixed fixtures are
// committed under testdata/. A change to the canonical serialization —
// field order, key sorting, time rendering, digest spelling — fails here
// instead of silently re-hashing every downstream consumer (T0309
// reconciliation, T0605 release pinning). Regenerate deliberately with
// `go test . -run TestGolden -update`.
var updateGoldens = flag.Bool("update", false, "regenerate golden manifest files")

// goldenPath resolves a golden file under testdata/.
func goldenPath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", name)
}

// readGolden returns the committed golden bytes, or fails the test.
func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(goldenPath(t, name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return b
}

// writeGolden records the bytes as the golden (only with -update).
func writeGolden(t *testing.T, name string, b []byte) {
	t.Helper()
	if !*updateGoldens {
		return
	}
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	if err := os.WriteFile(goldenPath(t, name), b, 0o644); err != nil {
		t.Fatalf("write golden %s: %v", name, err)
	}
}

// TestGoldenManifest is the byte-level golden: the populated fixture's
// canonical document and its state hash, both committed. The document
// golden pins every serialization rule at once; the hash golden pins the
// digest input, so any change to either needs a deliberate golden update.
func TestGoldenManifest(t *testing.T) {
	m := buildFixture(t)
	doc, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	writeGolden(t, "v1-golden.json", append(doc, '\n'))
	writeGolden(t, "v1-golden.state_hash", []byte(m.StateHash+"\n"))

	wantDoc := readGolden(t, "v1-golden.json")
	if string(wantDoc) != string(doc)+"\n" {
		t.Fatalf("canonical manifest bytes drifted from the golden:\n got: %s\nwant: %s", doc, wantDoc)
	}
	wantHash := readGolden(t, "v1-golden.state_hash")
	if string(wantHash) != m.StateHash+"\n" {
		t.Fatalf("state hash drifted from the golden:\n got: %s\nwant: %s", m.StateHash, wantHash)
	}
}

// TestGoldenEmptyManifest is the genesis-state golden: the empty root's
// manifest is canonical too, and its hash must not move between exports.
func TestGoldenEmptyManifest(t *testing.T) {
	state := domain.ProjectState{
		ID:              "state-00000000",
		ProjectID:       "proj-00000000",
		ManifestVersion: FormatV1,
	}
	m, err := Build(state, Snapshot{}, fixedTime(9, 0))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	doc, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	writeGolden(t, "v1-empty-golden.json", append(doc, '\n'))
	writeGolden(t, "v1-empty-golden.state_hash", []byte(m.StateHash+"\n"))

	wantDoc := readGolden(t, "v1-empty-golden.json")
	if string(wantDoc) != string(doc)+"\n" {
		t.Fatalf("empty manifest bytes drifted from the golden:\n got: %s\nwant: %s", doc, wantDoc)
	}
	wantHash := readGolden(t, "v1-empty-golden.state_hash")
	if string(wantHash) != m.StateHash+"\n" {
		t.Fatalf("empty state hash drifted from the golden:\n got: %s\nwant: %s", m.StateHash, wantHash)
	}
}
