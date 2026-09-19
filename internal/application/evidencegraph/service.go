package evidencegraph

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/evidence"
)

// ErrStore marks a persistence failure (the transport answers 503). It is
// this package's own sentinel: the not-found outcomes are the sciobjects
// sentinels every read surface already answers with, so the wire codes stay
// one vocabulary.
var ErrStore = errors.New("evidencegraph: store failure")

// Object types this read names by hand. Both are the canonical
// scientific_object type names the schema registry holds (docs/08).
const (
	// ObjectTypeClaim: a claim is a proposition that can be judged on its
	// own (docs/08 §Claim).
	ObjectTypeClaim = "claim"
	// ObjectTypeHypothesis: the object the two-section page is about.
	ObjectTypeHypothesis = "hypothesis"
)

// SubordinateRelationType is the relation that makes a claim one of a
// hypothesis's claims: "tests the target hypothesis" (docs/44's knowledge
// catalog, internal/rsg/relationcatalog), whose TARGET endpoint is pinned to
// a hypothesis by migration 00040. docs/08's Hypothesis entry states the
// same shape: a hypothesis is never "converted into" a claim, a later claim
// establishes a historical relation to it (tested_by/supports).
//
// It is the ONLY edge this read treats as subordination, and the section it
// feeds is the only place it is consulted. An object that tests a
// hypothesis WITHOUT being a claim (the endpoint-type guard deliberately
// leaves the source end unconstrained) is not "one of its claims" and does
// not appear in that section — the section's name is its contract, and it
// does not silently become a list of everything that mentions the
// hypothesis.
const SubordinateRelationType = "tests_hypothesis"

// Service is the read service. It holds no state beyond its adapters.
type Service struct {
	objects    ObjectStore
	assertions AssertionStore
	relations  RelationStore
}

// New wires the service.
func New(deps Deps) *Service {
	return &Service{objects: deps.Objects, assertions: deps.Assertions, relations: deps.Relations}
}

// ObjectEvidence reads one object's evidence, grouped by the target version
// each assertion pins.
//
// versionNo nil answers for the whole object: one group per version that
// carries at least one assertion, in version order (an inventory — a version
// with no evidence adds an empty group and no information). A pinned
// versionNo answers for that version ALONE, and its group is returned even
// when it carries no assertions: the caller asked a direct question about
// one version and "no evidence on this version" is its answer, not an empty
// document.
//
// The object is resolved first and its owning project is compared with the
// path project (case-insensitively — a uuid spelled in another case names
// the same row, and membership must not depend on the caller's spelling): a
// foreign object answers exactly what a nonexistent one answers
// (sciobjects.ErrObjectNotFound), so the read is no existence oracle for
// objects outside the path project.
func (s *Service) ObjectEvidence(ctx context.Context, projectID, objectID string, versionNo *int) (evidence.ObjectEvidence, error) {
	obj, err := s.object(ctx, projectID, objectID)
	if err != nil {
		return evidence.ObjectEvidence{}, err
	}
	versions, err := s.versions(ctx, objectID)
	if err != nil {
		return evidence.ObjectEvidence{}, err
	}
	selected := versions
	if versionNo != nil {
		v, ok := versionWithNo(versions, *versionNo)
		if !ok {
			return evidence.ObjectEvidence{}, sciobjects.ErrVersionNotFound
		}
		selected = []domain.ScientificObjectVersion{v}
	}
	groups, err := s.groups(ctx, selected, versionNo != nil)
	if err != nil {
		return evidence.ObjectEvidence{}, err
	}
	return evidence.ObjectEvidence{
		ProjectID: projectID,
		Object:    objectRef(obj, versions),
		Groups:    groups,
	}, nil
}

