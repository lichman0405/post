package manifest

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// ptr is a test helper for optional string fields.
func ptr(s string) *string { return &s }

// fixedTime pins a timestamp with microseconds so golden bytes stay stable.
func fixedTime(hour, minute int) time.Time {
	return time.Date(2026, 1, 15, hour, minute, 0, 123456000, time.UTC)
}

// fixtureState is the state every manifest in this file is built for: a
// git-compat state carrying its own recorded commit sha. Build takes no
// project input (purity invariant: the hash is a pure function of the
// state's recorded content), so there is no project half to fixture.
func fixtureState() domain.ProjectState {
	return domain.ProjectState{
		ID:              "state-00000001",
		ProjectID:       "proj-00000001",
		GitCommitSHA:    ptr("abcdef0123456789abcdef0123456789abcdef01"),
		ManifestVersion: FormatV1,
	}
}

// fixtureSnapshot is the golden test's snapshot: two objects (one with two
// versions and a policy pin), one relation, two blob attachments. Payloads
// deliberately arrive with shuffled keys and whitespace so the golden bytes
// prove the canonicalization, and the object list is deliberately out of
// order so they prove the stable ordering.
func fixtureSnapshot() Snapshot {
	policy := "policy-00000001"
	branch := "branch-00000001"
	return Snapshot{
		ObjectVersions: []ObjectVersion{
			{
				ID: "ov-00000002", ObjectID: "obj-00000002", ObjectType: "hypothesis",
				VersionNo: 1, StateID: "state-00000002", BranchID: &branch,
				SchemaRef:      SchemaRef{ID: "https://open-rd.example/schemas/hypothesis.schema.json", Version: "1"},
				Title:          "H1",
				LifecycleState: "active",
				Payload:        json.RawMessage(`{ "b" : 2, "a" : [1,2,3] }`),
				IntegrityHash:  "sha256:2222", CreatedBy: "user-00000001", CreatedAt: fixedTime(11, 0),
			},
			{
				ID: "ov-00000001", ObjectID: "obj-00000001", ObjectType: "experiment",
				VersionNo: 1, StateID: "state-00000001",
				SchemaRef:          SchemaRef{ID: "https://open-rd.example/schemas/experiment.schema.json", Version: "1"},
				Title:              "E1",
				LifecycleState:     "active",
				Payload:            json.RawMessage(`{"nested":{"y":1,"x":"z"},"temperature_k": 273.15}`),
				VisibilityPolicyID: &policy,
				IntegrityHash:      "sha256:1111", CreatedBy: "user-00000001", CreatedAt: fixedTime(10, 0),
			},
			{
				ID: "ov-00000003", ObjectID: "obj-00000001", ObjectType: "experiment",
				VersionNo: 2, StateID: "state-00000002", BranchID: &branch,
				SchemaRef:          SchemaRef{ID: "https://open-rd.example/schemas/experiment.schema.json", Version: "1"},
				Title:              "E1",
				LifecycleState:     "active",
				Payload:            json.RawMessage(`{"nested":{"y":1,"x":"z"},"temperature_k": 300.0,"note":"warmed"}`),
				VisibilityPolicyID: &policy,
				IntegrityHash:      "sha256:3333", CreatedBy: "user-00000001", CreatedAt: fixedTime(12, 0),
			},
		},
		RelationVersions: []RelationVersion{
			{
				ID: "rv-00000001", RelationID: "rel-00000001", VersionNo: 1,
				StateID:               "state-00000002",
				RelationType:          "supports",
				SourceObjectVersionID: "ov-00000001",
				TargetObjectVersionID: "ov-00000002",
				Payload:               json.RawMessage(`{"scope": "preliminary"}`),
				IntegrityHash:         "sha256:4444", CreatedBy: "user-00000001", CreatedAt: fixedTime(13, 0),
			},
		},
		BlobRefs: []BlobRef{
			{ID: "blob-00000002", Hash: "sha256:bbbb"},
			{ID: "blob-00000001", Hash: "sha256:aaaa"},
		},
	}
}

// buildFixture runs Build on the shared fixture with a fixed export time.
func buildFixture(t *testing.T) *Manifest {
	t.Helper()
	m, err := Build(fixtureState(), fixtureSnapshot(), fixedTime(14, 0))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return m
}

