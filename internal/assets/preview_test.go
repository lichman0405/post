package assets

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/validation"
)

// Task T0704 required test "publish preview tests".
//
// The preview is the read-only half of docs/23 §4's highest-risk
// operation: before a private→public publication is decided, it says who
// would see what — and, the part this file is mostly about, it NAMES every
// private thing the publication leans on instead of letting that thing
// ride along into a public, immutable document.
//
// The tests below are organised the way the acceptance criteria are:
//
//	1. the preview is complete and repeatable  (TestPreviewListsEveryCategory,
//	   TestPreviewIsRepeatable, TestPreviewMetadataIsOrderedByKey,
//	   TestPreviewCarriesTheObjectVersions)
//	2. previewing changes nothing              (TestPreviewIsReadOnly,
//	   plus the integration case that runs it against a real database)
//	3. a private dependency is named, never dropped
//	   (TestPreviewBlockerOnUnresolvedPin,
//	   TestPreviewNamesForeignPrivateRef,
//	   TestPreviewBlockersOnBlobsTheDataAccessClaimsAreOpen)
//
// and the determinism claim is proven by running the same input twice and
// comparing the marshalled bytes, not by reading the code for map
// iterations.

// The projects and entities the previews below resolve against.
const (
	// publishingProject is the project the publish is previewed in.
	publishingProject = "11111111-1111-4111-8111-111111111111"
	// foreignProject is a second project: the one a ref must not disclose.
	foreignProject = "22222222-2222-4222-8222-222222222222"
	// objectVersionRowID and objectRowID are the object version one origin
	// ref of TestPreviewCarriesTheObjectVersions points at.
	objectVersionRowID = "33333333-3333-4333-8333-333333333333"
	objectRowID        = "44444444-4444-4444-8444-444444444444"
	// openBlobID and restrictedBlobID are blob ids the manifest metadata
	// names in the blob tests.
	openBlobID       = "blobs/open-1"
	restrictedBlobID = "blobs/restricted-1"
)

// storedAsset is the asset validCandidate is published under: a dataset of
// the publishing project.
func storedAsset() *StoredAsset {
	return &StoredAsset{
		ID:              "55555555-5555-4555-8555-555555555555",
		PID:             samplePID,
		Type:            TypeDataset,
		Title:           "A published dataset",
		OriginProjectID: publishingProject,
	}
}

// answeredRelease is the state's answer for the one origin ref
// validCandidate declares ("release:" + sampleUUID): the release exists and
// lives in the given project, of the given visibility.
func answeredRelease(projectID string, visibility Visibility) []StoredRef {
	return []StoredRef{{
		Ref:               OriginRef("release:" + sampleUUID),
		Resolved:          true,
		ProjectID:         projectID,
		ProjectVisibility: visibility,
	}}
}

// publicDataset is validCandidate with its declared data actually openly
// attached, and every entity it names public: the base for tests about one
// thing, so that the rest of the impact is quiet.
//
// It is needed because validCandidate's own fixture declares the blob id
// "a value" (blob_ids is required metadata for a dataset, and the fixture
// fills it with the sample value), which resolves to nothing — a real
// finding, and the one the blob tests assert, but noise in a test about a
// pin or a ref.
func publicDataset(t *testing.T) (PublishCandidate, CurrentState) {
	t.Helper()
	c := candidateWithBlobs(t, validCandidate(t), openBlobID)
	return c, CurrentState{
		Asset: storedAsset(),
		Refs:  answeredRelease(publishingProject, VisibilityPublic),
		Pins:  []StoredPin{pinnedVersion(t, VisibilityPublic)},
		Blobs: []StoredBlob{{BlobID: openBlobID, Access: rights.DataAccessOpen}},
	}
}

// pinnedVersion is the state's answer for validCandidate's pin
// (anotherPID@2.1): the pinned version exists, with the given visibility.
func pinnedVersion(t *testing.T, visibility Visibility) StoredPin {
	t.Helper()
	pin, ok := NewDependencyPin(anotherPID, "2.1")
	if !ok {
		t.Fatal("fixture: the sample pids must form a pin")
	}
	return StoredPin{Pin: pin, Visibility: visibility}
}

// pinnedVersionRef returns validCandidate's pin in its canonical text form,
// which is the name every assertion below matches on.
func pinnedVersionRef(t *testing.T) string {
	t.Helper()
	return string(pinnedVersion(t, VisibilityPrivate).Pin)
}

// mustPreview runs the preview and fails when the state could not answer
// for the candidate.
func mustPreview(t *testing.T, c PublishCandidate, state CurrentState) ImpactPreview {
	t.Helper()
	p, err := Preview(PreviewRequest{ProjectID: publishingProject, Candidate: c}, state)
	if err != nil {
		t.Fatalf("Preview returned an error for a state it can answer for: %v", err)
	}
	return p
}

// namedDependency returns the private dependency a preview names for one
// (kind, ref) pair. A miss is the failure this whole task is about — a
// private thing the publication leans on that the preview did not name —
// so it fails the test rather than returning a zero value.
func namedDependency(t *testing.T, p ImpactPreview, kind, ref string) PrivateDependency {
	t.Helper()
	for _, d := range p.PrivateDependencies {
		if d.Kind == kind && d.Ref == ref {
			return d
		}
	}
	t.Fatalf("the preview does not name the %s dependency %q; private_dependencies = %s",
		kind, ref, renderDeps(p.PrivateDependencies))
	return PrivateDependency{}
}

// notNamed asserts a preview does not list one (kind, ref) pair, and says
// how the pair was accounted for if it does.
func notNamed(t *testing.T, p ImpactPreview, kind, ref string) {
	t.Helper()
	for _, d := range p.PrivateDependencies {
		if d.Kind == kind && d.Ref == ref {
			t.Fatalf("the preview names %q as a private dependency (blocking=%v), want it accounted for elsewhere: %s",
				ref, d.Blocking, d.Detail)
		}
	}
}

// blockerCodes returns the codes of one blocker list, in order.
func blockerCodes(bs []Blocker) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Code)
	}
	return out
}

// blockerFor returns the blocker carrying a code, and fails when the list
// does not have one.
func blockerFor(t *testing.T, bs []Blocker, code string) Blocker {
	t.Helper()
	for _, b := range bs {
		if b.Code == code {
			return b
		}
	}
	t.Fatalf("no %s in %v", code, blockerCodes(bs))
	return Blocker{}
}

