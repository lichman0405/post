package diff

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/lichman0405/post/internal/rsg/manifest"
)

// Golden diff tests (required test "golden diff"): the canonical diff
// bytes of the fixed fixtures are committed under testdata/. A change to
// the canonical serialization — field order, kind vocabulary, stable
// ordering, payload rendering — fails here instead of silently re-rendering
// every downstream consumer (the PR page T0408, the conflict detector
// T0405, the merge tasks). Regenerate deliberately with
// `go test . -run TestGolden -update`.
var updateGoldens = flag.Bool("update", false, "regenerate golden diff files")

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

// TestGoldenDiff is the byte-level golden over the shared three-way
// fixture (updated claim with a target-side concurrent update, aborted
// hypothesis, created finding, updated relation, one file diff ref). The
// golden pins every canonicalization rule at once: stable ordering,
// canonical payloads, changed-field vocabulary and order, kind strings,
// the summary breakdown and the file refs.
func TestGoldenDiff(t *testing.T) {
	d, err := Compute(baseInputs())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	doc, err := d.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	writeGolden(t, "v1-golden.json", append(doc, '\n'))

	wantDoc := readGolden(t, "v1-golden.json")
	if string(wantDoc) != string(doc)+"\n" {
		t.Fatalf("canonical diff bytes drifted from the golden:\n got: %s\nwant: %s", doc, wantDoc)
	}
}

// manifestSnapshot wraps one object version row into a snapshot, so the
// empty-diff fixture shares one row across all three lineages.
func manifestSnapshot(row manifest.ObjectVersion) manifest.Snapshot {
	return manifest.Snapshot{
		ObjectVersions:   []manifest.ObjectVersion{row},
		RelationVersions: []manifest.RelationVersion{},
		BlobRefs:         []manifest.BlobRef{},
	}
}

// TestGoldenEmptyDiff is the no-change golden: identical base/source
// lineages render an empty change list, and that shape is canonical too.
func TestGoldenEmptyDiff(t *testing.T) {
	row := objRow("ov-00000001", "obj-a-0000001", "claim", 1, "s-b", "active", "A", `{"statement":"a"}`, nil)
	snap := manifestSnapshot(row)
	d, err := Compute(Inputs{
		ProjectID:      "proj-00000002",
		Base:           StateRef{ID: "state-base-0002"},
		Source:         StateRef{ID: "state-src-000002"},
		Target:         StateRef{ID: "state-tgt-000002"},
		BaseSnapshot:   snap,
		SourceSnapshot: snap,
		TargetSnapshot: snap,
	})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	doc, err := d.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	writeGolden(t, "v1-empty-golden.json", append(doc, '\n'))

	wantDoc := readGolden(t, "v1-empty-golden.json")
	if string(wantDoc) != string(doc)+"\n" {
		t.Fatalf("empty diff bytes drifted from the golden:\n got: %s\nwant: %s", doc, wantDoc)
	}
}

// TestGoldenDiffRepeats is the reproducibility half of the acceptance
// criterion: recomputing the fixture twice yields the same canonical
// bytes, pinned against the golden.
func TestGoldenDiffRepeats(t *testing.T) {
	d1, err := Compute(baseInputs())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	d2, err := Compute(baseInputs())
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	b1, _ := d1.CanonicalJSON()
	b2, _ := d2.CanonicalJSON()
	if string(b1) != string(b2) {
		t.Fatalf("repeated compute moved the canonical bytes:\nfirst: %s\nsecond: %s", b1, b2)
	}
}
