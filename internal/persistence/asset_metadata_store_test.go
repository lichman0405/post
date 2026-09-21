package persistence

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lichman0405/post/internal/application/assetmetadata"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// Task T0706 required test "asset metadata tests" — the store's pure half.
//
// The transaction itself (the two locks, the update, the same-transaction
// appendAudit, the rollback a failed audit takes with it) is pinned over
// real PostgreSQL in tests/integration/asset_metadata_test.go: only a
// database can settle whether a write commits or rolls back. What is left
// for unit tests is the folding and rendering these three functions do,
// and one of them decides a distinction the whole surface rests on:
// applyChanges must tell a nil pointer from a pointer to an empty value.
// A store that could not would clear a field every time a client renamed
// the slug.

const storePID = "01j9z6k3m4n5p6q7r8s9t0v1w2"

func strp(s string) *string { return &s }

func listp(items ...string) *[]string { return &items }

// storedMetadata is the row every case starts from: an asset that already
// has all six fields set, so a "was this cleared or left alone" question
// has an answer either way.
func storedMetadata() assets.AssetMetadata {
	return assets.AssetMetadata{
		Title:         "Governance Subject",
		Slug:          "governance-subject",
		Description:   "a description",
		Keywords:      []string{"grain", "boundary"},
		Contact:       []string{"alice@example.org"},
		Documentation: []string{"https://example.org/docs"},
	}
}

// TestApplyChangesLeavesAbsentFieldsAlone is the "unchanged" half: a nil
// pointer changes nothing, and — the assertion that matters — a field that
// is merely NOT PRESENT does not become the zero value. If it did, a
// revision that renamed a slug would also wipe the description, and the
// audit row would record that as a change the caller made.
func TestApplyChangesLeavesAbsentFieldsAlone(t *testing.T) {
	before := storedMetadata()
	after := applyChanges(before, assetmetadata.Changes{Slug: strp("renamed")})
	if !reflect.DeepEqual(before, storedMetadata()) {
		t.Fatalf("applyChanges mutated its input: %+v", before)
	}
	want := storedMetadata()
	want.Slug = "renamed"
	if !reflect.DeepEqual(after, want) {
		t.Errorf("after = %+v, want %+v", after, want)
	}
}

// TestApplyChangesClearsOnAnExplicitEmptyValue is the other half, and the
// one a bare (non-pointer) field could not express: a pointer to "" and a
// pointer to an empty slice are REQUESTS to clear, not absences. Both are
// asserted, because the two list shapes take different code paths.
func TestApplyChangesClearsOnAnExplicitEmptyValue(t *testing.T) {
	after := applyChanges(storedMetadata(), assetmetadata.Changes{
		Description:   strp(""),
		Keywords:      listp(),
		Contact:       listp(),
		Documentation: listp(),
	})
	if after.Description != "" {
		t.Errorf("description = %q, want cleared", after.Description)
	}
	// Cleared lists are non-nil empty slices: the columns are NOT NULL with
	// a '{}' default, so a nil here would be a Go zero value pretending to
	// be a database NULL the column cannot hold.
	for name, got := range map[string][]string{
		"keywords":      after.Keywords,
		"contact":       after.Contact,
		"documentation": after.Documentation,
	} {
		if got == nil {
			t.Errorf("%s = nil after a clear, want an empty list", name)
		}
		if len(got) != 0 {
			t.Errorf("%s = %q, want cleared", name, got)
		}
	}
	if after.Title != "Governance Subject" || after.Slug != "governance-subject" {
		t.Errorf("a clear of one field moved another: %+v", after)
	}
}

// TestApplyChangesNormalisesNilListsOfAnUntouchedRow: a row read back with
// a nil slice (a hand-written pgx scan, a fixture) must not put a nil into
// the column on the way out. The rule is the column's, not the request's,
// so it is asserted for a request that mentions no list at all.
func TestApplyChangesNormalisesNilListsOfAnUntouchedRow(t *testing.T) {
	after := applyChanges(assets.AssetMetadata{Title: "t", Slug: "s"}, assetmetadata.Changes{Title: strp("t2")})
	if after.Keywords == nil || after.Contact == nil || after.Documentation == nil {
		t.Errorf("a list was left nil: %+v", after)
	}
}

// TestApplyChangesWritesTheValuesItIsGiven is the trivial direction, stated
// so the tests above cannot be passing because applyChanges ignores
// everything: a non-nil pointer writes.
func TestApplyChangesWritesTheValuesItIsGiven(t *testing.T) {
	after := applyChanges(storedMetadata(), assetmetadata.Changes{
		Title:         strp("A New Title"),
		Description:   strp("new prose"),
		Keywords:      listp("one", "two"),
		Contact:       listp("bob@example.org"),
		Documentation: listp("https://example.org/new"),
	})
	if after.Title != "A New Title" || after.Description != "new prose" {
		t.Errorf("scalars not written: %+v", after)
	}
	if !reflect.DeepEqual(after.Keywords, []string{"one", "two"}) {
		t.Errorf("keywords = %q", after.Keywords)
	}
	if !reflect.DeepEqual(after.Contact, []string{"bob@example.org"}) {
		t.Errorf("contact = %q", after.Contact)
	}
	if !reflect.DeepEqual(after.Documentation, []string{"https://example.org/new"}) {
		t.Errorf("documentation = %q", after.Documentation)
	}
}