// renderDeps renders a private dependency list for a failure message.
func renderDeps(deps []PrivateDependency) string {
	if len(deps) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(deps))
	for _, d := range deps {
		parts = append(parts, d.Kind+" "+d.Ref)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// containsAll reports whether s mentions every fragment — used to assert
// that a detail sentence actually explains itself.
func containsAll(s string, fragments ...string) bool {
	for _, f := range fragments {
		if !strings.Contains(s, f) {
			return false
		}
	}
	return true
}

// --- 1. completeness ---------------------------------------------------

// TestPreviewListsEveryCategory is acceptance criterion 1: the preview
// answers all five questions docs/23 §4 asks — the objects, the metadata,
// the blobs, the refs, and the private dependencies — over the SAME
// candidate, in one result.
func TestPreviewListsEveryCategory(t *testing.T) {
	c, state := publicDataset(t)
	state.Refs = append(state.Refs, StoredRef{
		Ref:               OriginRef("object_version:" + objectVersionRowID),
		Resolved:          true,
		ProjectID:         publishingProject,
		ProjectVisibility: VisibilityPublic,
		Object: &StoredObject{
			ObjectVersionID: objectVersionRowID,
			ObjectID:        objectRowID,
			Title:           "Sample 7 diffraction pattern",
		},
	})
	c.OriginRefs = []string{"release:" + sampleUUID, "object_version:" + objectVersionRowID}

	p := mustPreview(t, c, state)

	// The asset half: which asset the version lands under.
	if !p.Asset.Resolved || p.Asset.PID != samplePID || p.Asset.OriginProjectID != publishingProject {
		t.Errorf("asset = %+v, want the stored dataset of %s", p.Asset, publishingProject)
	}
	if p.Version != "1.0" || p.TargetVisibility != VisibilityPublic {
		t.Errorf("version/visibility = %q/%q, want 1.0/public", p.Version, p.TargetVisibility)
	}
	// The object half: the object version the publication would carry, and
	// the visibility it has today.
	if len(p.Objects) != 1 {
		t.Fatalf("objects = %+v, want the one object version ref", p.Objects)
	}
	if got := p.Objects[0]; got.ObjectVersionID != objectVersionRowID || got.ObjectID != objectRowID ||
		got.ProjectID != publishingProject || got.CurrentVisibility != VisibilityPublic ||
		got.Title != "Sample 7 diffraction pattern" {
		t.Errorf("objects[0] = %+v, want the referenced version with its title and today's visibility", got)
	}
	// The metadata half: every declared key, with its value.
	if len(p.Metadata) != len(requiredMetadata[TypeDataset]) {
		t.Errorf("metadata = %d keys, want the %d a dataset declares", len(p.Metadata), len(requiredMetadata[TypeDataset]))
	}
	// The blob half: the blob ids the manifest metadata names, with the
	// access each has today.
	if len(p.Blobs) != 1 || p.Blobs[0].BlobID != openBlobID || p.Blobs[0].CurrentAccess != BlobAccessOpen {
		t.Errorf("blobs = %+v, want the openly attached blob the dataset declares", p.Blobs)
	}
	// The ref half: both refs, in the order the candidate declares them.
	if len(p.Refs) != 2 || p.Refs[0].Ref != "release:"+sampleUUID || p.Refs[1].Ref != "object_version:"+objectVersionRowID {
		t.Errorf("refs = %+v, want both refs in candidate order", p.Refs)
	}
	// The dependency half: every pin, resolved.
	if len(p.Dependencies) != 1 || p.Dependencies[0].Pin != pinnedVersionRef(t) ||
		!p.Dependencies[0].Resolved || p.Dependencies[0].CurrentVisibility != string(VisibilityPublic) {
		t.Errorf("dependencies = %+v, want the pin resolved as public", p.Dependencies)
	}
	// Nothing here is private, so nothing is named and nothing blocks.
	if len(p.PrivateDependencies) != 0 {
		t.Errorf("private_dependencies = %s, want none: every referenced entity is public", renderDeps(p.PrivateDependencies))
	}
	if !p.Publishable {
		t.Errorf("publishable = false with no blocker and no private dependency: %v / %v",
			blockerCodes(p.PublishBlockers), blockerCodes(p.RightsBlockers))
	}
}

// TestPreviewCarriesTheObjectVersions pins the one place where an origin
// ref produces an object row: an object_version ref is the published
// content itself (docs/11 §3), so the preview shows it as what a reader
// would get, with the visibility of the project it lives in. The same
// version referenced twice is one object.
func TestPreviewCarriesTheObjectVersions(t *testing.T) {
	c, state := publicDataset(t)
	ref := "object_version:" + objectVersionRowID
	c.OriginRefs = []string{"release:" + sampleUUID, ref, ref}
	state.Refs = []StoredRef{
		{
			Ref:               OriginRef("release:" + sampleUUID),
			Resolved:          true,
			ProjectID:         publishingProject,
			ProjectVisibility: VisibilityPublic,
		},
		{
			Ref:               OriginRef(ref),
			Resolved:          true,
			ProjectID:         foreignProject,
			ProjectVisibility: VisibilityPrivate,
			Object: &StoredObject{
				ObjectVersionID: objectVersionRowID,
				ObjectID:        objectRowID,
				Title:           "Sample 7 diffraction pattern",
			},
		},
	}

	p := mustPreview(t, c, state)

	if len(p.Objects) != 1 {
		t.Fatalf("objects = %+v, want the same version listed once", p.Objects)
	}
	if p.Objects[0].CurrentVisibility != VisibilityPrivate || p.Objects[0].ProjectID != foreignProject {
		t.Errorf("objects[0] = %+v, want the object's own project and its visibility today", p.Objects[0])
	}
	// The refs list still has all three entries: a ref is what the candidate
	// declared, even when two of them name the same entity.
	if len(p.Refs) != 3 {
		t.Errorf("refs = %+v, want one entry per declared ref", p.Refs)
	}
	// And the private project the object lives in is named as a dependency.
	dep := namedDependency(t, p, string(KindObjectVersion), ref)
	if !dep.Blocking {
		t.Errorf("the object_version dependency is not blocking: %s", dep.Detail)
	}
}

// TestPreviewMetadataIsOrderedByKey keeps the metadata list in the
// document's own read order. The manifest is a Go map, so "some order"
// would be a different order on a different run; the list must be sorted,
// and the sort must be ascending by key.
func TestPreviewMetadataIsOrderedByKey(t *testing.T) {
	c, state := publicDataset(t)

	p := mustPreview(t, c, state)

	keys := make([]string, 0, len(p.Metadata))
	for _, m := range p.Metadata {
		keys = append(keys, m.Key)
	}
	for i := 1; i < len(keys); i++ {
		if keys[i-1] >= keys[i] {
			t.Fatalf("metadata keys %v are not ascending", keys)
		}
	}
	if len(keys) == 0 || keys[0] != "access_level" {
		t.Errorf("metadata = %v, want the dataset's keys in ascending order", keys)
	}
}

// --- 3. the private dependency leak, the crux of the task -------------

// TestPreviewNamesPinnedPrivateVersion is acceptance criterion 3 for the
// case docs/11 §5 calls a Dependency: a public publication that pins a
// version which is still private would make that pin — and the fact that
// the version exists at all — part of a public, immutable document. The
// preview must name it and refuse to call the publication safe.
func TestPreviewNamesPinnedPrivateVersion(t *testing.T) {
	c, state := publicDataset(t)
	state.Pins = []StoredPin{pinnedVersion(t, VisibilityPrivate)}

	p := mustPreview(t, c, state)

	pin := pinnedVersionRef(t)
	dep := namedDependency(t, p, DependencyAssetVersion, pin)
	if !dep.Blocking {
		t.Errorf("a public publication pinned to a private version is not blocking: %s", dep.Detail)
	}
	if !containsAll(dep.Detail, "private") {
		t.Errorf("the dependency detail does not say what was found: %q", dep.Detail)
	}
	if p.Publishable {
		t.Error("publishable = true while the publication pins a private version")
	}
	// The pin resolved, so it is not a publish blocker: what makes it unsafe
	// is what it resolves TO. The two lists say different things and this
	// case is the one that shows why they are separate.
	if len(p.PublishBlockers) != 0 {
		t.Errorf("publish_blockers = %v, want none: the pin resolves", blockerCodes(p.PublishBlockers))
	}
	// The dependency entry itself keeps the pin out of the "nothing to
	// report" reading: it is listed, resolved, and private.
	if len(p.Dependencies) != 1 || p.Dependencies[0].CurrentVisibility != string(VisibilityPrivate) {
		t.Errorf("dependencies = %+v, want the pin with its visibility today", p.Dependencies)
	}
}

// TestPreviewBlockersOnUnresolvedPin covers the other way a pin can be
// unsafe: it names no stored version at all. Then the published document
// would depend on something nobody can resolve — and the preview has to
// say so, in the blocker list (it cannot execute) and in the dependency
// list (the publication leans on it).
//
// It blocks whatever the target visibility is: a dangling pin is a broken
// document, not a disclosure.
func TestPreviewBlockersOnUnresolvedPin(t *testing.T) {
	for _, target := range []Visibility{VisibilityPublic, VisibilityPrivate} {
		t.Run(string(target), func(t *testing.T) {
			c, state := publicDataset(t)
			c.Visibility = target
			state.Pins = nil

			p := mustPreview(t, c, state)

			pin := pinnedVersionRef(t)
			dep := namedDependency(t, p, DependencyAssetVersion, pin)
			if !dep.Blocking {
				t.Errorf("an unresolvable pin is not blocking: %s", dep.Detail)
			}
			b := blockerFor(t, p.PublishBlockers, CodePreviewPinUnresolved)
			if b.Field != "dependency_pins" {
				t.Errorf("blocker field = %q, want dependency_pins", b.Field)
			}
			if !containsAll(b.Detail, pin) {
				t.Errorf("the blocker does not name the pin: %q", b.Detail)
			}
			if len(p.Dependencies) != 1 || p.Dependencies[0].Resolved {
				t.Errorf("dependencies = %+v, want the pin listed as unresolved", p.Dependencies)
			}
			if p.Publishable {
				t.Error("publishable = true while a pin resolves to nothing")
			}
		})
	}
}

// TestPreviewNamesForeignPrivateRef is acceptance criterion 3 for an
// origin ref: a public publication whose provenance points into ANOTHER
// private project would disclose that project's entity in a public
// document. Named, blocking.
func TestPreviewNamesForeignPrivateRef(t *testing.T) {
	c, state := publicDataset(t)
	state.Refs = append(state.Refs, StoredRef{
		Ref:               OriginRef("state:" + objectVersionRowID),
		Resolved:          true,
		ProjectID:         foreignProject,
		ProjectVisibility: VisibilityPrivate,
	})
	c.OriginRefs = []string{"release:" + sampleUUID, "state:" + objectVersionRowID}

	p := mustPreview(t, c, state)

	ref := "state:" + objectVersionRowID
	dep := namedDependency(t, p, string(KindState), ref)
	if !dep.Blocking {
		t.Errorf("a ref into another private project is not blocking: %s", dep.Detail)
	}
	if !containsAll(dep.Detail, foreignProject) {
		t.Errorf("the dependency detail does not name the project it would disclose: %q", dep.Detail)
	}
	if p.Publishable {
		t.Error("publishable = true while the version discloses another project's private state")
	}
}

// TestPreviewOwnProjectPrivateRefDoesNotBlock is the case that keeps the
// rule from being absurd. A version MUST carry at least one origin ref
// (specs/schemas/research-asset-version.schema.json: origin_refs, minItems
// 1), a private project may publish an asset (docs/12 §2), and it
// publishes the object versions of its own project — so a ref into the
// PUBLISHING project is what the version is made of, not a dependency on
// somebody else's private work.
//
// It is still NAMED: the private thing the publication points at is real,
// and a human deciding has to see it. It just does not block.
func TestPreviewOwnProjectPrivateRefDoesNotBlock(t *testing.T) {
	c, state := publicDataset(t)
	state.Refs = answeredRelease(publishingProject, VisibilityPrivate)

	p := mustPreview(t, c, state)

	dep := namedDependency(t, p, string(KindRelease), "release:"+sampleUUID)
	if dep.Blocking {
		t.Errorf("a ref into the publishing project's own private state blocks: %s", dep.Detail)
	}
	if !containsAll(dep.Detail, "publishing project") {
		t.Errorf("the dependency detail does not say why it does not block: %q", dep.Detail)
	}
	if !p.Publishable {
		t.Errorf("publishable = false although nothing here is unsafe: %v",
			blockerCodes(p.PublishBlockers))
	}
	// The ref is resolved, so it is not a blocker either.
	if len(p.PublishBlockers) != 0 {
		t.Errorf("publish_blockers = %v, want none", blockerCodes(p.PublishBlockers))
	}
	if p.Refs[0].CurrentVisibility != string(VisibilityPrivate) || !p.Refs[0].Resolved {
		t.Errorf("refs[0] = %+v, want the ref resolved with today's visibility", p.Refs[0])
	}
}

// TestPreviewBlockersOnUnresolvedRef: a canonical ref the state says does
// not exist. The published provenance would point at nothing.
func TestPreviewBlockersOnUnresolvedRef(t *testing.T) {
	c, state := publicDataset(t)
	state.Refs = []StoredRef{
		{Ref: OriginRef("release:" + sampleUUID)},
		{
			Ref:               OriginRef("state:" + objectVersionRowID),
			Resolved:          true,
			ProjectID:         publishingProject,
			ProjectVisibility: VisibilityPublic,
		},
	}
	c.OriginRefs = []string{"release:" + sampleUUID, "state:" + objectVersionRowID}

	p := mustPreview(t, c, state)

	b := blockerFor(t, p.PublishBlockers, CodePreviewRefUnresolved)
	if b.Field != "origin_refs" || !containsAll(b.Detail, "release:"+sampleUUID) {
		t.Errorf("blocker = %+v, want it to name the ref that does not resolve", b)
	}
	dep := namedDependency(t, p, string(KindRelease), "release:"+sampleUUID)
	if !dep.Blocking {
		t.Errorf("an unresolvable ref is not blocking: %s", dep.Detail)
	}
	if p.Refs[0].Resolved {
		t.Errorf("refs[0] = %+v, want resolved=false", p.Refs[0])
	}
	if p.Publishable {
		t.Error("publishable = true while the provenance points at nothing")
	}
}

// TestPreviewBlockerOnForeignPrivateRefStaysPrivate: the same foreign ref,
// published as private. Nothing becomes visible, so nothing blocks — but
// the private dependency is still named, because the publication leans on
// it and a human deciding whether to widen it later needs to know.
func TestPreviewBlockerOnForeignPrivateRefStaysPrivate(t *testing.T) {
	c, state := publicDataset(t)
	c.Visibility = VisibilityPrivate
	state.Refs = answeredRelease(foreignProject, VisibilityPrivate)
	state.Pins = []StoredPin{pinnedVersion(t, VisibilityPrivate)}

	p := mustPreview(t, c, state)

	ref := "release:" + sampleUUID
	dep := namedDependency(t, p, string(KindRelease), ref)
	if dep.Blocking {
		t.Errorf("a private publication blocks on a private dependency: %s", dep.Detail)
	}
	namedDependency(t, p, DependencyAssetVersion, pinnedVersionRef(t))
	if !p.Publishable {
		t.Errorf("publishable = false for a publication that widens nothing: %v",
			blockerCodes(p.PublishBlockers))
	}
}

// --- blobs -------------------------------------------------------------

// TestPreviewReportsBlobAccess is the blob half of "who would see what":
// each blob the manifest names, with today's access, in open | restricted |
// unknown.
func TestPreviewReportsBlobAccess(t *testing.T) {
	blob := func(id string, access rights.DataAccess) StoredBlob {
		return StoredBlob{BlobID: id, Access: access}
	}
	cases := []struct {
		name        string
		declared    []string
		accessLevel string
		blobs       []StoredBlob
		want        []PreviewBlob
	}{
		{
			name:        "open",
			declared:    []string{openBlobID},
			accessLevel: "restricted",
			blobs:       []StoredBlob{blob(openBlobID, rights.DataAccessOpen)},
			want: []PreviewBlob{
				{BlobID: openBlobID, Resolved: true, CurrentAccess: BlobAccessOpen, DeclaredAccess: "restricted"},
			},
		},
		{
			name:        "restricted",
			declared:    []string{restrictedBlobID},
			accessLevel: "restricted",
			blobs:       []StoredBlob{blob(restrictedBlobID, rights.DataAccessRestricted)},
			want: []PreviewBlob{
				{BlobID: restrictedBlobID, Resolved: true, CurrentAccess: BlobAccessRestricted, DeclaredAccess: "restricted"},
			},
		},
		{
			name:        "unknown",
			declared:    []string{"doi:10.5281/zenodo.1234"},
			accessLevel: "restricted",
			want: []PreviewBlob{
				{BlobID: "doi:10.5281/zenodo.1234", CurrentAccess: BlobAccessUnknown, DeclaredAccess: "restricted"},
			},
		},
		{
			name:        "promised open",
			declared:    []string{restrictedBlobID},
			accessLevel: "open",
			blobs:       []StoredBlob{blob(restrictedBlobID, rights.DataAccessRestricted)},
			want: []PreviewBlob{
				{BlobID: restrictedBlobID, Resolved: true, CurrentAccess: BlobAccessRestricted, DeclaredAccess: "open"},
			},
		},
		{
			name:        "declaration order, no repeats",
			declared:    []string{restrictedBlobID, openBlobID, restrictedBlobID},
			accessLevel: "restricted",
			blobs: []StoredBlob{
				blob(openBlobID, rights.DataAccessOpen),
				blob(restrictedBlobID, rights.DataAccessRestricted),
			},
			want: []PreviewBlob{
				{BlobID: restrictedBlobID, Resolved: true, CurrentAccess: BlobAccessRestricted, DeclaredAccess: "restricted"},
				{BlobID: openBlobID, Resolved: true, CurrentAccess: BlobAccessOpen, DeclaredAccess: "restricted"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCandidate(t)
			state := CurrentState{
				Asset: storedAsset(),
				Refs:  answeredRelease(publishingProject, VisibilityPublic),
				Pins:  []StoredPin{pinnedVersion(t, VisibilityPublic)},
				Blobs: tc.blobs,
			}
			c = candidateWithBlobs(t, c, tc.declared...)
			c = candidateWithMetadata(t, c, map[string]any{"access_level": tc.accessLevel})

			p := mustPreview(t, c, state)

			if len(p.Blobs) != len(tc.want) {
				t.Fatalf("blobs = %+v, want %+v", p.Blobs, tc.want)
			}
			for i := range tc.want {
				if p.Blobs[i] != tc.want[i] {
					t.Errorf("blobs[%d] = %+v, want %+v", i, p.Blobs[i], tc.want[i])
				}
			}
		})
	}
}

// TestPreviewBlockersOnBlobsTheDataAccessClaimsAreOpen is the case where a
// blob blocks: the version's own documents promise open data access while
// the blob is not open. docs/17 §5 keeps the two apart — public metadata
// is not open download — and the published document is immutable, so a
// promise the data cannot keep could never be corrected. The preview must
// name the blob and refuse.
//
// Two documents can make that promise and both count: the rights
// declaration (visibility.data_access, internal/rights) and the dataset
// manifest's own access_level metadata (specs/schemas/dataset.schema.json
// requires it as open | restricted).
func TestPreviewBlockersOnBlobsTheDataAccessClaimsAreOpen(t *testing.T) {
	cases := []struct {
		name          string
		rightsAccess  rights.DataAccess
		manifestLevel string
		wantBlocking  bool
	}{
		{name: "rights says open", rightsAccess: rights.DataAccessOpen, manifestLevel: "restricted", wantBlocking: true},
		{name: "manifest says open", rightsAccess: rights.DataAccessRestricted, manifestLevel: "open", wantBlocking: true},
		{name: "both say open", rightsAccess: rights.DataAccessOpen, manifestLevel: "open", wantBlocking: true},
		{name: "neither says open", rightsAccess: rights.DataAccessRestricted, manifestLevel: "restricted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCandidate(t)
			c.RightsJSON = rightsJSONWithDataAccess(t, tc.rightsAccess)
			c = candidateWithMetadata(t, c, map[string]any{
				"blob_ids":     []string{restrictedBlobID},
				"access_level": tc.manifestLevel,
			})
			state := CurrentState{
				Asset: storedAsset(),
				Refs:  answeredRelease(publishingProject, VisibilityPublic),
				Pins:  []StoredPin{pinnedVersion(t, VisibilityPublic)},
				Blobs: []StoredBlob{{BlobID: restrictedBlobID, Access: rights.DataAccessRestricted}},
			}

			p := mustPreview(t, c, state)

			dep := namedDependency(t, p, DependencyBlob, restrictedBlobID)
			if dep.Blocking != tc.wantBlocking {
				t.Errorf("blob dependency blocking = %v, want %v (%s)", dep.Blocking, tc.wantBlocking, dep.Detail)
			}
			if dep.Blocking && !containsAll(dep.Detail, "data_access", "open") {
				t.Errorf("the dependency detail does not state the contradiction: %q", dep.Detail)
			}
			if got := p.Publishable; got == tc.wantBlocking {
				t.Errorf("publishable = %v with a blocking blob = %v", got, tc.wantBlocking)
			}
			if p.Blobs[0].DeclaredAccess != map[bool]string{true: "open", false: "restricted"}[tc.wantBlocking] {
				t.Errorf("blobs[0].DeclaredAccess = %q, want the promise the documents make", p.Blobs[0].DeclaredAccess)
			}
		})
	}
}

// TestPreviewNamesRestrictedBlobUnderARestrictedDeclaration is the case
// docs/55 calls RESTRICTED_BLOB and docs/17 §5 sanctions: public metadata
// over data that stays behind the download gate. The blob is named — it is
// what the publication leans on and what a reuser would need — and the
// failure to fetch it is the access control working, not a leak.
func TestPreviewNamesRestrictedBlobUnderARestrictedDeclaration(t *testing.T) {
	c, state := publicDataset(t)
	c = candidateWithBlobs(t, c, restrictedBlobID)
	c = candidateWithMetadata(t, c, map[string]any{"access_level": "restricted"})
	state.Blobs = []StoredBlob{{BlobID: restrictedBlobID, Access: rights.DataAccessRestricted}}

	p := mustPreview(t, c, state)

	dep := namedDependency(t, p, DependencyBlob, restrictedBlobID)
	if dep.Blocking {
		t.Errorf("a restricted blob under a restricted declaration blocks: %s", dep.Detail)
	}
	if !containsAll(dep.Detail, "download gate") {
		t.Errorf("the dependency detail does not say where the bytes stay): %q", dep.Detail)
	}
	if !p.Publishable {
		t.Errorf("publishable = false, want the publication to be executable: %v", blockerCodes(p.PublishBlockers))
	}
}

// TestPreviewDoesNotNameOpenBlobs: an openly attached blob is visible
// today, so it is not a private dependency. The distinction matters — a
// preview that named everything would say nothing.
func TestPreviewDoesNotNameOpenBlobs(t *testing.T) {
	c, state := publicDataset(t)

	p := mustPreview(t, c, state)

	notNamed(t, p, DependencyBlob, openBlobID)
	if len(p.PrivateDependencies) != 0 {
		t.Errorf("private_dependencies = %s, want none", renderDeps(p.PrivateDependencies))
	}
}

// --- 2. repeatability and read-only ------------------------------------

// TestPreviewIsRepeatable is acceptance criterion 2's halfway mark and the
// determinism claim of docs/31 Gate C: the same candidate against the same
// state gives the same bytes. The second run is handed the state in a
// DIFFERENT slice order, because a reader assembling the answers in
// whatever order its query returned them must not change the preview.
func TestPreviewIsRepeatable(t *testing.T) {
	c := validCandidate(t)
	refs := []StoredRef{
		{
			Ref:               OriginRef("release:" + sampleUUID),
			Resolved:          true,
			ProjectID:         publishingProject,
			ProjectVisibility: VisibilityPrivate,
		},
		{
			Ref:               OriginRef("object_version:" + objectVersionRowID),
			Resolved:          true,
			ProjectID:         foreignProject,
			ProjectVisibility: VisibilityPrivate,
			Object:            &StoredObject{ObjectVersionID: objectVersionRowID, ObjectID: objectRowID, Title: "Sample 7"},
		},
	}
	blobs := []StoredBlob{
		{BlobID: openBlobID, Access: rights.DataAccessOpen},
		{BlobID: restrictedBlobID, Access: rights.DataAccessRestricted},
	}
	pins := []StoredPin{pinnedVersion(t, VisibilityPrivate)}
	c.OriginRefs = []string{"release:" + sampleUUID, "object_version:" + objectVersionRowID}
	c = candidateWithBlobs(t, c, openBlobID, restrictedBlobID)
	c = candidateWithMetadata(t, c, map[string]any{"access_level": "restricted"})

	first := mustPreview(t, c, CurrentState{Asset: storedAsset(), Refs: refs, Blobs: blobs, Pins: pins})

	// Same answers, reversed. Every input list is reordered at once, so a
	// preview that leaked any list's order into its output would differ.
	reversed := func(n int) []int {
		out := make([]int, 0, n)
		for i := n - 1; i >= 0; i-- {
			out = append(out, i)
		}
		return out
	}
	shuffled := CurrentState{Asset: storedAsset()}
	for _, i := range reversed(len(refs)) {
		shuffled.Refs = append(shuffled.Refs, refs[i])
	}
	for _, i := range reversed(len(blobs)) {
		shuffled.Blobs = append(shuffled.Blobs, blobs[i])
	}
	for _, i := range reversed(len(pins)) {
		shuffled.Pins = append(shuffled.Pins, pins[i])
	}
	second := mustPreview(t, c, shuffled)

	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("two previews of the same candidate/state differ:\n first: %s\nsecond: %s", firstJSON, secondJSON)
	}
	if !reflect.DeepEqual(first, second) {
		t.Error("two previews of the same candidate/state are not DeepEqual")
	}
	// A preview with nothing to say still says it in lists, not nulls: a
	// client reading private_dependencies must not have to tell "absent"
	// from "empty".
	if !strings.Contains(string(firstJSON), `"private_dependencies":[`) {
		t.Errorf("private_dependencies is not an array in %s", firstJSON)
	}
}

