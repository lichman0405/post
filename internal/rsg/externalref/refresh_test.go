package externalref

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

// fakeFetcher answers canned upstream documents, recording each call so
// the tests can pin how often upstream was actually consulted.
type fakeFetcher struct {
	docs  []UpstreamMetadata
	calls int
	err   error
}

func (f *fakeFetcher) Fetch(ctx context.Context, ref Ref) (UpstreamMetadata, error) {
	f.calls++
	if f.err != nil {
		return UpstreamMetadata{}, f.err
	}
	if f.calls > len(f.docs) {
		return UpstreamMetadata{}, errf(ErrUpstreamUnavailable, "no canned document for call %d", f.calls)
	}
	return f.docs[f.calls-1], nil
}

func mustBuild(t *testing.T, id string, meta UpstreamMetadata, now func() time.Time) Snapshot {
	t.Helper()
	s, err := BuildSnapshot(id, meta, now)
	if err != nil {
		t.Fatalf("BuildSnapshot: %v", err)
	}
	return s
}

func doc(raw string) UpstreamMetadata {
	return UpstreamMetadata{UpstreamVersion: "v1", Metadata: json.RawMessage(raw)}
}

func TestRefreshFirstSnapshotAlwaysChanges(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC) }
	fetch := &fakeFetcher{docs: []UpstreamMetadata{doc(`{"title":"A"}`)}}
	r := NewRefresher(fetch).WithClock(now)

	res, err := r.Refresh(context.Background(), Ref{SourceType: "publication", ExternalIdentifier: "10.1000/a"}, "ref-1", nil)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !res.Changed {
		t.Fatal("first refresh (no head) must report Changed")
	}
	if fetch.calls != 1 {
		t.Fatalf("upstream consulted %d times, want exactly 1", fetch.calls)
	}
	if res.Snapshot.ExternalReferenceID != "ref-1" || !res.Snapshot.AccessedAt.Equal(now()) {
		t.Fatalf("snapshot = %+v, want ref-1 at the injected clock time", res.Snapshot)
	}
}

// TestRefreshUnchangedUpstreamReturnsHeadUntouched is the acceptance
// criterion in miniature (旧 snapshot 固定 / upstream refresh 不改历史):
// when upstream still matches the head, the head comes back byte-for-
// byte untouched and nothing new is produced — an unchanged upstream
// never grows the log. It also covers the jsonb-vs-Go spelling case: the
// head carries the jsonb-canonical text the database would hand back,
// the fresh fetch builds the Go-canonical form, and the comparison still
// sees ONE document.
func TestRefreshUnchangedUpstreamReturnsHeadUntouched(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC) }
	fetch := &fakeFetcher{docs: []UpstreamMetadata{doc(`{"a": 1, "title": "x"}`)}}
	r := NewRefresher(fetch).WithClock(now)

	// A head as the database would return it: jsonb-canonical text.
	head := Snapshot{
		ExternalReferenceID: "ref-1",
		AccessedAt:          now(),
		UpstreamVersion:     "v1",
		Metadata:            json.RawMessage(`{"a": 1, "title": "x"}`),
		SnapshotHash:        "db-derived",
	}
	headBefore := head

	res, err := r.Refresh(context.Background(), Ref{SourceType: "publication", ExternalIdentifier: "10.1000/a"}, "ref-1", &head)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.Changed {
		t.Fatal("unchanged upstream must report Changed=false")
	}
	// The head is the exact value passed in — untouched, not rebuilt.
	if !reflect.DeepEqual(res.Snapshot, headBefore) {
		t.Fatalf("returned snapshot differs from the head:\nhead: %+v\ngot:  %+v", headBefore, res.Snapshot)
	}
	if !res.Snapshot.AccessedAt.Equal(headBefore.AccessedAt) {
		t.Fatalf("head accessed_at mutated: %v → %v", headBefore.AccessedAt, res.Snapshot.AccessedAt)
	}
}

func TestRefreshChangedUpstreamAppendsNewSnapshot(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 14, 9, 0, 0, 0, time.UTC) }
	fetch := &fakeFetcher{docs: []UpstreamMetadata{doc(`{"title":"v2","year":2026}`)}}
	r := NewRefresher(fetch).WithClock(now)

	head := mustBuild(t, "ref-1", doc(`{"title":"v1","year":2025}`), now)
	headBefore := head

	res, err := r.Refresh(context.Background(), Ref{SourceType: "publication", ExternalIdentifier: "10.1000/a"}, "ref-1", &head)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !res.Changed {
		t.Fatal("changed upstream must report Changed=true")
	}
	// The new snapshot pins the new document, and the head is intact —
	// the caller appends the new row; it never rewrites the old one.
	if string(res.Snapshot.Metadata) == string(headBefore.Metadata) {
		t.Fatal("new snapshot carries the old document")
	}
	if !reflect.DeepEqual(head, headBefore) {
		t.Fatalf("refresh mutated the head: %+v → %+v", headBefore, head)
	}
}

// TestRefreshFailureFabricatesNothing: an unreachable/unusable upstream
// fails the refresh and produces no snapshot — stored history only ever
// holds facts actually observed.
func TestRefreshFailureFabricatesNothing(t *testing.T) {
	r := NewRefresher(&fakeFetcher{err: ErrUpstreamUnavailable})
	res, err := r.Refresh(context.Background(), Ref{SourceType: "publication", ExternalIdentifier: "10.1000/a"}, "ref-1", nil)
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("Refresh error = %v, want ErrUpstreamUnavailable", err)
	}
	if res.Changed || !reflect.DeepEqual(res.Snapshot, Snapshot{}) {
		t.Fatalf("failed refresh returned a snapshot to persist: %+v", res)
	}
}

// TestRefreshValidatesBeforeFetching: a nameless source is refused
// before any network call, and a refresher without a fetcher cannot
// invent upstream.
func TestRefreshValidatesBeforeFetching(t *testing.T) {
	cases := []struct {
		name string
		ref  Ref
		want error
	}{
		{"missing source_type", Ref{ExternalIdentifier: "10.1000/a"}, ErrNotResolvable},
		{"missing external_identifier", Ref{SourceType: "publication"}, ErrNotResolvable},
		{"both missing", Ref{}, ErrNotResolvable},
		// The source_type enum (external_reference.schema.json) is
		// canonical: a value outside it is refused before any network
		// call, exactly as the database would refuse the write — the
		// enum is the gate ladder's, but the refresh may not even try.
		{"enum-outside source_type", Ref{SourceType: "paper", ExternalIdentifier: "10.1000/a"}, ErrNotResolvable},
		{"enum case is exact", Ref{SourceType: "PUBLICATION", ExternalIdentifier: "10.1000/a"}, ErrNotResolvable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetch := &fakeFetcher{docs: []UpstreamMetadata{doc(`{}`)}}
			_, err := NewRefresher(fetch).Refresh(context.Background(), tc.ref, "ref-1", nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Refresh error = %v, want %v", err, tc.want)
			}
			if fetch.calls != 0 {
				t.Fatalf("upstream consulted %d times, want 0 (validation precedes the fetch)", fetch.calls)
			}
		})
	}

	_, err := NewRefresher(nil).Refresh(context.Background(), Ref{SourceType: "publication", ExternalIdentifier: "10.1000/a"}, "ref-1", nil)
	if !errors.Is(err, ErrUpstreamUnavailable) {
		t.Fatalf("nil fetcher error = %v, want ErrUpstreamUnavailable", err)
	}
}