// TestMetadataSnapshotExcludesTheReservedCover is a scope assertion with a
// reason: the cover slot is written by nothing in this build, so a value in
// an audit before/after pair would record a change that cannot happen.
// metadataSnapshot is the half that feeds the audit row; metadataFromRow
// (which serves the response) does carry the cover, because a reader has to
// be able to see that the slot is empty.
func TestMetadataSnapshotExcludesTheReservedCover(t *testing.T) {
	row := sqlc.ResearchAsset{
		Pid:           storePID,
		Title:         "Governance Subject",
		Slug:          "governance-subject",
		Description:   "a description",
		Keywords:      []string{"grain"},
		Contact:       []string{"alice@example.org"},
		Documentation: []string{"https://example.org/docs"},
		CoverBlobID:   pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
	}
	snapshot := metadataSnapshot(row)
	if snapshot.CoverBlobID != "" {
		t.Errorf("metadataSnapshot carries the cover (%q): the audit pair must record only what a revision can move",
			snapshot.CoverBlobID)
	}
	if snapshot.Title != row.Title || snapshot.Description != row.Description || snapshot.Slug != row.Slug {
		t.Errorf("metadataSnapshot dropped a revisable field: %+v", snapshot)
	}
	if !reflect.DeepEqual(snapshot.Keywords, row.Keywords) {
		t.Errorf("metadataSnapshot.Keywords = %q, want %q", snapshot.Keywords, row.Keywords)
	}

	// The response-facing renderer does carry it, and it reads back as the
	// empty string while the column is NULL — the state of every asset
	// today.
	full := metadataFromRow(sqlc.ResearchAsset{Pid: storePID, Title: "t", Slug: "s"})
	if full.CoverBlobID != "" {
		t.Errorf("metadataFromRow(CoverBlobID NULL) = %q, want empty", full.CoverBlobID)
	}
}

// TestMetadataSummaryKeysAreTheColumnNames: the audit row's before/after
// document is read by a human in the Activity page without a decoder
// dictionary, so its keys are the columns' own names. The set is asserted
// EXACTLY — a summary that grew a seventh key would be recording a field
// the revision does not write.
func TestMetadataSummaryKeysAreTheColumnNames(t *testing.T) {
	summary := metadataSummary(storedMetadata())
	want := map[string]any{
		"title":         "Governance Subject",
		"slug":          "governance-subject",
		"description":   "a description",
		"keywords":      []string{"grain", "boundary"},
		"contact":       []string{"alice@example.org"},
		"documentation": []string{"https://example.org/docs"},
	}
	if !reflect.DeepEqual(summary, want) {
		t.Errorf("summary = %#v, want %#v", summary, want)
	}
}

// TestMetadataSummaryRendersAnEmptyListNotNull: the three list columns are
// NOT NULL DEFAULT '{}' (migration 00125), so their summary renders [] and
// never null. A stored summary that said null would describe a column state
// the schema cannot produce.
func TestMetadataSummaryRendersAnEmptyListNotNull(t *testing.T) {
	summary := metadataSummary(assets.AssetMetadata{})
	for _, key := range []string{"keywords", "contact", "documentation"} {
		got, ok := summary[key].([]string)
		if !ok {
			t.Fatalf("%s = %#v, want a []string", key, summary[key])
		}
		if got == nil {
			t.Errorf("%s = nil, want an empty list", key)
		}
	}
}

// TestNonNilList pins the normaliser the three column reads all go through.
func TestNonNilList(t *testing.T) {
	if got := nonNilList(nil); got == nil || len(got) != 0 {
		t.Errorf("nonNilList(nil) = %#v, want an empty non-nil slice", got)
	}
	items := []string{"a"}
	if got := nonNilList(items); !reflect.DeepEqual(got, items) {
		t.Errorf("nonNilList(%q) = %q, want it unchanged", items, got)
	}
}

// TestMapAssetMetadataErrorKeepsTheSentinels: the mapping is what decides
// whether a transport can answer each outcome with its own status, so the
// negative is asserted too — an unknown failure must NOT be reported as a
// validation failure the caller would try to fix by editing the request.
func TestMapAssetMetadataErrorKeepsTheSentinels(t *testing.T) {
	for _, want := range []error{
		assetmetadata.ErrValidation,
		assetmetadata.ErrForbidden,
		assetmetadata.ErrProjectNotFound,
		assetmetadata.ErrAssetNotFound,
		assetmetadata.ErrCoverNotSupported,
		assetmetadata.ErrStore,
	} {
		if got := mapAssetMetadataError(want); !reflect.DeepEqual(got, want) {
			t.Errorf("mapAssetMetadataError(%v) = %v, want it unchanged", want, got)
		}
	}
	unknown := errUnknown{}
	got := mapAssetMetadataError(unknown)
	if !errors.Is(got, assetmetadata.ErrStore) {
		t.Errorf("mapAssetMetadataError(%v) = %v, want a wrap of ErrStore", unknown, got)
	}
	if errors.Is(got, unknown) || got.Error() == unknown.Error() {
		t.Error("an unknown failure passed through unmapped: the transport would answer 500 for a store outage")
	}
	if !strings.Contains(got.Error(), unknown.Error()) {
		t.Errorf("mapped error = %q, want the cause kept for the log", got)
	}
}

type errUnknown struct{}

func (errUnknown) Error() string { return "some driver failure" }