// TestPreviewIsReadOnly states the property the integration test then
// proves against a real database: Preview reads its arguments. It hands
// the same state value to two previews of two different candidates and
// checks the state is unchanged — the shallow version of "nothing was
// written", which catches the mistake a type system cannot: a reader that
// memoises into the state it was given.
func TestPreviewIsReadOnly(t *testing.T) {
	c := validCandidate(t)
	state := CurrentState{
		Asset: storedAsset(),
		Refs:  answeredRelease(foreignProject, VisibilityPrivate),
		Pins:  []StoredPin{pinnedVersion(t, VisibilityPrivate)},
		Blobs: []StoredBlob{{BlobID: openBlobID, Access: rights.DataAccessOpen}},
	}
	before, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}

	mustPreview(t, c, state)
	other := validCandidate(t)
	other.Version = "2.0"
	other.Visibility = VisibilityPrivate
	mustPreview(t, other, state)

	after, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("the state changed across two previews:\nbefore: %s\n after: %s", before, after)
	}
}

// --- the gate is the one enforcer --------------------------------------

// TestPreviewReusesTheGate is the "do not build a second rulebook" test:
// the publish blockers of a candidate that breaks a publish rule are the
// gate's own refusals, code for code, and the facts are the gate's facts.
// A preview that decided publishability on its own would report something
// else here.
func TestPreviewReusesTheGate(t *testing.T) {
	c := validCandidate(t)
	c.CreatorIDs = nil
	c.Version = ""
	c.Manifest = nil
	c.IntegrityHash = "not-a-hash"
	state := CurrentState{
		Asset: storedAsset(),
		Refs:  answeredRelease(publishingProject, VisibilityPublic),
		Pins:  []StoredPin{pinnedVersion(t, VisibilityPublic)},
	}

	gateRes, gateErr := Gate(c)
	if gateErr == nil {
		t.Fatal("fixture: the gate must refuse this candidate")
	}
	p := mustPreview(t, c, state)

	got := blockerCodes(p.PublishBlockers)
	want := refusalCodes(t, gateErr)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("publish_blockers = %v, want the gate's own refusals %v in order", got, want)
	}
	if p.Facts != previewFacts(gateRes.Facts) {
		t.Errorf("facts = %+v, want the gate's %+v", p.Facts, previewFacts(gateRes.Facts))
	}
	if p.Publishable {
		t.Error("publishable = true for a candidate the gate refuses")
	}
}

