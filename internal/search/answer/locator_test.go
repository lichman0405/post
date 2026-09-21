package answer_test

import (
	"context"
	"testing"

	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/answer"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// hrefOf answers a one-source ranking with no provider — a fallback, which is
// where every source list comes from in these tests — and returns the single
// source's href.
func hrefOf(t *testing.T, ranked ranking.Ranked) string {
	t.Helper()
	got, err := newGenerator(t, answer.Deps{}).Answer(context.Background(), answer.Input{
		Result:  resultFor(ranked),
		Signals: signalsFor(),
	})
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if len(got.Sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(got.Sources))
	}
	return got.Sources[0].Href
}

// TestSourcesAreAddressableOrSayWhyNot is the closure of T0906's second
// acceptance criterion ("source click 可定位"): every entity type the
// projection produces either has an address or is explicitly unaddressable,
// and the table below is the decision for each one. A projection that gains
// an entity type fails the coverage check at the end of this test until the
// new type is decided, so no source can quietly inherit a wrong link.
func TestSourcesAreAddressableOrSayWhyNot(t *testing.T) {
	cases := []struct {
		name   string
		ranked ranking.Ranked
		want   string
		why    string
	}{
		{
			name:   "asset",
			ranked: rankedAsset(assetRef, assetPID, cleanFactors()),
			want:   "/api/v1/assets/AST-0001?version=2",
			why:    "the contract's asset page data, with the version the candidate pins",
		},
		{
			name:   "knowledge",
			ranked: rankedKnowledge(1, knownRef, knownPID, cleanFactors()),
			want:   "/api/v1/knowledge/KNW-0007",
			why:    "the contract's published knowledge read, addressed by pid",
		},
		{
			name:   "release",
			ranked: rankedRelease(1),
			want:   "/api/v1/projects/" + projectID + "/releases/" + releaseUUID,
			why:    "the release read, which is nested under its project",
		},
		{
			name:   "state",
			ranked: rankedState(1),
			want:   "",
			why:    "no route reads a state by id; every object read is nested under a branch a candidate does not carry",
		},
		{
			name:   "object version",
			ranked: rankedObjectVersion(1),
			want:   "",
			why:    "the same: the routes that take (projectId, objectId) read the object's evidence, not the object",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hrefOf(t, tc.ranked); got != tc.want {
				t.Fatalf("href = %q, want %q (%s)", got, tc.want, tc.why)
			}
		})
	}

	// Every entity type the projection can write has a decision above, and
	// the two kinds retrieval returns are both covered.
	decided := map[string]bool{}
	for _, tc := range cases {
		decided[tc.ranked.EntityType] = true
		decided[tc.ranked.Kind] = true
	}
	for _, entityType := range search.EntityTypes() {
		if !decided[entityType] {
			t.Fatalf("the projection writes entity type %q, which this test does not decide an address for", entityType)
		}
	}
	for _, kind := range []string{retrieval.KindDocument, retrieval.KindObjectVersion} {
		if !decided[kind] {
			t.Fatalf("retrieval returns kind %q, which this test does not decide an address for", kind)
		}
	}
}

// TestSourceHrefIsNeverAGuess: a link is only minted when every segment of it
// is one path segment, so a source is either addressed correctly or not at
// all. A link that 404s would tell a reader something false about what the
// platform holds, which is the defect a fabricated citation is.
func TestSourceHrefIsNeverAGuess(t *testing.T) {
	// An identity carrying a separator cannot be a path segment: the route
	// patterns match exactly one, so escaping it would produce a path the
	// platform does not serve.
	broken := rankedAsset(assetRef, "AST-0001/../../admin", cleanFactors())
	if got := hrefOf(t, broken); got != "" {
		t.Fatalf("href = %q for an identity that is not one path segment, want no link", got)
	}

	// A release without its project has no address either: the release id is
	// unique within a project's timeline, not globally.
	orphan := rankedRelease(1)
	orphan.Candidate.ProjectID = ""
	if got := hrefOf(t, orphan); got != "" {
		t.Fatalf("href = %q for a release with no project, want no link", got)
	}

	// An asset candidate with no version label is still addressable — the
	// route serves the newest version the caller may see — but the query
	// parameter must be absent rather than empty.
	unversioned := rankedAsset(assetRef, assetPID, cleanFactors())
	unversioned.Version = ""
	unversioned.Candidate.Version = ""
	if got := hrefOf(t, unversioned); got != "/api/v1/assets/AST-0001" {
		t.Fatalf("href = %q, want the bare asset path", got)
	}
}

// TestSourceHrefsAreDistinctAndStable: two sources of different kinds never
// share an address, and the same source is addressed the same way on every
// run — the property a fixture pins.
func TestSourceHrefsAreDistinctAndStable(t *testing.T) {
	seen := map[string]string{}
	sources := []ranking.Ranked{
		rankedAsset(assetRef, assetPID, cleanFactors()),
		rankedKnowledge(2, knownRef, knownPID, cleanFactors()),
		rankedRelease(3),
	}
	for i := range sources {
		href := hrefOf(t, sources[i])
		if href == "" {
			t.Fatalf("source %d has no href", i)
		}
		if other, ok := seen[href]; ok {
			t.Fatalf("two sources share the address %q: %s and %s", href, other, sources[i].Ref)
		}
		seen[href] = sources[i].Ref
		if again := hrefOf(t, sources[i]); again != href {
			t.Fatalf("href is not stable: %q then %q", href, again)
		}
	}
}
