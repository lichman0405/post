package releases

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/rsg/manifest"
)

// TestBuildDeterministic renders the same release twice at different
// render times: the hash and every byte except generated_at must be
// identical (acceptance: manifest deterministic).
func TestBuildDeterministic(t *testing.T) {
	in := fixtureInput(t)
	m1, err := Build(withGeneratedAt(in, fixedTime(18, 0)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	m2, err := Build(withGeneratedAt(in, fixedTime(20, 30)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if m1.ManifestHash != m2.ManifestHash {
		t.Fatalf("hash moved between renders of the same release: %s != %s", m1.ManifestHash, m2.ManifestHash)
	}
	doc1, err := m1.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	doc2, err := m2.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	var v1, v2 map[string]any
	if err := json.Unmarshal(doc1, &v1); err != nil {
		t.Fatalf("parse document: %v", err)
	}
	if err := json.Unmarshal(doc2, &v2); err != nil {
		t.Fatalf("parse document: %v", err)
	}
	v1["generated_at"] = nil
	v2["generated_at"] = nil
	if !reflect.DeepEqual(v1, v2) {
		t.Fatalf("document bytes drifted beyond generated_at:\nfirst:  %s\nsecond: %s", doc1, doc2)
	}
}

// fixtureInput is the populated fixture as a ManifestInput value.
func fixtureInput(t *testing.T) ManifestInput {
	t.Helper()
	return ManifestInput{
		ProjectID: "proj-00000001",
		StateID:   "state-00000001",
		Version:   "v1.0.0",
		State:     fixtureStateManifest(t),
		Policy:    fixturePolicyPin(),
		Schemas:   fixtureSchemaPins(),
		Reviews:   fixtureReviews(),
	}
}

// withGeneratedAt pins the render time of an input.
func withGeneratedAt(in ManifestInput, at time.Time) ManifestInput {
	in.GeneratedAt = at
	return in
}

// TestBuildCanonicalizesPinDocuments re-renders the pinned policy
// documents in canonical form: sorted keys, compact — the same rule the
// state manifest applies to payloads, so two spellings of one document
// pin identically.
func TestBuildCanonicalizesPinDocuments(t *testing.T) {
	m := buildFixture(t)
	doc, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if strings.Contains(string(doc), `{ "release_min_reviewers"`) || strings.Contains(string(doc), `"release_min_reviewers" : 2`) {
		t.Fatalf("policy document not canonicalized: %s", doc)
	}
	// The org policy's keys arrive shuffled; the canonical form sorts
	// them.
	var parsed struct {
		Policy struct {
			Organization struct {
				Document map[string]any `json:"document"`
			} `json:"organization"`
		} `json:"policy"`
	}
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("parse document: %v", err)
	}
	docOrg := parsed.Policy.Organization.Document
	if docOrg["main_protected"] != true {
		t.Fatalf("org policy document lost a rule: %v", docOrg)
	}
	if docOrg["release_min_reviewers"] != float64(2) {
		t.Fatalf("org policy document lost a rule: %v", docOrg)
	}
}

// TestBuildSortsSchemasAndReviews renders the arrays in canonical order
// regardless of the input order.
func TestBuildSortsSchemasAndReviews(t *testing.T) {
	in := fixtureInput(t)
	// Shuffle both arrays: the render must restore the canonical order.
	in.Schemas = []SchemaPin{in.Schemas[1], in.Schemas[0]}
	in.Reviews = []ReviewRecord{in.Reviews[1], in.Reviews[0]}
	m, err := Build(withGeneratedAt(in, fixedTime(18, 0)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if m.Schemas[0].ID >= m.Schemas[1].ID {
		t.Fatalf("schemas not sorted by (id, version): %+v", m.Schemas)
	}
	if m.Reviews[0].PullRequestNumber > m.Reviews[1].PullRequestNumber {
		t.Fatalf("review records not sorted by pull request number: %+v", m.Reviews)
	}
	// The reviews of the second fixture PR arrive newest-first; the
	// render orders them by (created_at, id).
	rec := m.Reviews[1]
	if rec.Reviews[0].ID != "rev-00000002" || rec.Reviews[1].ID != "rev-00000003" {
		t.Fatalf("reviews not sorted by (created_at, id): %+v", rec.Reviews)
	}
}

// TestBuildNormalizesTimestamps renders every timestamp in UTC.
func TestBuildNormalizesTimestamps(t *testing.T) {
	m := buildFixture(t)
	if m.GeneratedAt.Location() != time.UTC {
		t.Fatalf("generated_at not UTC: %v", m.GeneratedAt)
	}
	for _, pin := range []*PinnedPolicyVersion{m.Policy.Organization, m.Policy.Project} {
		if pin.CreatedAt.Location() != time.UTC {
			t.Fatalf("policy created_at not UTC: %v", pin.CreatedAt)
		}
	}
	for _, rec := range m.Reviews {
		for _, r := range rec.Reviews {
			if r.CreatedAt.Location() != time.UTC {
				t.Fatalf("review created_at not UTC: %v", r.CreatedAt)
			}
		}
	}
}

// TestContentMirrorsDocument guards the content struct against drift: the
// hash input must be exactly the document minus generated_at and
// manifest_hash.
func TestContentMirrorsDocument(t *testing.T) {
	m := buildFixture(t)
	doc, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	contentBytes, err := m.ContentCanonicalJSON()
	if err != nil {
		t.Fatalf("ContentCanonicalJSON: %v", err)
	}
	var docMap, contentMap map[string]any
	if err := json.Unmarshal(doc, &docMap); err != nil {
		t.Fatalf("parse document: %v", err)
	}
	if err := json.Unmarshal(contentBytes, &contentMap); err != nil {
		t.Fatalf("parse content: %v", err)
	}
	delete(docMap, "generated_at")
	delete(docMap, "manifest_hash")
	if !reflect.DeepEqual(docMap, contentMap) {
		t.Fatalf("document and hash content diverged:\ndocument: %v\ncontent:  %v", docMap, contentMap)
	}
}

// TestVerifyHashAndTamper checks the self-verification: the built
// manifest verifies, and any semantic change to it fails the check.
func TestVerifyHashAndTamper(t *testing.T) {
	m := buildFixture(t)
	if !m.VerifyHash() {
		t.Fatalf("built manifest does not verify")
	}
	m.Version = "v2.0.0"
	if m.VerifyHash() {
		t.Fatalf("tampered manifest still verifies")
	}
}

// TestBuildVerifiesEmbeddedStateHash refuses a state export whose pin
// would not close: the embedded state hash must re-derive from the
// embedded content, or the release records a broken chain.
func TestBuildVerifiesEmbeddedStateHash(t *testing.T) {
	in := fixtureInput(t)
	state := in.State
	state.StateHash = "sha256:" + strings.Repeat("0", 64)
	in.State = state
	if _, err := Build(withGeneratedAt(in, fixedTime(18, 0))); err == nil {
		t.Fatalf("expected an error for a state export that does not verify")
	}
}

// TestBuildRequiresStateExport refuses an input without the state export.
func TestBuildRequiresStateExport(t *testing.T) {
	in := fixtureInput(t)
	in.State = nil
	if _, err := Build(withGeneratedAt(in, fixedTime(18, 0))); err == nil {
		t.Fatalf("expected an error for a nil state export")
	}
}

// TestBuildEmptyArraysRenderAsArrays: the minimal release renders its
// empty arrays as [] (never null) so the document shape is stable.
func TestBuildEmptyArraysRenderAsArrays(t *testing.T) {
	m, err := Build(withGeneratedAt(ManifestInput{
		ProjectID: "proj-00000001",
		StateID:   "state-00000001",
		Version:   "v0.0.1",
		State:     fixtureStateManifest(t),
		Schemas:   []SchemaPin{},
		Reviews:   []ReviewRecord{},
	}, fixedTime(18, 0)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	doc, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	for _, field := range []string{`"schemas":[]`, `"reviews":[]`} {
		if !strings.Contains(string(doc), field) {
			t.Fatalf("empty array rendered wrong (want %s): %s", field, doc)
		}
	}
}

// TestBuildSemanticChangesMoveHash: every semantic input of the release —
// the state export, the policy documents, the schema pins, the review
// record, the version — is part of the hash. The render-time field is the
// only one that is not.
func TestBuildSemanticChangesMoveHash(t *testing.T) {
	base, err := Build(withGeneratedAt(fixtureInput(t), fixedTime(18, 0)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// A second, genuinely different state export: one object version's
	// title changed — a semantic change of the snapshot.
	snap2 := fixtureStateSnapshot()
	snap2.ObjectVersions[0].Title = "E1-renamed"
	state2, err := manifest.Build(fixtureState(), snap2, fixedTime(14, 0))
	if err != nil {
		t.Fatalf("manifest.Build: %v", err)
	}
	cases := []struct {
		name string
		mut  func(in *ManifestInput)
	}{
		{"version", func(in *ManifestInput) { in.Version = "v1.0.1" }},
		{"project", func(in *ManifestInput) { in.ProjectID = "proj-00000002" }},
		{"state content", func(in *ManifestInput) { in.State = state2 }},
		{"org policy rule", func(in *ManifestInput) {
			org := *in.Policy.Organization
			org.Document = json.RawMessage(`{"main_protected":false}`)
			in.Policy.Organization = &org
		}},
		{"policy pin dropped", func(in *ManifestInput) { in.Policy = nil }},
		{"schema pin content hash", func(in *ManifestInput) {
			in.Schemas = append([]SchemaPin(nil), in.Schemas...)
			in.Schemas[0].ContentHash = "ffff"
		}},
		{"schema pin added", func(in *ManifestInput) {
			in.Schemas = append(append([]SchemaPin(nil), in.Schemas...), SchemaPin{ID: "https://open-rd.example/schemas/dataset.schema.json", Version: "1", ContentHash: "dddd"})
		}},
		{"review decision", func(in *ManifestInput) {
			in.Reviews = append([]ReviewRecord(nil), in.Reviews...)
			rs := append([]Review(nil), in.Reviews[0].Reviews...)
			rs[0].Decision = "changes_requested"
			in.Reviews[0].Reviews = rs
		}},
		{"review dropped", func(in *ManifestInput) { in.Reviews = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := fixtureInput(t)
			tc.mut(&in)
			m, err := Build(withGeneratedAt(in, fixedTime(18, 0)))
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if m.ManifestHash == base.ManifestHash {
				t.Fatalf("hash did not move for semantic change %q: %s", tc.name, base.ManifestHash)
			}
		})
	}
	// The only field that must NOT move the hash: the render time.
	same, err := Build(withGeneratedAt(fixtureInput(t), fixedTime(19, 15)))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if same.ManifestHash != base.ManifestHash {
		t.Fatalf("hash moved with only generated_at: %s != %s", same.ManifestHash, base.ManifestHash)
	}
}