// TestPreviewFactsMirrorsTheLadderFacts keeps PreviewFacts and
// validation.AssetFacts in step. PreviewFacts is a second struct on
// purpose (the ladder's type carries a json.RawMessage and no tags, and
// putting tags on it would make a wire contract out of a validation
// struct), and the price of a second struct is that it can fall behind —
// this test is the payment. It fails when the ladder gains a fact, loses
// one, or renames one.
func TestPreviewFactsMirrorsTheLadderFacts(t *testing.T) {
	ladder := map[string]bool{}
	lt := reflect.TypeOf(validation.AssetFacts{})
	for i := 0; i < lt.NumField(); i++ {
		f := lt.Field(i)
		if f.Type.Kind() == reflect.Bool {
			ladder[f.Name] = true
		}
	}
	if len(ladder) == 0 {
		t.Fatal("validation.AssetFacts carries no bool fact: the ladder's vocabulary moved somewhere else")
	}
	wire := map[string]bool{}
	seen := map[string]string{}
	pt := reflect.TypeOf(PreviewFacts{})
	for i := 0; i < pt.NumField(); i++ {
		f := pt.Field(i)
		if f.Type.Kind() != reflect.Bool {
			t.Errorf("PreviewFacts.%s is %s, want bool — every ladder fact is a bool", f.Name, f.Type)
			continue
		}
		wire[f.Name] = true
		tag := f.Tag.Get("json")
		if tag != snakeCase(f.Name) {
			t.Errorf("PreviewFacts.%s has json tag %q, want %q", f.Name, tag, snakeCase(f.Name))
		}
		if prev, dup := seen[tag]; dup {
			t.Errorf("PreviewFacts.%s and %s share the json name %q", prev, f.Name, tag)
		}
		seen[tag] = f.Name
	}
	for name := range ladder {
		if !wire[name] {
			t.Errorf("validation.AssetFacts.%s has no PreviewFacts field: the preview would drop the fact", name)
		}
	}
	for name := range wire {
		if !ladder[name] {
			t.Errorf("PreviewFacts.%s is not a validation.AssetFacts fact: the preview would invent one", name)
		}
	}
}

