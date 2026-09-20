package evidencegraph

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/evidence"
)

// Task T0506 — the read service's own decisions, pinned with fakes: which
// object resolves, which versions are asked for, and which rows the
// hypothesis's second section may contain. The projection's bucketing rules
// are pinned in internal/evidence; these tests are about the service's
// selection, not about repeating that.

const projID = "11111111-1111-4111-8111-111111111111"

// anon is the zero reader — the anonymous caller the transports build when no
// session resolved. Most of the cases below are about the service's
// SELECTION (which object, which versions, which sections), and the audience
// has its own test at the bottom of this file.
var anon = projects.Reader{}

// member is a resolved caller: the identity every audience decision is made
// about.
var member = projects.Reader{UserID: "33333333-3333-4333-8333-333333333333", Authenticated: true}

type fakeObjects struct {
	objects  map[string]domain.ScientificObject
	versions map[string][]domain.ScientificObjectVersion
	calls    []string
	err      error
}

func (f *fakeObjects) GetObject(_ context.Context, objectID string) (domain.ScientificObject, error) {
	f.calls = append(f.calls, "GetObject:"+objectID)
	if f.err != nil {
		return domain.ScientificObject{}, f.err
	}
	obj, ok := f.objects[objectID]
	if !ok {
		return domain.ScientificObject{}, sciobjects.ErrObjectNotFound
	}
	return obj, nil
}

func (f *fakeObjects) ListVersions(_ context.Context, objectID string) ([]domain.ScientificObjectVersion, error) {
	f.calls = append(f.calls, "ListVersions:"+objectID)
	if f.err != nil {
		return nil, f.err
	}
	return f.versions[objectID], nil
}

type fakeAssertions struct {
	rows map[string][]evidence.Assertion
	// asked records every version id the service read assertions for, in
	// order — the "which versions were queried" evidence.
	asked []string
	// readers records the reader of every call, in order — ADR-024's
	// evidence at this layer: the reader is an input of the READ, so no
	// query may be issued without one.
	readers []string
	err     error
}

func (f *fakeAssertions) ListForTargetVersion(_ context.Context, versionID, readerUserID string) ([]evidence.Assertion, error) {
	f.asked = append(f.asked, versionID)
	f.readers = append(f.readers, readerUserID)
	if f.err != nil {
		return nil, f.err
	}
	return f.rows[versionID], nil
}

type fakeRelations struct {
	rows []rsg.ObjectRelationVersion
	err  error
}

func (f *fakeRelations) ListVersionsForObject(_ context.Context, _, _ string) ([]rsg.ObjectRelationVersion, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.rows, nil
}

func version(id string, no int, title string) domain.ScientificObjectVersion {
	return domain.ScientificObjectVersion{ID: id, ObjectID: "obj", VersionNo: no, Title: title}
}

func assertion(id, targetVersionID, relation string) evidence.Assertion {
	return evidence.Assertion{
		ID: id, Relation: relation, EvidenceType: "experimental",
		Scope: json.RawMessage(`{}`), TargetObjectVersionID: targetVersionID,
	}
}

func relation(id, relationID, relType, sourceID, sourceType, targetID string) rsg.ObjectRelationVersion {
	return rsg.ObjectRelationVersion{
		Relation: domain.RelationVersion{ID: id, RelationID: relationID, RelationType: relType},
		Source:   rsg.ObjectRelationEndpoint{ObjectID: sourceID, ObjectType: sourceType, Title: "src"},
		Target:   rsg.ObjectRelationEndpoint{ObjectID: targetID, ObjectType: "hypothesis", Title: "hyp"},
	}
}

func newService(objs *fakeObjects, asserts *fakeAssertions, rels *fakeRelations) *Service {
	return New(Deps{Objects: objs, Assertions: asserts, Relations: rels})
}

func groupIDs(groups []evidence.TargetGroup) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.Target.ObjectVersionID)
	}
	return out
}

