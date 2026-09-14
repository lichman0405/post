package externalref

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestBuildSnapshotDeterministic(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 14, 10, 30, 0, 0, time.UTC) }
	meta := UpstreamMetadata{
		UpstreamVersion: "v3",
		Metadata:        json.RawMessage(`{"title":"x","year":2026}`),
	}
	a, err := BuildSnapshot("ref-1", meta, now)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	b, err := BuildSnapshot("ref-1", meta, now)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	// Same input, same clock → byte-identical snapshot (the builder is a
	// pure function of its inputs; nothing random may leak in).
	if a.SnapshotHash != b.SnapshotHash || string(a.Metadata) != string(b.Metadata) {
		t.Fatalf("BuildSnapshot is not deterministic:\n%+v\n%+v", a, b)
	}
	if !a.AccessedAt.Equal(b.AccessedAt) {
		t.Fatalf("accessed_at differs across identical builds: %v vs %v", a.AccessedAt, b.AccessedAt)
	}
	// The clock is stamped, never caller-supplied, and normalized to UTC.
	if !a.AccessedAt.Equal(now()) || a.AccessedAt.Location() != time.UTC {
		t.Fatalf("AccessedAt = %v, want the injected clock value in UTC", a.AccessedAt)
	}
	// The version marker travels with the snapshot.
	if a.UpstreamVersion != "v3" {
		t.Fatalf("UpstreamVersion = %q, want %q", a.UpstreamVersion, "v3")
	}
	// The hash is sha256-hex of the canonical bytes.
	if a.SnapshotHash != digest(a.Metadata) {
		t.Fatalf("SnapshotHash = %q, want digest of the stored bytes %q", a.SnapshotHash, digest(a.Metadata))
	}
}

// TestBuildSnapshotCanonicalizes proves that two byte spellings of one
// document (a hand-written JSON object with odd whitespace/order vs a
// Go-marshaled map) produce byte-identical stored metadata: the builder
// re-canonicalizes, so equal documents never split into "changed" by
// spelling.
func TestBuildSnapshotCanonicalizes(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC) }
	a, err := BuildSnapshot("ref-1", UpstreamMetadata{Metadata: json.RawMessage(`{ "b" : 2, "a" : 1 }`)}, now)
	if err != nil {
		t.Fatalf("BuildSnapshot(odd spelling): %v", err)
	}
	doc := map[string]any{"a": float64(1), "b": float64(2)}
	raw, _ := json.Marshal(doc)
	b, err := BuildSnapshot("ref-1", UpstreamMetadata{Metadata: raw}, now)
	if err != nil {
		t.Fatalf("BuildSnapshot(go spelling): %v", err)
	}
	if string(a.Metadata) != string(b.Metadata) || a.SnapshotHash != b.SnapshotHash {
		t.Fatalf("equal documents built unequal snapshots:\n%q\n%q", a.Metadata, b.Metadata)
	}
}

func TestBuildSnapshotRejectsNonDocument(t *testing.T) {
	for _, in := range []string{`[1,2]`, `"scalar"`, `42`, `null`, `not json`} {
		_, err := BuildSnapshot("ref-1", UpstreamMetadata{Metadata: json.RawMessage(in)}, nil)
		if !errors.Is(err, ErrInvalidUpstream) {
			t.Errorf("BuildSnapshot(%s) error = %v, want ErrInvalidUpstream (a snapshot pins a document, never a scalar)", in, err)
		}
	}
}

// TestSameContentAcrossByteSpellings is the change-detection acceptance
// pin: a head read back from the database (jsonb-canonical text — spaces
// after colons, length-then-bytewise key order) and a freshly built
// snapshot (Go-canonical compact JSON) compare EQUAL whenever the
// documents are equal, so byte spellings never cause a false "changed".
func TestSameContentAcrossByteSpellings(t *testing.T) {
	// jsonb's canonical text spelling of the same document.
	jsonbStyle := `{"a": 1, "title": "x"}`
	// The builder's canonical spelling.
	doc := map[string]any{"a": float64(1), "title": "x"}
	goStyle, _ := json.Marshal(doc)

	a := Snapshot{Metadata: json.RawMessage(jsonbStyle)}
	b := Snapshot{Metadata: json.RawMessage(goStyle)}
	if !SameContent(a, b) {
		t.Fatalf("SameContent(%q, %q) = false, want true (equal documents, different byte spellings)", jsonbStyle, goStyle)
	}
	if !SameContent(b, a) {
		t.Fatal("SameContent must be symmetric")
	}

	// One changed field IS a change — including the same keys in a
	// different order with a different value set.
	c := Snapshot{Metadata: json.RawMessage(`{"a": 2, "title": "x"}`)}
	if SameContent(a, c) {
		t.Fatalf("SameContent(%q, %q) = true, want false", a.Metadata, c.Metadata)
	}
	// Key order differences never matter.
	d := Snapshot{Metadata: json.RawMessage(`{"title": "x", "a": 1}`)}
	if !SameContent(a, d) {
		t.Fatalf("SameContent(%q, %q) = false, want true (key order is not content)", a.Metadata, d.Metadata)
	}
	// Unparseable metadata is never "same".
	if SameContent(Snapshot{Metadata: json.RawMessage(`{]`)}, a) {
		t.Fatal("SameContent accepted unparseable metadata")
	}
}

// TestSameContentIncludesUpstreamVersion pins the Fetcher contract: the
// compared unit is the (Metadata, UpstreamVersion) PAIR, so a source
// whose document is unchanged but whose version marker moved counts as
// changed — the refresh appends a new snapshot instead of reporting the
// head current, and an adapter reporting the version separately from the
// document still moves the log.
func TestSameContentIncludesUpstreamVersion(t *testing.T) {
	doc := json.RawMessage(`{"a": 1, "title": "x"}`)
	head := Snapshot{Metadata: doc, UpstreamVersion: "v1"}

	// Same document (different byte spelling), moved version: changed.
	fresh := Snapshot{Metadata: json.RawMessage(`{"title": "x", "a": 1}`), UpstreamVersion: "v2"}
	if SameContent(head, fresh) {
		t.Fatal("SameContent = true for equal documents with different UpstreamVersion; want false (the version marker is part of upstream state)")
	}
	// Same document, same version: unchanged, whatever the spelling.
	if !SameContent(head, Snapshot{Metadata: json.RawMessage(`{"title": "x", "a": 1}`), UpstreamVersion: "v1"}) {
		t.Fatal("SameContent = false for the identical pair")
	}
	// A reported version differs from none: the upstream stopped or
	// started reporting it, and the log must record that.
	if SameContent(Snapshot{Metadata: doc}, Snapshot{Metadata: doc, UpstreamVersion: "v1"}) {
		t.Fatal("SameContent = true for empty vs reported UpstreamVersion; want false")
	}
}