// HypothesisEvidence reads the hypothesis page: TWO SECTIONS, never merged.
//
//   - Direct: the assertions pinned to the hypothesis's own versions,
//     grouped by version — the same shape (and the same read) as any other
//     object's evidence.
//   - Claims: the hypothesis's subordinate claims (SubordinateRelationType),
//     each carrying its own assertions grouped by the claim version they
//     pin. Every subordinate claim appears, with an empty evidence list when
//     it carries none — the claim list is complete; the evidence lists say
//     what there is.
//
// No total, no ratio, no net position is computed across the two sections,
// and none across the claims inside the second one.
//
// The path object must be a hypothesis: any other type answers the same
// not-found an unknown object answers (the route's name is its contract; a
// claim's own evidence is on the object read).
func (s *Service) HypothesisEvidence(ctx context.Context, projectID, objectID string) (evidence.HypothesisEvidence, error) {
	obj, err := s.object(ctx, projectID, objectID)
	if err != nil {
		return evidence.HypothesisEvidence{}, err
	}
	if obj.ObjectType != ObjectTypeHypothesis {
		return evidence.HypothesisEvidence{}, sciobjects.ErrObjectNotFound
	}
	versions, err := s.versions(ctx, objectID)
	if err != nil {
		return evidence.HypothesisEvidence{}, err
	}
	direct, err := s.groups(ctx, versions, false)
	if err != nil {
		return evidence.HypothesisEvidence{}, err
	}
	rels, err := s.relations.ListVersionsForObject(ctx, projectID, objectID)
	if err != nil {
		return evidence.HypothesisEvidence{}, fmt.Errorf("%w: list relations of %s: %v", ErrStore, objectID, err)
	}
	claims := subordinateClaims(rels, objectID)
	out := evidence.HypothesisEvidence{
		ProjectID:  projectID,
		Hypothesis: objectRef(obj, versions),
		Direct:     direct,
		Claims:     make([]evidence.ClaimEvidence, 0, len(claims)),
	}
	for _, c := range claims {
		cv, err := s.versions(ctx, c.ObjectID)
		if err != nil {
			return evidence.HypothesisEvidence{}, err
		}
		groups, err := s.groups(ctx, cv, false)
		if err != nil {
			return evidence.HypothesisEvidence{}, err
		}
		out.Claims = append(out.Claims, evidence.ClaimEvidence{Claim: c, Evidence: groups})
	}
	return out, nil
}

// groups reads every version's assertions and assembles the groups.
//
// One query per version, through the ONE pre-provisioned per-target read:
// the version log of a single object is small, and reusing the query the
// schema already carries beats a new one (and a new index) for a read that
// would save no round trips anyone can feel. includeEmpty decides whether a
// version carrying nothing still gets its group (see the two read comments
// above for which read wants which).
func (s *Service) groups(ctx context.Context, versions []domain.ScientificObjectVersion, includeEmpty bool) ([]evidence.TargetGroup, error) {
	targets := make([]evidence.Target, 0, len(versions))
	rows := make([]evidence.Assertion, 0)
	for _, v := range versions {
		assertions, err := s.assertions.ListForTargetVersion(ctx, v.ID)
		if err != nil {
			return nil, fmt.Errorf("%w: list assertions for version %s: %v", ErrStore, v.ID, err)
		}
		if len(assertions) == 0 && !includeEmpty {
			continue
		}
		targets = append(targets, targetOf(v))
		rows = append(rows, assertions...)
	}
	return evidence.GroupAll(targets, rows), nil
}

// object resolves one object inside the path project, mapping every failure
// the way this read's callers answer it.
func (s *Service) object(ctx context.Context, projectID, objectID string) (domain.ScientificObject, error) {
	obj, err := s.objects.GetObject(ctx, objectID)
	if err != nil {
		if errors.Is(err, sciobjects.ErrObjectNotFound) {
			return domain.ScientificObject{}, sciobjects.ErrObjectNotFound
		}
		return domain.ScientificObject{}, fmt.Errorf("%w: get object %s: %v", ErrStore, objectID, err)
	}
	if !strings.EqualFold(obj.ProjectID, projectID) {
		// A foreign object under any project prefix answers what a
		// nonexistent object answers — for every version, and whatever the
		// query string says (docs/45: no existence oracle).
		return domain.ScientificObject{}, sciobjects.ErrObjectNotFound
	}
	return obj, nil
}