// TestObjectEvidenceUnpinnedAsksEveryVersionAndKeepsOnlyTheOccupied pins the
// two halves of the unpinned read: every version of the object is queried
// (nothing is skipped by assumption), and only the ones carrying assertions
// reach the document (an inventory of evidence, not of versions).
func TestObjectEvidenceUnpinnedAsksEveryVersionAndKeepsOnlyTheOccupied(t *testing.T) {
	objs := &fakeObjects{
		objects: map[string]domain.ScientificObject{"obj": {ID: "obj", ObjectType: "claim", ProjectID: projID}},
		versions: map[string][]domain.ScientificObjectVersion{"obj": {
			version("v1", 1, "first"), version("v2", 2, "second"), version("v3", 3, "third"),
		}},
	}
	asserts := &fakeAssertions{rows: map[string][]evidence.Assertion{
		"v1": {assertion("a1", "v1", "supports")},
		"v3": {assertion("a3", "v3", "contradicts")},
	}}
	svc := newService(objs, asserts, &fakeRelations{})

	out, err := svc.ObjectEvidence(context.Background(), anon, projID, "obj", nil)
	if err != nil {
		t.Fatalf("ObjectEvidence: %v", err)
	}
	if got := groupIDs(out.Groups); len(got) != 2 || got[0] != "v1" || got[1] != "v3" {
		t.Errorf("groups = %v, want [v1 v3] (v2 carries nothing and is not an inventory entry)", got)
	}
	if len(asserts.asked) != 3 {
		t.Errorf("versions queried = %v, want all three", asserts.asked)
	}
	if out.Object.Title != "third" {
		t.Errorf("object title = %q, want the newest version's title", out.Object.Title)
	}
}

// TestObjectEvidencePinnedAsksOneVersionAndAnswersEvenWhenEmpty — a pinned
// question gets its group whether or not there is anything in it, and the
// other versions are not queried (the read is about one version).
func TestObjectEvidencePinnedAsksOneVersionAndAnswersEvenWhenEmpty(t *testing.T) {
	objs := &fakeObjects{
		objects: map[string]domain.ScientificObject{"obj": {ID: "obj", ObjectType: "claim", ProjectID: projID}},
		versions: map[string][]domain.ScientificObjectVersion{"obj": {
			version("v1", 1, "first"), version("v2", 2, "second"),
		}},
	}
	asserts := &fakeAssertions{rows: map[string][]evidence.Assertion{}}
	svc := newService(objs, asserts, &fakeRelations{})

	no := 1
	out, err := svc.ObjectEvidence(context.Background(), anon, projID, "obj", &no)
	if err != nil {
		t.Fatalf("ObjectEvidence: %v", err)
	}
	if got := groupIDs(out.Groups); len(got) != 1 || got[0] != "v1" {
		t.Fatalf("groups = %v, want exactly [v1] even with no assertions", got)
	}
	if len(asserts.asked) != 1 || asserts.asked[0] != "v1" {
		t.Errorf("versions queried = %v, want [v1] only", asserts.asked)
	}
	if out.Object.Title != "second" {
		t.Errorf("object title = %q: the ref names the object, not the pinned version", out.Object.Title)
	}
	if out.Groups[0].Supporting == nil || out.Groups[0].Contesting == nil {
		t.Errorf("the empty group rendered null buckets: %+v", out.Groups[0])
	}
}

// TestObjectEvidenceRefusesAForeignObjectAndAnUnknownVersion — the read is
// no existence oracle: an object of another project and an object that does
// not exist answer the same sentinel, and a version number the log does not
// contain answers the version sentinel rather than an empty page.
func TestObjectEvidenceRefusesAForeignObjectAndAnUnknownVersion(t *testing.T) {
	objs := &fakeObjects{
		objects: map[string]domain.ScientificObject{
			"obj":    {ID: "obj", ObjectType: "claim", ProjectID: projID},
			"theirs": {ID: "theirs", ObjectType: "claim", ProjectID: "22222222-2222-4222-8222-222222222222"},
		},
		versions: map[string][]domain.ScientificObjectVersion{"theirs": {version("x1", 1, "x")}, "obj": {version("v1", 1, "first")}},
	}
	svc := newService(objs, &fakeAssertions{}, &fakeRelations{})

	if _, err := svc.ObjectEvidence(context.Background(), anon, projID, "theirs", nil); !errors.Is(err, sciobjects.ErrObjectNotFound) {
		t.Errorf("foreign object error = %v, want ErrObjectNotFound", err)
	}
	if _, err := svc.ObjectEvidence(context.Background(), anon, projID, "missing", nil); !errors.Is(err, sciobjects.ErrObjectNotFound) {
		t.Errorf("unknown object error = %v, want ErrObjectNotFound", err)
	}
	no := 99
	if _, err := svc.ObjectEvidence(context.Background(), anon, projID, "obj", &no); !errors.Is(err, sciobjects.ErrVersionNotFound) {
		t.Errorf("unknown version error = %v, want ErrVersionNotFound", err)
	}
}