// snakeCase renders a Go field name as the json name it must carry.
func snakeCase(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			if b.Len() > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// --- rights blockers ---------------------------------------------------

// TestPreviewReportsRightsBlockers is the fifth item of the preview's own
// list: what the rights declaration itself refuses. The refusal must be
// the rights model's — its code and its field path INSIDE the document,
// prefixed by the request field the document arrives in — and the assets
// gate's own "the rights document does not parse" must not be listed
// beside it: one broken declaration, reported once.
func TestPreviewReportsRightsBlockers(t *testing.T) {
	cases := []struct {
		name       string
		rightsJSON string
		wantCode   string
		wantField  string
		wantGone   []string
	}{
		{
			name:       "no document at all",
			rightsJSON: "",
			wantCode:   CodeMissingRights,
			wantField:  "rights",
			wantGone:   []string{CodeMissingRights, CodeInvalidRights},
		},
		{
			name:       "not a document",
			rightsJSON: `{"version": 1, "unknown_field": true}`,
			wantCode:   rights.CodeMalformedDocument,
			wantField:  "rights",
			wantGone:   []string{CodeInvalidRights},
		},
		{
			name:       "a field out of its vocabulary",
			rightsJSON: `{"version":1,"usage":{"commercial_use":"maybe"},"visibility":{"metadata":"project_policy","data_access":"restricted"}}`,
			wantCode:   rights.CodeInvalidUsageValue,
			wantField:  "rights.usage.commercial_use",
			wantGone:   []string{CodeInvalidRights},
		},
		{
			name: "data access out of its vocabulary",
			// A valid usage block, so the ONE refusal below is the data
			// access axis: a document that breaks several rules would make
			// this case about the list rather than about the field path.
			rightsJSON: `{"version":1,"usage":{"commercial_use":"unspecified","derivatives":"unspecified","redistribution":"unspecified","model_training":"unspecified","attribution":"required","patent_grant":"none"},"visibility":{"metadata":"project_policy","data_access":"openish"}}`,
			wantCode:   rights.CodeInvalidDataAccess,
			wantField:  "rights.visibility.data_access",
			wantGone:   []string{CodeInvalidRights},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, state := publicDataset(t)
			c.RightsJSON = json.RawMessage(tc.rightsJSON)

			p := mustPreview(t, c, state)

			b := blockerFor(t, p.RightsBlockers, tc.wantCode)
			if b.Field != tc.wantField {
				t.Errorf("rights blocker field = %q, want %q", b.Field, tc.wantField)
			}
			if strings.TrimSpace(b.Detail) == "" {
				t.Error("the rights blocker carries no detail")
			}
			for _, gone := range tc.wantGone {
				for _, got := range blockerCodes(p.PublishBlockers) {
					if got == gone {
						t.Errorf("publish_blockers repeats the rights refusal as %s: %v", gone, blockerCodes(p.PublishBlockers))
					}
				}
			}
			if p.Publishable {
				t.Error("publishable = true while the rights declaration is refused")
			}
			if p.Facts.Rights {
				t.Error("facts.rights = true while the rights declaration is refused")
			}
		})
	}
}