// versions reads the object's version log. An object with no versions is
// impossible through the write surface (every object is created with its
// first version in one transaction), so an empty log is a broken read and
// fails closed as not-found rather than rendering an empty page.
func (s *Service) versions(ctx context.Context, objectID string) ([]domain.ScientificObjectVersion, error) {
	versions, err := s.objects.ListVersions(ctx, objectID)
	if err != nil {
		return nil, fmt.Errorf("%w: list versions of %s: %v", ErrStore, objectID, err)
	}
	if len(versions) == 0 {
		return nil, sciobjects.ErrObjectNotFound
	}
	return versions, nil
}

// subordinateClaims selects the claims one hypothesis's second section
// lists, from the relation versions whose source or target pins a version of
// the hypothesis.
//
// A row qualifies when its type is SubordinateRelationType, its SOURCE is a
// claim and its TARGET object is the hypothesis. Rows arrive newest first,
// so the first row per relation id is the relation's current head — a
// superseded edge does not resurrect an old title, and a claim that appears
// in several rows (several edges, or several versions of one) is listed
// once. The result is sorted by object id, so one database state renders one
// document.
func subordinateClaims(rels []rsg.ObjectRelationVersion, hypothesisObjectID string) []evidence.ClaimRef {
	seenRelation := make(map[string]bool, len(rels))
	seenClaim := make(map[string]bool, len(rels))
	out := make([]evidence.ClaimRef, 0, len(rels))
	for _, rel := range rels {
		if rel.Relation.RelationType != SubordinateRelationType {
			continue
		}
		if rel.Source.ObjectType != ObjectTypeClaim {
			continue
		}
		if !strings.EqualFold(rel.Target.ObjectID, hypothesisObjectID) {
			continue
		}
		if seenRelation[rel.Relation.RelationID] {
			continue
		}
		seenRelation[rel.Relation.RelationID] = true
		if seenClaim[rel.Source.ObjectID] {
			continue
		}
		seenClaim[rel.Source.ObjectID] = true
		out = append(out, evidence.ClaimRef{
			ObjectID:   rel.Source.ObjectID,
			ObjectType: rel.Source.ObjectType,
			Title:      rel.Source.Title,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObjectID < out[j].ObjectID })
	return out
}

// targetOf renders one version as the pin a group is keyed by.
func targetOf(v domain.ScientificObjectVersion) evidence.Target {
	no := v.VersionNo
	return evidence.Target{ObjectVersionID: v.ID, VersionNo: &no, Title: v.Title}
}

// objectRef renders one object for the wire from its FULL version log. The
// title is the newest version's — the object's display title — even when the
// read itself is pinned to an older version: the ref names the object, and
// the pin it was read for is named by the group's target. An object whose
// log the caller could not read keeps an empty title rather than an invented
// one.
func objectRef(obj domain.ScientificObject, versions []domain.ScientificObjectVersion) evidence.ObjectRef {
	ref := evidence.ObjectRef{ObjectID: obj.ID, ObjectType: obj.ObjectType}
	newest := 0
	for _, v := range versions {
		if v.VersionNo > newest {
			newest = v.VersionNo
			ref.Title = v.Title
		}
	}
	return ref
}

// versionWithNo finds one version of a log by its number (the log arrives
// oldest first, but this does not rely on it).
func versionWithNo(versions []domain.ScientificObjectVersion, versionNo int) (domain.ScientificObjectVersion, bool) {
	for _, v := range versions {
		if v.VersionNo == versionNo {
			return v, true
		}
	}
	return domain.ScientificObjectVersion{}, false
}