// TestHypothesisEvidenceKeepsTheTwoSectionsApart is the ruling's own test at
// this layer: the hypothesis's own assertions land in Direct, a subordinate
// claim's assertions land only under that claim, and the claim list carries
// a claim with no evidence rather than omitting it.
func TestHypothesisEvidenceKeepsTheTwoSectionsApart(t *testing.T) {
	objs := &fakeObjects{
		objects: map[string]domain.ScientificObject{
			"hyp": {ID: "hyp", ObjectType: "hypothesis", ProjectID: projID},
			"c1":  {ID: "c1", ObjectType: "claim", ProjectID: projID},
			"c2":  {ID: "c2", ObjectType: "claim", ProjectID: projID},
		},
		versions: map[string][]domain.ScientificObjectVersion{
			"hyp": {version("h1", 1, "hypothesis v1")},
			"c1":  {version("c1v1", 1, "claim one")},
			"c2":  {version("c2v1", 1, "claim two")},
		},
	}
	asserts := &fakeAssertions{rows: map[string][]evidence.Assertion{
		"h1":   {assertion("direct-support", "h1", "supports")},
		"c1v1": {assertion("claim-contra", "c1v1", "contradicts")},
	}}
	rels := &fakeRelations{rows: []rsg.ObjectRelationVersion{
		relation("r2", "rel-c2", SubordinateRelationType, "c2", ObjectTypeClaim, "hyp"),
		relation("r1", "rel-c1", SubordinateRelationType, "c1", ObjectTypeClaim, "hyp"),
	}}
	svc := newService(objs, asserts, rels)

	out, err := svc.HypothesisEvidence(context.Background(), anon, projID, "hyp")
	if err != nil {
		t.Fatalf("HypothesisEvidence: %v", err)
	}
	if got := groupIDs(out.Direct); len(got) != 1 || got[0] != "h1" {
		t.Fatalf("direct sections = %v, want [h1]", got)
	}
	if len(out.Direct[0].Supporting) != 1 || out.Direct[0].Supporting[0].ID != "direct-support" {
		t.Fatalf("direct supporting = %+v", out.Direct[0].Supporting)
	}
	if len(out.Claims) != 2 {
		t.Fatalf("claims = %d, want both subordinate claims", len(out.Claims))
	}
	// Sorted by object id: c1 before c2, whatever order the store returned.
	if out.Claims[0].Claim.ObjectID != "c1" || out.Claims[1].Claim.ObjectID != "c2" {
		t.Fatalf("claim order = %s, %s, want c1, c2", out.Claims[0].Claim.ObjectID, out.Claims[1].Claim.ObjectID)
	}
	first := out.Claims[0]
	if len(first.Evidence) != 1 || first.Evidence[0].Target.ObjectVersionID != "c1v1" {
		t.Fatalf("claim c1 evidence = %+v, want its own version group", first.Evidence)
	}
	if len(first.Evidence[0].Contesting) != 1 || first.Evidence[0].Contesting[0].ID != "claim-contra" {
		t.Fatalf("claim c1 contesting = %+v", first.Evidence[0].Contesting)
	}
	// The separation itself: nothing pinned to the hypothesis's own version
	// may appear in the claims section, and the claim's assertion may not
	// appear under Direct.
	for _, g := range first.Evidence {
		for _, bucket := range [][]evidence.Assertion{g.Supporting, g.Contesting, g.Neutral, g.Unlabeled} {
			for _, row := range bucket {
				if row.ID == "direct-support" {
					t.Errorf("the hypothesis's own assertion leaked into a claim's evidence")
				}
			}
		}
	}
	for _, bucket := range [][]evidence.Assertion{out.Direct[0].Supporting, out.Direct[0].Contesting,
		out.Direct[0].Neutral, out.Direct[0].Unlabeled} {
		for _, row := range bucket {
			if row.ID == "claim-contra" {
				t.Errorf("a subordinate claim's assertion appeared in the hypothesis's own section")
			}
		}
	}
	// A claim carrying nothing is listed with an empty list, not dropped.
	if out.Claims[1].Evidence == nil {
		t.Errorf("a claim with no evidence rendered null instead of an empty list")
	}
}