// TestPreviewAcceptsTheRightsTemplate: the other direction — a declaration
// this platform reads produces no rights blocker, so the list is a verdict
// and not a constant.
func TestPreviewAcceptsTheRightsTemplate(t *testing.T) {
	c, state := publicDataset(t)

	p := mustPreview(t, c, state)

	if len(p.RightsBlockers) != 0 {
		t.Errorf("rights_blockers = %v, want none for the template document", blockerCodes(p.RightsBlockers))
	}
	if !p.Facts.Rights {
		t.Error("facts.rights = false for a valid rights document")
	}
	if !p.Publishable {
		t.Errorf("publishable = false: %v", blockerCodes(p.PublishBlockers))
	}
}

// --- the asset, and the state's contract -------------------------------

// TestPreviewReportsTheAssetThePidNames covers the three refusals that
// need the stored asset row.
func TestPreviewReportsTheAssetThePidNames(t *testing.T) {
	cases := []struct {
		name      string
		asset     *StoredAsset
		project   string
		wantCode  string
		wantField string
	}{
		{
			name:      "the pid names no asset",
			asset:     nil,
			project:   publishingProject,
			wantCode:  CodePreviewAssetUnknown,
			wantField: "asset_pid",
		},
		{
			name: "the asset is of another type",
			asset: &StoredAsset{
				PID: samplePID, Type: TypeBenchmark, Title: "t", OriginProjectID: publishingProject,
			},
			project:   publishingProject,
			wantCode:  CodePreviewAssetTypeMismatch,
			wantField: "asset_type",
		},
		{
			name: "the asset belongs to another project",
			asset: &StoredAsset{
				PID: samplePID, Type: TypeDataset, Title: "t", OriginProjectID: foreignProject,
			},
			project:   publishingProject,
			wantCode:  CodePreviewAssetProjectMismatch,
			wantField: "project_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, state := publicDataset(t)
			state.Asset = tc.asset

			p, err := Preview(PreviewRequest{ProjectID: tc.project, Candidate: c}, state)
			if err != nil {
				t.Fatalf("Preview: %v", err)
			}

			b := blockerFor(t, p.PublishBlockers, tc.wantCode)
			if b.Field != tc.wantField {
				t.Errorf("blocker field = %q, want %q", b.Field, tc.wantField)
			}
			if p.Publishable {
				t.Error("publishable = true although the asset does not resolve")
			}
			if tc.asset == nil && p.Asset.Resolved {
				t.Error("asset.resolved = true for a pid that names no asset")
			}
			// The impact is still computed: a human deciding needs to see
			// what would have been exposed even when the publication cannot
			// execute. Everything public here, so nothing is named.
			if tc.wantCode != CodePreviewAssetUnknown && len(p.Objects) != 0 {
				t.Errorf("objects = %+v, want the impact computed anyway", p.Objects)
			}
		})
	}
}

