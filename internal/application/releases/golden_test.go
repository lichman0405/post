package releases

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// Golden release manifest tests (required test "release golden"): the
// canonical document bytes and the manifest hash of the fixed fixtures
// are committed under testdata/. A change to the release manifest's
// canonical serialization — field order, key sorting, time rendering,
// digest spelling, the embedded state pin — fails here instead of
// silently re-hashing every stored release (docs/11 §1: the manifest is
// the immutable snapshot; its bytes are the identity). Regenerate
// deliberately with `go test . -run TestGolden -update`.
var updateGoldens = flag.Bool("update", false, "regenerate golden release manifest files")

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

// TestGoldenReleaseManifest is the byte-level golden: the populated
// fixture's canonical document and its manifest hash, both committed. The
// document golden pins every serialization rule at once — including the
// embedded state export bytes, the canonicalized policy documents, the
// schema pin order and the review record order; the hash golden pins the
// digest input, so any change to either needs a deliberate golden update.
func TestGoldenReleaseManifest(t *testing.T) {
	m := buildFixture(t)
	doc, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	writeGolden(t, "release-v1-golden.json", append(doc, '\n'))
	writeGolden(t, "release-v1-golden.manifest_hash", []byte(m.ManifestHash+"\n"))

	wantDoc := readGolden(t, "release-v1-golden.json")
	if string(wantDoc) != string(doc)+"\n" {
		t.Fatalf("canonical manifest bytes drifted from the golden:\n got: %s\nwant: %s", doc, wantDoc)
	}
	wantHash := readGolden(t, "release-v1-golden.manifest_hash")
	if string(wantHash) != m.ManifestHash+"\n" {
		t.Fatalf("manifest hash drifted from the golden:\n got: %s\nwant: %s", m.ManifestHash, wantHash)
	}
}

// TestGoldenMinimalReleaseManifest is the minimal-release golden: no
// policy pin, no schema pins, no review record. The bare document is
// canonical too — its bytes and hash must not move either (a release the
// gate refuses still has one exact rendering).
func TestGoldenMinimalReleaseManifest(t *testing.T) {
	m, err := Build(ManifestInput{
		ProjectID:   "proj-00000001",
		StateID:     "state-00000001",
		Version:     "v0.0.1",
		GeneratedAt: fixedTime(18, 0),
		State:       fixtureStateManifest(t),
		Policy:      nil,
		Schemas:     []SchemaPin{},
		Reviews:     []ReviewRecord{},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	doc, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	writeGolden(t, "release-v1-minimal-golden.json", append(doc, '\n'))
	writeGolden(t, "release-v1-minimal-golden.manifest_hash", []byte(m.ManifestHash+"\n"))

	wantDoc := readGolden(t, "release-v1-minimal-golden.json")
	if string(wantDoc) != string(doc)+"\n" {
		t.Fatalf("minimal manifest bytes drifted from the golden:\n got: %s\nwant: %s", doc, wantDoc)
	}
	wantHash := readGolden(t, "release-v1-minimal-golden.manifest_hash")
	if string(wantHash) != m.ManifestHash+"\n" {
		t.Fatalf("minimal manifest hash drifted from the golden:\n got: %s\nwant: %s", m.ManifestHash, wantHash)
	}
}