// TestHypothesisEvidenceRefusesOtherObjectTypes — the route's name is its
// contract: a claim asked for as a hypothesis answers what an unknown object
// answers.
func TestHypothesisEvidenceRefusesOtherObjectTypes(t *testing.T) {
	objs := &fakeObjects{
		objects: map[string]domain.ScientificObject{
			"c1": {ID: "c1", ObjectType: "claim", ProjectID: projID},
		},
		versions: map[string][]domain.ScientificObjectVersion{"c1": {version("c1v1", 1, "claim one")}},
	}
	svc := newService(objs, &fakeAssertions{}, &fakeRelations{})
	if _, err := svc.HypothesisEvidence(context.Background(), anon, projID, "c1"); !errors.Is(err, sciobjects.ErrObjectNotFound) {
		t.Errorf("claim asked for as hypothesis: err = %v, want ErrObjectNotFound", err)
	}
}

// TestSubordinateClaimsSelectsOnlyClaimSourcesTargetingTheHypothesis pins
// the selection rule the section's name promises: only SubordinateRelationType
// edges, only claim sources, only edges targeting THIS hypothesis; one row
// per edge (the newest, since the store returns newest first) and one entry
// per claim.
func TestSubordinateClaimsSelectsOnlyClaimSourcesTargetingTheHypothesis(t *testing.T) {
	rels := []rsg.ObjectRelationVersion{
		// Newest first, as the port contracts.
		relation("rv2", "rel-c1", SubordinateRelationType, "c1", ObjectTypeClaim, "hyp"),
		relation("rv1", "rel-c1", SubordinateRelationType, "c1", ObjectTypeClaim, "hyp"),
		relation("rv3", "rel-other", SubordinateRelationType, "c3", ObjectTypeClaim, "OTHER-HYP"),
		relation("rv4", "rel-protocol", SubordinateRelationType, "p1", "protocol", "hyp"),
		relation("rv5", "rel-supports", "supports", "c4", ObjectTypeClaim, "hyp"),
		relation("rv6", "rel-c2", SubordinateRelationType, "c2", ObjectTypeClaim, "HYP"),
	}
	got := subordinateClaims(rels, "hyp")
	if len(got) != 2 {
		t.Fatalf("claims = %+v, want c1 and c2 only", got)
	}
	if got[0].ObjectID != "c1" || got[1].ObjectID != "c2" {
		t.Errorf("claims = %s, %s; want c1, c2", got[0].ObjectID, got[1].ObjectID)
	}
	if got[0].Title != "src" {
		t.Errorf("claim title = %q: the endpoint label must be carried through", got[0].Title)
	}
}

// TestStoreFailuresBecomeErrStore — an adapter failure is never rendered as
// an empty page, and each read reports it.
func TestStoreFailuresBecomeErrStore(t *testing.T) {
	boom := errors.New("connection reset")
	objs := &fakeObjects{
		objects:  map[string]domain.ScientificObject{"hyp": {ID: "hyp", ObjectType: "hypothesis", ProjectID: projID}},
		versions: map[string][]domain.ScientificObjectVersion{"hyp": {version("h1", 1, "h")}},
		err:      boom,
	}
	svc := newService(objs, &fakeAssertions{err: boom}, &fakeRelations{err: boom})
	if _, err := svc.HypothesisEvidence(context.Background(), anon, projID, "hyp"); !errors.Is(err, ErrStore) {
		t.Errorf("object store failure: err = %v, want ErrStore", err)
	}

	objs = &fakeObjects{
		objects:  map[string]domain.ScientificObject{"hyp": {ID: "hyp", ObjectType: "hypothesis", ProjectID: projID}},
		versions: map[string][]domain.ScientificObjectVersion{"hyp": {version("h1", 1, "h")}},
	}
	svc = newService(objs, &fakeAssertions{err: boom}, &fakeRelations{})
	if _, err := svc.HypothesisEvidence(context.Background(), anon, projID, "hyp"); !errors.Is(err, ErrStore) {
		t.Errorf("assertion store failure: err = %v, want ErrStore", err)
	}
	svc = newService(objs, &fakeAssertions{}, &fakeRelations{err: boom})
	if _, err := svc.HypothesisEvidence(context.Background(), anon, projID, "hyp"); !errors.Is(err, ErrStore) {
		t.Errorf("relation store failure: err = %v, want ErrStore", err)
	}
}