// TestBuildDeterministic exports the same state twice at different export
// times: the hash and the content bytes must be identical (acceptance:
// 同一 state 多次导出 hash 相同), only generated_at may move.
func TestBuildDeterministic(t *testing.T) {
	state := fixtureState()
	snap := fixtureSnapshot()
	m1, err := Build(state, snap, fixedTime(14, 0))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	m2, err := Build(state, snap, fixedTime(16, 30))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if m1.StateHash != m2.StateHash {
		t.Fatalf("hash moved between exports of the same state: %s != %s", m1.StateHash, m2.StateHash)
	}
	b1, err := m1.ContentCanonicalJSON()
	if err != nil {
		t.Fatalf("content 1: %v", err)
	}
	b2, err := m2.ContentCanonicalJSON()
	if err != nil {
		t.Fatalf("content 2: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("content bytes differ between exports:\n%s\n%s", b1, b2)
	}
	if m1.GeneratedAt.Equal(m2.GeneratedAt) {
		t.Fatalf("generated_at should reflect the export time")
	}
}

// TestStableOrdering shuffles the snapshot rows and re-sorts them: the
// canonical output must not depend on input order (requirement: stable
// ordering).
func TestStableOrdering(t *testing.T) {
	state := fixtureState()
	ordered := buildFixture(t)
	shuffled := fixtureSnapshot()
	shuffled.ObjectVersions[0], shuffled.ObjectVersions[1], shuffled.ObjectVersions[2] =
		shuffled.ObjectVersions[2], shuffled.ObjectVersions[0], shuffled.ObjectVersions[1]
	shuffled.BlobRefs[0], shuffled.BlobRefs[1] = shuffled.BlobRefs[1], shuffled.BlobRefs[0]
	other, err := Build(state, shuffled, fixedTime(14, 0))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if ordered.StateHash != other.StateHash {
		t.Fatalf("hash depends on input order: %s != %s", ordered.StateHash, other.StateHash)
	}
	a, _ := ordered.CanonicalJSON()
	b, _ := other.CanonicalJSON()
	if !bytes.Equal(a, b) {
		t.Fatalf("bytes depend on input order:\n%s\n%s", a, b)
	}
	// The sorted order is the canonical one: object 1's versions come
	// before object 2's, grouped by version.
	if ordered.ObjectVersions[0].ObjectID != "obj-00000001" || ordered.ObjectVersions[1].VersionNo != 2 {
		t.Fatalf("object versions not in canonical (object_id, version_no) order: %+v", ordered.ObjectVersions)
	}
	if ordered.BlobRefs[0].ID != "blob-00000001" {
		t.Fatalf("blob refs not sorted by id: %+v", ordered.BlobRefs)
	}
}

// TestSemanticChangesMoveHash proves every semantic change moves the hash
// (acceptance: 任何 semantic change 改变 hash). Each case mutates exactly
// one semantic input on a fresh snapshot and asserts the hash changed.
func TestSemanticChangesMoveHash(t *testing.T) {
	base := buildFixture(t)
	state := fixtureState()
	mutations := []struct {
		name string
		run  func(s *Snapshot)
	}{
		{"object payload", func(s *Snapshot) {
			s.ObjectVersions[0].Payload = json.RawMessage(`{"b":2,"a":[1,2,4]}`)
		}},
		{"object title", func(s *Snapshot) { s.ObjectVersions[0].Title = "renamed" }},
		{"object lifecycle", func(s *Snapshot) { s.ObjectVersions[0].LifecycleState = "aborted" }},
		{"object policy pin", func(s *Snapshot) { s.ObjectVersions[0].VisibilityPolicyID = ptr("policy-00000009") }},
		{"schema version", func(s *Snapshot) { s.ObjectVersions[0].SchemaRef.Version = "2" }},
		{"object type", func(s *Snapshot) { s.ObjectVersions[0].ObjectType = "material" }},
		{"extra object version", func(s *Snapshot) {
			s.ObjectVersions = append(s.ObjectVersions, ObjectVersion{
				ID: "ov-00000009", ObjectID: "obj-00000003", ObjectType: "finding",
				VersionNo: 1, StateID: "state-00000003",
				SchemaRef: SchemaRef{ID: "https://open-rd.example/schemas/finding.schema.json", Version: "1"},
				Title:     "F1", LifecycleState: "active",
				Payload:       json.RawMessage(`{}`),
				IntegrityHash: "sha256:9999", CreatedBy: "user-00000001", CreatedAt: fixedTime(15, 0),
			})
		}},
		{"removed object version", func(s *Snapshot) { s.ObjectVersions = s.ObjectVersions[:2] }},
		{"relation payload", func(s *Snapshot) {
			s.RelationVersions[0].Payload = json.RawMessage(`{"scope":"final"}`)
		}},
		{"relation endpoints", func(s *Snapshot) {
			s.RelationVersions[0].TargetObjectVersionID = "ov-00000003"
		}},
		{"extra blob", func(s *Snapshot) {
			s.BlobRefs = append(s.BlobRefs, BlobRef{ID: "blob-00000009", Hash: "sha256:cccc"})
		}},
		{"blob hash", func(s *Snapshot) { s.BlobRefs[0].Hash = "sha256:changed" }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			snap := fixtureSnapshot()
			tc.run(&snap)
			m, err := Build(state, snap, fixedTime(14, 0))
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if m.StateHash == base.StateHash {
				t.Fatalf("hash did not move for %s: %s", tc.name, m.StateHash)
			}
		})
	}
	// The git ref is semantic too: it is the state's OWN recorded commit
	// sha (project_states.git_commit_sha) — dropping, emptying or changing
	// it must move the hash, and a null git ref is not the same as a pinned
	// one.
	t.Run("git ref present vs absent", func(t *testing.T) {
		noGitState := state
		noGitState.GitCommitSHA = nil
		m, err := Build(noGitState, fixtureSnapshot(), fixedTime(14, 0))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if m.GitRef != nil {
			t.Fatalf("expected null git_ref without a commit sha, got %+v", m.GitRef)
		}
		if m.StateHash == base.StateHash {
			t.Fatalf("hash did not move when the git ref was dropped")
		}
		// An empty recorded sha renders the same null git ref: a state with
		// no git-compat commit is a state with no git-compat commit, whether
		// recorded as NULL or as an empty string.
		emptyGitState := state
		emptyGitState.GitCommitSHA = ptr("")
		m2, err := Build(emptyGitState, fixtureSnapshot(), fixedTime(14, 0))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if m2.GitRef != nil {
			t.Fatalf("expected null git_ref with an empty commit sha, got %+v", m2.GitRef)
		}
		if m2.StateHash != m.StateHash {
			t.Fatalf("nil and empty commit shas must hash alike: %s != %s", m2.StateHash, m.StateHash)
		}
		if m2.StateHash == base.StateHash {
			t.Fatalf("hash did not move when the git ref was dropped")
		}
	})
	t.Run("git ref changed", func(t *testing.T) {
		otherGitState := state
		otherGitState.GitCommitSHA = ptr("1111111111111111111111111111111111111111")
		m, err := Build(otherGitState, fixtureSnapshot(), fixedTime(14, 0))
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if m.GitRef == nil || *m.GitRef != "1111111111111111111111111111111111111111" {
			t.Fatalf("expected the changed commit sha in git_ref, got %+v", m.GitRef)
		}
		if m.StateHash == base.StateHash {
			t.Fatalf("hash did not move when the commit sha changed")
		}
	})
}

