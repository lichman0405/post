package releases

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// ptr is a test helper for optional string fields.
func ptr(s string) *string { return &s }

// fixedTime pins a timestamp with microseconds so golden bytes stay
// stable. Times deliberately start non-UTC in some fixtures — the render
// normalizes them.
func fixedTime(hour, minute int) time.Time {
	return time.Date(2026, 2, 20, hour, minute, 0, 654321000, time.UTC)
}

// fixedLocalTime is the same instant as fixedTime but in a fixed-offset
// zone, to prove the render normalizes timestamps to UTC.
func fixedLocalTime(hour, minute int) time.Time {
	loc := time.FixedZone("fixture", 5*3600)
	return time.Date(2026, 2, 20, hour, minute, 0, 654321000, loc)
}

// fixtureStateManifest renders the shared state export fixture (T0206
// shape): two objects (one with two versions and a policy pin), one
// relation and two blob refs, at a fixed export time. It is the state
// half of every release fixture in this package.
func fixtureStateManifest(t *testing.T) *manifest.Manifest {
	t.Helper()
	m, err := manifest.Build(fixtureState(), fixtureStateSnapshot(), fixedTime(14, 0))
	if err != nil {
		t.Fatalf("manifest.Build: %v", err)
	}
	return m
}

// fixtureState is the state the fixture export is built for.
func fixtureState() domain.ProjectState {
	return domain.ProjectState{
		ID:              "state-00000001",
		ProjectID:       "proj-00000001",
		GitCommitSHA:    ptr("abcdef0123456789abcdef0123456789abcdef01"),
		ManifestVersion: manifest.FormatV1,
	}
}

// fixtureStateSnapshot is the fixture export's snapshot. Payloads
// deliberately arrive with shuffled keys and whitespace so the golden
// bytes prove the canonicalization, and the object list is deliberately
// out of order so they prove the stable ordering.
func fixtureStateSnapshot() manifest.Snapshot {
	policy := "policy-00000001"
	branch := "branch-00000002"
	return manifest.Snapshot{
		ObjectVersions: []manifest.ObjectVersion{
			{
				ID: "ov-00000002", ObjectID: "obj-00000002", ObjectType: "hypothesis",
				VersionNo: 1, StateID: "state-00000002", BranchID: &branch,
				SchemaRef:      manifest.SchemaRef{ID: "https://open-rd.example/schemas/hypothesis.schema.json", Version: "1"},
				Title:          "H1",
				LifecycleState: "active",
				Payload:        json.RawMessage(`{ "b" : 2, "a" : [1,2,3] }`),
				IntegrityHash:  "sha256:2222", CreatedBy: "user-00000001", CreatedAt: fixedTime(11, 0),
			},
			{
				ID: "ov-00000001", ObjectID: "obj-00000001", ObjectType: "experiment",
				VersionNo: 1, StateID: "state-00000001",
				SchemaRef:          manifest.SchemaRef{ID: "https://open-rd.example/schemas/experiment.schema.json", Version: "1"},
				Title:              "E1",
				LifecycleState:     "active",
				Payload:            json.RawMessage(`{"nested":{"y":1,"x":"z"},"temperature_k": 273.15}`),
				VisibilityPolicyID: &policy,
				IntegrityHash:      "sha256:1111", CreatedBy: "user-00000001", CreatedAt: fixedTime(10, 0),
			},
			{
				ID: "ov-00000003", ObjectID: "obj-00000001", ObjectType: "experiment",
				VersionNo: 2, StateID: "state-00000002", BranchID: &branch,
				SchemaRef:          manifest.SchemaRef{ID: "https://open-rd.example/schemas/experiment.schema.json", Version: "1"},
				Title:              "E1",
				LifecycleState:     "active",
				Payload:            json.RawMessage(`{"nested":{"y":1,"x":"z"},"temperature_k": 300.0,"note":"warmed"}`),
				VisibilityPolicyID: &policy,
				IntegrityHash:      "sha256:3333", CreatedBy: "user-00000001", CreatedAt: fixedTime(12, 0),
			},
		},
		RelationVersions: []manifest.RelationVersion{
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
		BlobRefs: []manifest.BlobRef{
			{ID: "blob-00000002", Hash: "sha256:bbbb"},
			{ID: "blob-00000001", Hash: "sha256:aaaa"},
		},
	}
}

// fixturePolicyPin is the pinned policy snapshot: one organization
// version and one project version, documents deliberately spelled with
// shuffled keys and whitespace so the golden bytes prove the
// canonicalization.
func fixturePolicyPin() *PolicyPin {
	return &PolicyPin{
		Organization: &PinnedPolicyVersion{
			ID:        "policy-00000002",
			Version:   "v2",
			Document:  json.RawMessage(`{ "release_min_reviewers" : 2, "main_protected" : true }`),
			CreatedBy: "user-00000002",
			CreatedAt: fixedLocalTime(9, 0),
		},
		Project: &PinnedPolicyVersion{
			ID:        "policy-00000003",
			Version:   "v1",
			Document:  json.RawMessage(`{ "release_min_reviewers" : 3 }`),
			CreatedBy: "user-00000001",
			CreatedAt: fixedLocalTime(10, 0),
		},
	}
}

// fixtureSchemaPins lists the schema refs of the fixture snapshot,
// deliberately out of order — the render sorts them.
func fixtureSchemaPins() []SchemaPin {
	return []SchemaPin{
		{ID: "https://open-rd.example/schemas/experiment.schema.json", Version: "1", ContentHash: "eeee"},
		{ID: "https://open-rd.example/schemas/hypothesis.schema.json", Version: "1", ContentHash: "hhhh"},
	}
}

// fixtureReviews is the review record: two PRs, reviews out of order and
// with non-UTC timestamps — the render sorts records by PR number and
// reviews by (created_at, id).
func fixtureReviews() []ReviewRecord {
	return []ReviewRecord{
		{
			PullRequestNumber: 2,
			ProposedStateID:   "state-00000002",
			Reviews: []Review{
				{ID: "rev-00000003", ReviewerID: "user-00000003", ReviewKind: "integrity", Decision: "approved", Body: "provenance complete", CreatedAt: fixedLocalTime(16, 0)},
				{ID: "rev-00000002", ReviewerID: "user-00000002", ReviewKind: "scientific", Decision: "approved", Body: "method sound", CreatedAt: fixedLocalTime(15, 0)},
			},
		},
		{
			PullRequestNumber: 1,
			ProposedStateID:   "state-00000001",
			Reviews: []Review{
				{ID: "rev-00000001", ReviewerID: "user-00000002", ReviewKind: "scientific", Decision: "approved", Body: "", CreatedAt: fixedLocalTime(14, 0)},
			},
		},
	}
}

// buildFixture renders the populated golden fixture: the shared state
// export plus the full pin set, at a fixed render time.
func buildFixture(t *testing.T) *ReleaseManifest {
	t.Helper()
	m, err := Build(ManifestInput{
		ProjectID:   "proj-00000001",
		StateID:     "state-00000001",
		Version:     "v1.0.0",
		GeneratedAt: fixedTime(18, 0),
		State:       fixtureStateManifest(t),
		Policy:      fixturePolicyPin(),
		Schemas:     fixtureSchemaPins(),
		Reviews:     fixtureReviews(),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return m
}