// TestPreviewRefusesAnIncompleteState is the reader's contract: a state
// that does not answer for a canonical ref is not "the ref does not
// exist", it is a reader that dropped one — and the preview fails loudly
// rather than reporting its own reader's silence as a finding.
func TestPreviewRefusesAnIncompleteState(t *testing.T) {
	c := validCandidate(t)
	states := map[string]CurrentState{
		"no refs at all": {Asset: storedAsset(), Pins: []StoredPin{pinnedVersion(t, VisibilityPublic)}},
		"another ref": {
			Asset: storedAsset(),
			Refs: []StoredRef{{
				Ref:      OriginRef("state:" + objectVersionRowID),
				Resolved: true,
			}},
			Pins: []StoredPin{pinnedVersion(t, VisibilityPublic)},
		},
	}
	for name, state := range states {
		t.Run(name, func(t *testing.T) {
			_, err := Preview(PreviewRequest{ProjectID: publishingProject, Candidate: c}, state)
			if err == nil {
				t.Fatal("Preview accepted a state that answers for none of the candidate's refs")
			}
			if !strings.Contains(err.Error(), "release:"+sampleUUID) {
				t.Errorf("the error does not name the ref that went unanswered: %v", err)
			}
		})
	}
	// And the contract holds the other way: an invalid ref is not a missing
	// answer, because there is no entity a reader could have answered for.
	// The gate refuses its shape; the preview does not also fail.
	broken, state := publicDataset(t)
	broken.OriginRefs = []string{"release:" + sampleUUID, "not-a-ref"}
	p := mustPreview(t, broken, state)
	if ref := p.Refs[1]; ref.Resolved || ref.Kind != "" {
		t.Errorf("refs[1] = %+v, want an unresolved entry with no kind", ref)
	}
	blockerFor(t, p.PublishBlockers, CodeInvalidProvenanceRef)
}