// TestCanonicalJSONPayload pins the payload canonicalization: sorted keys,
// compact form, numbers preserved exactly, nested objects and arrays.
func TestCanonicalJSONPayload(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`{ "b" : 2, "a" : 1 }`, `{"a":1,"b":2}`},
		{"{\n\t\"x\": [1, 2, 3]\n}", `{"x":[1,2,3]}`},
		{`{"big": 123456789012345678901234567890}`, `{"big":123456789012345678901234567890}`},
		{`{"nested":{"z":1,"a":{"k":"v"}}}`, `{"nested":{"a":{"k":"v"},"z":1}}`},
		{`[3, 1, 2]`, `[3,1,2]`},
		{`"scalar"`, `"scalar"`},
		{`1.5`, `1.5`},
		{`{"unicode":"研究"}`, `{"unicode":"研究"}`},
	}
	for _, tc := range cases {
		got, err := CanonicalJSON([]byte(tc.in))
		if err != nil {
			t.Fatalf("CanonicalJSON(%s): %v", tc.in, err)
		}
		if string(got) != tc.want {
			t.Fatalf("CanonicalJSON(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
	if _, err := CanonicalJSON([]byte(`{"broken":`)); err == nil {
		t.Fatalf("expected an error for invalid JSON")
	}
	// The input must be exactly one JSON value: trailing content after the
	// first value would be silently dropped, and this function feeds the
	// state hash — two stored payloads differing only after the first value
	// must not hash alike. Trailing whitespace stays fine (jsonb never
	// produces it, but the decoder skips it before EOF).
	for _, tc := range []string{`{"a":1}garbage`, `{"a":1}{"b":2}`, `[1,2] [3]`, `"x"y`} {
		if _, err := CanonicalJSON([]byte(tc)); err == nil {
			t.Fatalf("expected an error for trailing content in %q", tc)
		}
	}
	if got, err := CanonicalJSON([]byte("{\"a\":1} \n\t")); err != nil {
		t.Fatalf("trailing whitespace must stay valid: %v", err)
	} else if string(got) != `{"a":1}` {
		t.Fatalf("trailing whitespace canonicalized wrong: %s", got)
	}
}

// TestVerifyHash proves the manifest self-check: the built manifest
// verifies, and a tampered semantic field fails it.
func TestVerifyHash(t *testing.T) {
	m := buildFixture(t)
	if !m.VerifyHash() {
		t.Fatalf("built manifest must verify its own hash")
	}
	tampered := *m
	tampered.ObjectVersions = append([]ObjectVersion{}, m.ObjectVersions...)
	tampered.ObjectVersions[0].Title = "tampered"
	if tampered.VerifyHash() {
		t.Fatalf("tampered manifest must fail verification")
	}
	tampered2 := *m
	tampered2.StateHash = "sha256:deadbeef"
	if tampered2.VerifyHash() {
		t.Fatalf("foreign hash must fail verification")
	}
}

// TestManifestContentMirrorsDocument guards the content struct against
// drift: the hash input must be exactly the document minus generated_at and
// state_hash.
func TestManifestContentMirrorsDocument(t *testing.T) {
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
	delete(docMap, "state_hash")
	if !reflect.DeepEqual(docMap, contentMap) {
		t.Fatalf("document and hash content diverged:\ndocument: %v\ncontent:  %v", docMap, contentMap)
	}
}

// TestBuildRejectsForeignManifestVersion refuses to export a state written
// under a format this exporter does not speak.
func TestBuildRejectsForeignManifestVersion(t *testing.T) {
	state := fixtureState()
	state.ManifestVersion = "v9"
	if _, err := Build(state, fixtureSnapshot(), fixedTime(14, 0)); err == nil {
		t.Fatalf("expected an error for manifest format v9")
	}
}

// TestEmptyManifest exports a genesis state (no versions, no git ref): the
// document still has every array present (empty, not null) and a stable
// hash.
func TestEmptyManifest(t *testing.T) {
	state := domain.ProjectState{
		ID:              "state-00000000",
		ProjectID:       "proj-00000000",
		ManifestVersion: FormatV1,
	}
	m, err := Build(state, Snapshot{}, fixedTime(14, 0))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	doc, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	for _, field := range []string{`"object_versions":[]`, `"relation_versions":[]`, `"schema_refs":[]`, `"policy_refs":[]`, `"blob_refs":[]`, `"git_ref":null`} {
		if !strings.Contains(string(doc), field) {
			t.Fatalf("empty manifest must render %s, got %s", field, doc)
		}
	}
	if m.StateHash == "" || !m.VerifyHash() {
		t.Fatalf("empty manifest must carry a verifying hash")
	}
	// An empty manifest of the same state must hash identically on every
	// export.
	m2, err := Build(state, Snapshot{}, fixedTime(20, 0))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if m2.StateHash != m.StateHash {
		t.Fatalf("empty manifest hash moved: %s != %s", m2.StateHash, m.StateHash)
	}
}

// TestGoldenManifestIsSchemaValid validates the golden export against the
// canonical rsg-manifest schema (specs/schemas/rsg-manifest.schema.json) —
// the exporter's output must stay inside the contract.
func TestGoldenManifestIsSchemaValid(t *testing.T) {
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	m := buildFixture(t)
	doc, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	err = reg.Validate(schemareg.Ref{ID: "https://open-rd.example/schemas/rsg-manifest.schema.json", Version: schemareg.CanonicalV1}, doc)
	if err != nil {
		t.Fatalf("golden manifest does not satisfy the rsg-manifest schema: %v\n%s", err, doc)
	}
}