// TestEveryAssertionReadCarriesTheReader (ADR-024) — the reader is an INPUT
// of the read, so there is no path through this service that reads assertions
// for a caller it was not given. The probe is the reader the store observes:
// a service that dropped it (or re-derived it, or defaulted it) would show up
// here as an empty or foreign user id, whatever it did with the rows.
//
// It covers BOTH reads and every query each one issues, because "the reader
// reaches the store" is only true if it reaches it on the subordinate claims'
// versions too — the hypothesis page's second section reads the same way the
// first one does.
func TestEveryAssertionReadCarriesTheReader(t *testing.T) {
	objs := &fakeObjects{
		objects: map[string]domain.ScientificObject{
			"obj": {ID: "obj", ObjectType: "claim", ProjectID: projID},
			"hyp": {ID: "hyp", ObjectType: "hypothesis", ProjectID: projID},
			"c1":  {ID: "c1", ObjectType: "claim", ProjectID: projID},
		},
		versions: map[string][]domain.ScientificObjectVersion{
			"obj": {version("v1", 1, "first"), version("v2", 2, "second")},
			"hyp": {version("h1", 1, "hypothesis v1")},
			"c1":  {version("c1v1", 1, "claim one")},
		},
	}
	rels := &fakeRelations{rows: []rsg.ObjectRelationVersion{
		relation("r1", "rel-c1", SubordinateRelationType, "c1", ObjectTypeClaim, "hyp"),
	}}

	// The object read: the unpinned form asks every version of the object, so
	// every one of those queries must carry the reader.
	asserts := &fakeAssertions{}
	svc := newService(objs, asserts, rels)
	if _, err := svc.ObjectEvidence(context.Background(), member, projID, "obj", nil); err != nil {
		t.Fatalf("ObjectEvidence: %v", err)
	}
	if len(asserts.readers) != 2 {
		t.Fatalf("assertion reads = %d, want one per version of the object", len(asserts.readers))
	}
	for i, got := range asserts.readers {
		if got != member.UserID {
			t.Errorf("read %d carried reader %q, want %q", i, got, member.UserID)
		}
	}

	// The hypothesis page: the direct section AND each subordinate claim's
	// version go through the same read, so both must carry it.
	asserts = &fakeAssertions{}
	svc = newService(objs, asserts, rels)
	if _, err := svc.HypothesisEvidence(context.Background(), member, projID, "hyp"); err != nil {
		t.Fatalf("HypothesisEvidence: %v", err)
	}
	if len(asserts.readers) != 2 {
		t.Fatalf("assertion reads = %d, want one for the hypothesis and one for its claim", len(asserts.readers))
	}
	for i, got := range asserts.readers {
		if got != member.UserID {
			t.Errorf("hypothesis-page read %d carried reader %q, want %q", i, got, member.UserID)
		}
	}

	// And an anonymous caller is carried as the empty user id rather than
	// dropped altogether: the store has to be asked the question, because
	// "which rows may this caller see" for an anonymous caller is still the
	// read's question (it is answered by the public predicate, not by
	// skipping the read).
	asserts = &fakeAssertions{}
	svc = newService(objs, asserts, rels)
	if _, err := svc.ObjectEvidence(context.Background(), anon, projID, "obj", nil); err != nil {
		t.Fatalf("ObjectEvidence (anonymous): %v", err)
	}
	for i, got := range asserts.readers {
		if got != "" {
			t.Errorf("anonymous read %d carried reader %q, want the empty user id", i, got)
		}
	}
}