// TestPreviewAnswersForAnUnreadableManifest pins the model's half of the
// rule the state reader depends on: a candidate whose manifest cannot be
// read is a REFUSED candidate, not an unanswerable one.
//
// The refs are the candidate's own field, so a state that answers them IS
// complete even when the manifest yielded nothing — the preview must not
// answer ErrIncompleteState, which is reserved for a reader that dropped a
// ref. The pins and the blobs really are declared in the manifest, so an
// unreadable manifest costs exactly those two lists, and the answer that
// keeps that honest is the gate's own refusal in publish_blockers with
// publishable=false: the preview never reports a document it could not read
// as a publication with nothing private in it.
//
// The OTHER half — that the real reader resolves the refs when
// ParseManifest fails, rather than returning before it looks — cannot be
// pinned from here, because this test hands Preview the state directly.
// tests/integration's TestAssetPreviewAnswersAnUnreadableManifest pins that
// half against real PostgreSQL, through the route.
func TestPreviewAnswersForAnUnreadableManifest(t *testing.T) {
	valid := validCandidate(t)
	missingKey := func(t *testing.T) []byte {
		t.Helper()
		m, err := ParseManifest(valid.Manifest)
		if err != nil {
			t.Fatalf("fixture: the valid manifest must parse: %v", err)
		}
		delete(m.Metadata, "quality_notes")
		raw, err := m.CanonicalJSON()
		if err != nil {
			t.Fatalf("fixture: canonical manifest: %v", err)
		}
		return raw
	}
	cases := map[string]struct {
		manifest []byte
		code     string
	}{
		"a required metadata key is missing": {manifest: missingKey(t), code: CodeMissingMetadata},
		"the document is empty":              {manifest: []byte(`{}`), code: CodeUnsupportedManifestVersion},
		"the document is not JSON":           {manifest: []byte(`not a manifest`), code: CodeMalformedManifest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := valid
			c.Manifest = tc.manifest
			// The reader's answer for this candidate, exactly as
			// PostgresStateStore gives it: the asset and the refs are
			// resolved, and the pins and the blobs — which come from the
			// manifest — are not there to resolve.
			state := CurrentState{Asset: storedAsset(), Refs: answeredRelease(publishingProject, VisibilityPublic)}

			p, err := Preview(PreviewRequest{ProjectID: publishingProject, Candidate: c}, state)
			if err != nil {
				t.Fatalf("Preview refused to answer a candidate whose manifest cannot be read: %v "+
					"(this is the reader's contract, and the refs here ARE answered)", err)
			}
			if len(p.Refs) != 1 || !p.Refs[0].Resolved ||
				p.Refs[0].CurrentVisibility != string(VisibilityPublic) {
				t.Errorf("refs = %+v, want the candidate's own ref resolved and reported as usual", p.Refs)
			}
			blockerFor(t, p.PublishBlockers, tc.code)
			if p.Publishable {
				t.Error("publishable = true for a candidate whose manifest cannot be read; the refusal is " +
					"what keeps the unread pins and blobs from reading as 'nothing private here'")
			}
			if len(p.Dependencies) != 0 || len(p.Blobs) != 0 {
				t.Errorf("dependencies = %+v, blobs = %+v, want both empty: the document that would have "+
					"declared them could not be read", p.Dependencies, p.Blobs)
			}
		})
	}
}

// --- fixtures ----------------------------------------------------------

// candidateWithBlobs returns the candidate with its manifest declaring the
// given blob ids.
func candidateWithBlobs(t *testing.T, c PublishCandidate, blobIDs ...string) PublishCandidate {
	t.Helper()
	ids := make([]string, 0, len(blobIDs))
	ids = append(ids, blobIDs...)
	return candidateWithMetadata(t, c, map[string]any{"blob_ids": ids})
}

// candidateWithMetadata returns the candidate with the given manifest
// metadata keys replaced AND its integrity hash recomputed.
//
// The recomputation is the point: the gate requires the hash to cover the
// manifest (docs/11 §3), so a test that rewrote the manifest and kept the
// old hash would be testing the gate's refusal instead of the preview —
// and would pass for the wrong reason. The canonical bytes are parsed,
// re-rendered and re-hashed exactly the way the publish path does it.
func candidateWithMetadata(t *testing.T, c PublishCandidate, values map[string]any) PublishCandidate {
	t.Helper()
	m, err := ParseManifest(c.Manifest)
	if err != nil {
		t.Fatalf("fixture: the manifest must parse: %v", err)
	}
	if m.Metadata == nil {
		m.Metadata = Metadata{}
	}
	for k, v := range values {
		m.Metadata[k] = v
	}
	raw, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("fixture: canonical manifest: %v", err)
	}
	hash, err := m.Hash()
	if err != nil {
		t.Fatalf("fixture: manifest hash: %v", err)
	}
	c.Manifest = raw
	c.IntegrityHash = hash
	return c
}

// rightsJSONWithDataAccess renders the rights template with the data
// access axis set, so a case can state which document promises openness.
func rightsJSONWithDataAccess(t *testing.T, access rights.DataAccess) json.RawMessage {
	t.Helper()
	d := rights.New()
	d.Visibility.DataAccess = access
	raw, err := d.Marshal()
	if err != nil {
		t.Fatalf("fixture: marshal rights document: %v", err)
	}
	return raw
}
