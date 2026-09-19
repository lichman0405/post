package rsg

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	"github.com/lichman0405/post/internal/rsg/semantics"
)

// The evidence-assertion write command's own tests (T0806): the rules that
// are decided in Go — which class an assertion lands in, the two DERIVED
// axes, the refusals that all answer one existence-hiding outcome, and the
// event a cross-project assertion emits. What the command does to the
// database (the row, the commit, the unicity) is pinned by the integration
// suite, which runs it against PostgreSQL through the real product path.

const (
	evTargetVer    = "33333333-3333-4333-8333-333333333333"
	evEvidenceVer  = "44444444-4444-4444-8444-444444444444"
	evOtherProject = "22222222-2222-4222-8222-222222222222"
)

// fakeEvidence is the evidence port: the facts the command reads to make its
// decision (a version's owning project and the publication it carries), and
// the row write it performs inside the commit.
type fakeEvidence struct {
	// facts maps a version id to its project facts.
	facts map[string]VersionProjectFacts
	// factErr is answered for a version the map does not hold (default:
	// sciobjects.ErrVersionNotFound).
	factErr error
	// publications maps a version id to its publication.
	publications map[string]KnowledgePublicationFacts
	// pubErr is a store failure for the publication read.
	pubErr error
	// writeErr fails the row write (the commit must roll back with it).
	writeErr error
	// writeCalls counts row writes that were reached.
	writeCalls int
	// gotWrite is the last row handed to the store.
	gotWrite CreateEvidenceAssertionInTxParams
}

func (f *fakeEvidence) CreateEvidenceAssertionInTx(ctx context.Context, tx states.Transaction, in CreateEvidenceAssertionInTxParams) (EvidenceAssertionRow, error) {
	f.writeCalls++
	f.gotWrite = in
	if f.writeErr != nil {
		return EvidenceAssertionRow{}, f.writeErr
	}
	// The row the database would hand back: the derived axes as stored, the
	// review state the column defaulted to, and the pre-generated id.
	return EvidenceAssertionRow{
		ID:                      in.ID,
		ProjectID:               in.ProjectID,
		StateID:                 in.StateID,
		TargetObjectVersionID:   in.TargetObjectVersionID,
		EvidenceObjectVersionID: in.EvidenceObjectVersionID,
		RelationType:            in.RelationType,
		EvidenceType:            in.EvidenceType,
		Scope:                   in.Scope,
		Directness:              in.Directness,
		InferenceNature:         in.InferenceNature,
		ReasoningNote:           in.ReasoningNote,
		ReviewState:             string(domain.EvidenceReviewUnreviewed),
		EvidenceOrigin:          in.EvidenceOrigin,
		Visibility:              in.Visibility,
		CreatedBy:               in.CreatedBy,
	}, nil
}

func (f *fakeEvidence) GetVersionProjectFacts(ctx context.Context, objectVersionID string) (VersionProjectFacts, error) {
	if facts, ok := f.facts[objectVersionID]; ok {
		return facts, nil
	}
	if f.factErr != nil {
		return VersionProjectFacts{}, f.factErr
	}
	return VersionProjectFacts{}, sciobjects.ErrVersionNotFound
}

func (f *fakeEvidence) GetKnowledgePublicationForVersion(ctx context.Context, objectVersionID string) (KnowledgePublicationFacts, bool, error) {
	if f.pubErr != nil {
		return KnowledgePublicationFacts{}, false, f.pubErr
	}
	pub, ok := f.publications[objectVersionID]
	return pub, ok, nil
}

// networkedEvidence is the common world: project-1 owns both versions
// (an assertion from project-1 is origin evidence), the target carries a
// network-visible publication, and the cited version is an ordinary public
// version.
func networkedEvidence() *fakeEvidence {
	return &fakeEvidence{
		facts: map[string]VersionProjectFacts{
			evTargetVer:   {ObjectVersionID: evTargetVer, ObjectID: "object-1", ObjectType: "finding", ProjectID: "project-1", ProjectVisibility: "public"},
			evEvidenceVer: {ObjectVersionID: evEvidenceVer, ObjectID: "object-2", ObjectType: "dataset", ProjectID: "project-1", ProjectVisibility: "public"},
		},
		publications: map[string]KnowledgePublicationFacts{
			evTargetVer: {ID: "pub-1", PID: "01j9z6k3m4n5p6q7r8s9t0v1w2", ObjectVersionID: evTargetVer, ProjectID: "project-1", ProjectVisibility: "public", Rights: rights.New(), RightsValid: true},
		},
	}
}

func validEvidenceInput() CreateEvidenceAssertionInput {
	return CreateEvidenceAssertionInput{
		TargetVersionRef:   evTargetVer,
		EvidenceVersionRef: evEvidenceVer,
		Relation:           "supports",
		EvidenceType:       "experimental",
		Scope:              json.RawMessage(`{}`),
		Directness:         "direct",
		InferenceNature:    "mechanistic",
	}
}

// newEvidenceService wires the service over the fakes and returns both, so a
// case can read what was recorded and what was committed.
func newEvidenceService(t *testing.T, store *fakeEvidence, branches *fakeBranches) (*Service, *fakeRecorder, *fakeStates) {
	t.Helper()
	if branches == nil {
		branches = &fakeBranches{}
	}
	sts := &fakeStates{head: domain.ProjectState{ID: "head-1"}, stateID: "state-1"}
	rec := &fakeRecorder{}
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	svc := NewService(Deps{
		Projects:  memberProject(),
		Branches:  branches,
		States:    sts,
		Latest:    &fakeLatest{state: domain.ProjectState{ID: "latest-1"}},
		Objects:   newFakeObjects(),
		Relations: &fakeRelations{},
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    rec,
		Evidence:  store,
	})
	return svc, rec, sts
}

// TestCreateEvidenceAssertionWritesTheDerivedAxes: the row the store is
// handed carries the axes the COMMAND derived — origin/external from the
// project comparison, visibility from the asserting project, the branch and
// the cited version's own policy pin — and the review state is not the
// author's to set.
func TestCreateEvidenceAssertionWritesTheDerivedAxes(t *testing.T) {
	store := networkedEvidence()
	svc, _, sts := newEvidenceService(t, store, nil)

	res, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", validEvidenceInput())
	if err != nil {
		t.Fatalf("CreateEvidenceAssertion: %v", err)
	}
	if sts.committed != 1 {
		t.Errorf("commits = %d, want the assertion to land as one state commit", sts.committed)
	}
	if store.gotWrite.EvidenceOrigin != string(domain.EvidenceOriginInternal) {
		t.Errorf("evidence_origin = %q, want %q: the asserting project owns the target", store.gotWrite.EvidenceOrigin, domain.EvidenceOriginInternal)
	}
	if store.gotWrite.Visibility != string(domain.EvidenceVisibilityPublic) {
		t.Errorf("visibility = %q, want public (public project, public branch, no version policy pin)", store.gotWrite.Visibility)
	}
	if res.Assertion.ReviewState != string(domain.EvidenceReviewUnreviewed) {
		t.Errorf("review_state = %q, want the row's own default: an assertion is born unreviewed", res.Assertion.ReviewState)
	}
	if store.gotWrite.Scope == nil || string(store.gotWrite.Scope) != "{}" {
		t.Errorf("scope = %s, want the empty object the schema defaults to", store.gotWrite.Scope)
	}
}

// TestCreateEvidenceAssertionStoresTheSchemasUnknownForUndeclaredAxes: the
// two axes a caller may omit are not columns the table will fill in. The
// INSERT names them, so 00007's DEFAULT never applies, and 00058's CHECK
// admits only the canonical vocabulary — an empty string is a REJECTED row,
// not an undeclared one. The command therefore normalizes "not declared" to
// the schema's own 'unknown' before the row is built, which is what the
// write path documents and what this pins; the end-to-end consequence (a
// caller following specs/mcp/tools.json gets 201, never the store failure)
// is pinned in tests/integration/evidence_network_test.go.
func TestCreateEvidenceAssertionStoresTheSchemasUnknownForUndeclaredAxes(t *testing.T) {
	store := networkedEvidence()
	svc, _, _ := newEvidenceService(t, store, nil)

	in := validEvidenceInput()
	in.Directness, in.InferenceNature = "", ""
	if _, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", in); err != nil {
		t.Fatalf("CreateEvidenceAssertion with neither axis declared: %v", err)
	}
	if store.gotWrite.Directness != string(domain.EvidenceDirectnessUnknown) {
		t.Errorf("directness = %q, want %q for an undeclared axis", store.gotWrite.Directness, domain.EvidenceDirectnessUnknown)
	}
	if store.gotWrite.InferenceNature != string(domain.EvidenceInferenceUnknown) {
		t.Errorf("inference_nature = %q, want %q for an undeclared axis", store.gotWrite.InferenceNature, domain.EvidenceInferenceUnknown)
	}

	// A blank value is the same declaration as an absent one, not a
	// fourth directness.
	in.Directness, in.InferenceNature = "  ", "\t"
	if _, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", in); err != nil {
		t.Fatalf("CreateEvidenceAssertion with blank axes: %v", err)
	}
	if store.gotWrite.Directness != string(domain.EvidenceDirectnessUnknown) {
		t.Errorf("directness = %q, want %q for a blank axis", store.gotWrite.Directness, domain.EvidenceDirectnessUnknown)
	}
	if store.gotWrite.InferenceNature != string(domain.EvidenceInferenceUnknown) {
		t.Errorf("inference_nature = %q, want %q for a blank axis", store.gotWrite.InferenceNature, domain.EvidenceInferenceUnknown)
	}

	// A declared value still travels as itself: the normalization fills a
	// gap, it does not overwrite a declaration.
	in.Directness, in.InferenceNature = "indirect", "causal"
	if _, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", in); err != nil {
		t.Fatalf("CreateEvidenceAssertion with declared axes: %v", err)
	}
	if store.gotWrite.Directness != "indirect" || store.gotWrite.InferenceNature != "causal" {
		t.Errorf("declared axes = (%q, %q), want (indirect, causal): normalization must not overwrite a declaration",
			store.gotWrite.Directness, store.gotWrite.InferenceNature)
	}
}

// TestCreateEvidenceAssertionDerivesExternalFromAnotherProject: the same
// write from another project is EXTERNAL evidence, and every axis of the
// classification follows from facts the command read — never from a field the
// caller sent (the request shape has none).
func TestCreateEvidenceAssertionDerivesExternalFromAnotherProject(t *testing.T) {
	store := networkedEvidence()
	// The asserting project is project-2 (the caller's own project); the
	// target version belongs to project-1, and the cited version is
	// project-2's own.
	store.facts[evEvidenceVer] = VersionProjectFacts{ObjectVersionID: evEvidenceVer, ObjectID: "object-2", ObjectType: "dataset", ProjectID: "project-2", ProjectVisibility: "public"}
	svc, rec, _ := newEvidenceService(t, store, nil)

	res, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-2", "branch-1", validEvidenceInput())
	if err != nil {
		t.Fatalf("CreateEvidenceAssertion: %v", err)
	}
	if store.gotWrite.EvidenceOrigin != string(domain.EvidenceOriginExternal) {
		t.Errorf("evidence_origin = %q, want external", store.gotWrite.EvidenceOrigin)
	}
	if store.gotWrite.ProjectID != "project-2" {
		t.Errorf("the row was written for project %q, want the asserting project", store.gotWrite.ProjectID)
	}
	if res.Assertion.ReviewState != string(domain.EvidenceReviewUnreviewed) {
		t.Errorf("review_state = %q, want unreviewed", res.Assertion.ReviewState)
	}
	if len(rec.recorded) != 2 {
		t.Fatalf("events = %d, want the state commit plus knowledge.external_evidence_added", len(rec.recorded))
	}
	added := rec.recorded[1]
	if added.EventType != eventKnowledgeExternalEvidenceAdded {
		t.Errorf("event = %q, want %q", added.EventType, eventKnowledgeExternalEvidenceAdded)
	}
	if added.Visibility != events.VisibilityPublic {
		t.Errorf("visibility = %q, want public: the assertion and the publication are both public", added.Visibility)
	}
}

// TestCreateEvidenceAssertionEventNamesTheAssertionAndItsEnds: the event's
// payload is identity and references — the assertion, the two version pins
// and the publication — so a consumer can resolve the rows itself. The
// declared type name is the YAML's knowledge.external_evidence_added, not
// docs/18's older prose spelling.
func TestCreateEvidenceAssertionEventNamesTheAssertionAndItsEnds(t *testing.T) {
	store := networkedEvidence()
	store.facts[evEvidenceVer] = VersionProjectFacts{ObjectVersionID: evEvidenceVer, ObjectID: "object-2", ObjectType: "dataset", ProjectID: "project-2", ProjectVisibility: "public"}
	svc, rec, _ := newEvidenceService(t, store, nil)

	res, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-2", "branch-1", validEvidenceInput())
	if err != nil {
		t.Fatalf("CreateEvidenceAssertion: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.recorded[1].Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	for key, want := range map[string]string{
		"assertion_id":               res.Assertion.ID,
		"project_id":                 "project-2",
		"target_object_version_id":   evTargetVer,
		"evidence_object_version_id": evEvidenceVer,
		"relation":                   "supports",
		"evidence_type":              "experimental",
		"evidence_origin":            string(domain.EvidenceOriginExternal),
		"knowledge_pid":              "01j9z6k3m4n5p6q7r8s9t0v1w2",
	} {
		if payload[key] != want {
			t.Errorf("payload[%q] = %v, want %q", key, payload[key], want)
		}
	}
}

// TestCreateEvidenceAssertionOriginEvidenceEmitsNoExternalEvent: the event
// is "external evidence ADDED", so an assertion from the object's own project
// does not emit it. It still lands as a state commit with its own
// state.committed event — the write is not silent, it is simply not news.
func TestCreateEvidenceAssertionOriginEvidenceEmitsNoExternalEvent(t *testing.T) {
	store := networkedEvidence()
	svc, rec, _ := newEvidenceService(t, store, nil)

	if _, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", validEvidenceInput()); err != nil {
		t.Fatalf("CreateEvidenceAssertion: %v", err)
	}
	if len(rec.recorded) != 1 {
		t.Fatalf("events = %d, want only the state commit", len(rec.recorded))
	}
	if rec.recorded[0].EventType == eventKnowledgeExternalEvidenceAdded {
		t.Errorf("origin evidence emitted %q", eventKnowledgeExternalEvidenceAdded)
	}
}

// TestCreateEvidenceAssertionEventIsPrivateWhenTheAssertionIs: an event is
// never more visible than its subject. An assertion whose own visibility is
// private (here: committed to a private branch) emits a private event, even
// though the publication it lands on is network-visible.
func TestCreateEvidenceAssertionEventIsPrivateWhenTheAssertionIs(t *testing.T) {
	store := networkedEvidence()
	store.facts[evEvidenceVer] = VersionProjectFacts{ObjectVersionID: evEvidenceVer, ObjectID: "object-2", ObjectType: "dataset", ProjectID: "project-2", ProjectVisibility: "public"}
	svc, rec, _ := newEvidenceService(t, store, &fakeBranches{visibility: domain.BranchVisibilityPrivate})

	res, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-2", "branch-1", validEvidenceInput())
	if err != nil {
		t.Fatalf("CreateEvidenceAssertion: %v", err)
	}
	if store.gotWrite.Visibility != string(domain.EvidenceVisibilityPrivate) {
		t.Errorf("visibility = %q, want private for a private branch", store.gotWrite.Visibility)
	}
	if rec.recorded[1].Visibility != events.VisibilityPrivate {
		t.Errorf("event visibility = %q, want private: the event must not outlive its subject", rec.recorded[1].Visibility)
	}
	if res.Assertion.Visibility != string(domain.EvidenceVisibilityPrivate) {
		t.Errorf("the stored row reports %q, want the derived private", res.Assertion.Visibility)
	}
}

// TestCreateEvidenceAssertionRefusesAVersionWithNoPolicy: a cited version
// that pins its own visibility policy is not governed by the project default,
// so the assertion is private even in a public project on a public branch.
func TestCreateEvidenceAssertionRefusesAVersionWithNoPolicy(t *testing.T) {
	policy := "policy-1"
	store := networkedEvidence()
	store.facts[evEvidenceVer] = VersionProjectFacts{
		ObjectVersionID: evEvidenceVer, ObjectID: "object-2", ObjectType: "dataset",
		ProjectID: "project-1", ProjectVisibility: "public", VisibilityPolicyID: &policy,
	}
	svc, _, _ := newEvidenceService(t, store, nil)

	if _, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", validEvidenceInput()); err != nil {
		t.Fatalf("CreateEvidenceAssertion: %v", err)
	}
	if store.gotWrite.Visibility != string(domain.EvidenceVisibilityPrivate) {
		t.Errorf("visibility = %q, want private: a version with its own policy pin is not governed by the project default", store.gotWrite.Visibility)
	}
}

// TestCreateEvidenceAssertionRefusalsAreOneOutcome is the fail-closed table:
// every "you may not pin that version there" answers the SAME error, with the
// same code, and writes nothing. Folding them into distinguishable outcomes
// would make the write route an oracle for which versions exist, which
// projects are published, and whose members may see what.
func TestCreateEvidenceAssertionRefusalsAreOneOutcome(t *testing.T) {
	membersOnly := KnowledgePublicationFacts{
		ID: "pub-1", PID: "01j9z6k3m4n5p6q7r8s9t0v1w2", ObjectVersionID: evTargetVer,
		ProjectID: "project-1", ProjectVisibility: "public", RightsValid: false,
	}
	cases := []struct {
		name string
		// mutate adjusts the world and/or the input for this case.
		mutate func(store *fakeEvidence, in *CreateEvidenceAssertionInput)
		side   string
	}{
		{
			name: "the target version does not exist",
			mutate: func(store *fakeEvidence, in *CreateEvidenceAssertionInput) {
				delete(store.facts, evTargetVer)
			},
			side: "target",
		},
		{
			name: "the cited version does not exist",
			mutate: func(store *fakeEvidence, in *CreateEvidenceAssertionInput) {
				delete(store.facts, evEvidenceVer)
			},
			side: "evidence",
		},
		{
			name: "the cited version belongs to another project",
			mutate: func(store *fakeEvidence, in *CreateEvidenceAssertionInput) {
				store.facts[evEvidenceVer] = VersionProjectFacts{ObjectVersionID: evEvidenceVer, ProjectID: evOtherProject, ProjectVisibility: "public"}
			},
			side: "evidence",
		},
		{
			name: "the target carries no publication",
			mutate: func(store *fakeEvidence, in *CreateEvidenceAssertionInput) {
				delete(store.publications, evTargetVer)
				// The cited version is the asserting project's own, so the
				// refusal below is the target's and not the evidence side's.
				store.facts[evEvidenceVer] = VersionProjectFacts{ObjectVersionID: evEvidenceVer, ProjectID: "project-2", ProjectVisibility: "public"}
			},
			side: "target",
		},
		{
			name: "the target is published to its own members only",
			mutate: func(store *fakeEvidence, in *CreateEvidenceAssertionInput) {
				store.publications[evTargetVer] = membersOnly
				// The assertion comes from ANOTHER project, so the
				// members-only audience is what refuses it.
				store.facts[evEvidenceVer] = VersionProjectFacts{ObjectVersionID: evEvidenceVer, ProjectID: "project-2", ProjectVisibility: "public"}
			},
			side: "target",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := networkedEvidence()
			in := validEvidenceInput()
			c.mutate(store, &in)
			svc, rec, sts := newEvidenceService(t, store, nil)

			_, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-2", "branch-1", in)
			var refused *EvidenceRefUnavailableError
			if !errors.As(err, &refused) {
				t.Fatalf("err = %v, want EvidenceRefUnavailableError", err)
			}
			if refused.Side != c.side {
				t.Errorf("refused side = %q, want %q", refused.Side, c.side)
			}
			if refused.Code() != CodeEvidenceRefUnavailable {
				t.Errorf("code = %q, want %q", refused.Code(), CodeEvidenceRefUnavailable)
			}
			if store.writeCalls != 0 || sts.committed != 0 {
				t.Errorf("the refusal wrote a row (%d writes, %d commits)", store.writeCalls, sts.committed)
			}
			if len(rec.recorded) != 0 {
				t.Errorf("the refusal recorded %d events", len(rec.recorded))
			}
		})
	}
}

// TestCreateEvidenceAssertionRefusalDoesNotLeakTheReason: the refusal names
// the version the caller itself sent, and none of the reasons. A client that
// could read "it exists but is not published" out of the message would learn
// what the repository holds.
func TestCreateEvidenceAssertionRefusalDoesNotLeakTheReason(t *testing.T) {
	store := networkedEvidence()
	delete(store.publications, evTargetVer) // published is the ONLY thing missing
	svc, _, _ := newEvidenceService(t, store, nil)

	_, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", validEvidenceInput())
	if err == nil {
		t.Fatal("a version with no publication was accepted")
	}
	message := err.Error()
	for _, leak := range []string{"publication", "publish", "external", "audience", "project"} {
		if strings.Contains(message, leak) {
			t.Errorf("the refusal message discloses %q: %s", leak, message)
		}
	}
	// The version the caller named is not a disclosure: the caller sent it.
	if !strings.Contains(message, evTargetVer) {
		t.Errorf("the refusal does not name the version the caller sent: %s", message)
	}
}

// TestCreateEvidenceAssertionFailsClosedWithoutAStore: a service wired
// without the evidence port refuses the write rather than writing a row with
// invented axes.
func TestCreateEvidenceAssertionFailsClosedWithoutAStore(t *testing.T) {
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	svc := NewService(Deps{
		Projects: memberProject(),
		Branches: &fakeBranches{},
		States:   &fakeStates{head: domain.ProjectState{ID: "head-1"}, stateID: "state-1"},
		Latest:   &fakeLatest{state: domain.ProjectState{ID: "latest-1"}},
		Objects:  newFakeObjects(),
		Authz:    authz.NewMatrixEngine(),
		Schemas:  reg,
		Events:   &fakeRecorder{},
	})
	_, err = svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", validEvidenceInput())
	if !errors.Is(err, ErrStore) {
		t.Fatalf("err = %v, want ErrStore for a service with no evidence store", err)
	}
}

// TestCreateEvidenceAssertionFailsClosedOnAStoreFailure: a read that could
// not be completed is not a "no". Aborting the write is the only outcome that
// cannot store an assertion whose axes were never established.
func TestCreateEvidenceAssertionFailsClosedOnAStoreFailure(t *testing.T) {
	store := networkedEvidence()
	store.pubErr = errors.New("connection reset")
	svc, _, sts := newEvidenceService(t, store, nil)

	_, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", validEvidenceInput())
	if err == nil {
		t.Fatal("the write proceeded over a failed publication read")
	}
	if sts.committed != 0 {
		t.Errorf("commits = %d over a failed read", sts.committed)
	}
}

// TestCreateEvidenceAssertionIsRefusedBeforeAnyRead: authorization runs
// first, so a refused caller learns nothing about whether the versions exist
// (docs/45). The store is never touched.
func TestCreateEvidenceAssertionIsRefusedBeforeAnyRead(t *testing.T) {
	store := networkedEvidence()
	svc, _, _ := newEvidenceService(t, store, nil)
	// The actor has no role in the project: GetMembership answers
	// ErrMemberNotFound, which is the "no" requireWrite turns into a refusal.
	svc.projects = &fakeProjects{membership: projects.ErrMemberNotFound}

	_, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", validEvidenceInput())
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if store.writeCalls != 0 {
		t.Errorf("a refused caller reached the evidence store")
	}
}

// TestCreateEvidenceAssertionRefusesAMalformedReference: a ref that could
// never name a version row is a caller error, answered as validation — not a
// lookup that comes back "not found" and hides the difference.
func TestCreateEvidenceAssertionRefusesAMalformedReference(t *testing.T) {
	store := networkedEvidence()
	svc, _, _ := newEvidenceService(t, store, nil)

	in := validEvidenceInput()
	in.TargetVersionRef = "object_version:not-a-uuid"
	_, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", in)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if store.writeCalls != 0 {
		t.Errorf("a malformed reference reached the store")
	}
}

// --------------------------------------------------------------------------
// T0509: the literature evidence unit — a REFUSAL on this write path
// --------------------------------------------------------------------------

// TestCreateEvidenceAssertionRefusesLiteratureWithoutAnEvidenceUnit: docs/10
// §6 forbids `DOI -> supports Claim` outright (docs/19 §4 states the same rule
// from the reference side), and V1's schema carries no excerpt field, so the
// reasoning note is the only place a literature assertion can name the unit it
// cites. An EMPTY note therefore names no unit under any reading, and this
// write path refuses it.
//
// The distinction this test exists to keep: the rule is "nothing was written",
// NOT "what was written is not good enough". Both notes below are empty of any
// location — one literally, one after trimming — and both are refused.
func TestCreateEvidenceAssertionRefusesLiteratureWithoutAnEvidenceUnit(t *testing.T) {
	for _, tc := range []struct {
		name string
		note string
	}{
		{"an empty note", ""},
		{"a whitespace-only note", " \t\n "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := networkedEvidence()
			in := validEvidenceInput()
			in.EvidenceType = "literature"
			in.ReasoningNote = tc.note
			svc, rec, sts := newEvidenceService(t, store, nil)

			_, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", in)
			var refused *LiteratureEvidenceUnitUnnamedError
			if !errors.As(err, &refused) {
				t.Fatalf("err = %v, want LiteratureEvidenceUnitUnnamedError", err)
			}
			if refused.Code() != semantics.HintLiteratureEvidenceUnitUnnamed {
				t.Errorf("code = %q, want %q", refused.Code(), semantics.HintLiteratureEvidenceUnitUnnamed)
			}
			// The promoted advisory travels WITH the refusal — this is what
			// "the hints are not silently dropped" means on this path: the
			// author still reads the locate-the-unit guidance.
			for _, want := range []string{"figure", "table", "reasoning note"} {
				if !strings.Contains(refused.Error(), want) {
					t.Errorf("the refusal message %q does not carry the advisory's %q", refused.Error(), want)
				}
			}
			if store.writeCalls != 0 || sts.committed != 0 {
				t.Errorf("the refusal wrote a row (%d writes, %d commits)", store.writeCalls, sts.committed)
			}
			if len(rec.recorded) != 0 {
				t.Errorf("the refusal recorded %d events", len(rec.recorded))
			}
		})
	}
}

// TestCreateEvidenceAssertionRefusesLiteratureBeforeAnyRead: the semantic
// checks are payload-pure, so the refusal lands before the first storage read.
// The world below has NO target version at all — a write that read first would
// answer EvidenceRefUnavailableError — and the payload mistake still decides,
// which is what keeps a denial from disclosing whether the versions named
// exist (docs/45, the ordering rule this file's header states).
func TestCreateEvidenceAssertionRefusesLiteratureBeforeAnyRead(t *testing.T) {
	store := networkedEvidence()
	delete(store.facts, evTargetVer)
	svc, _, _ := newEvidenceService(t, store, nil)

	in := validEvidenceInput()
	in.EvidenceType = "literature"
	in.ReasoningNote = ""
	_, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", in)
	var refused *LiteratureEvidenceUnitUnnamedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want LiteratureEvidenceUnitUnnamedError — the payload rule runs before the version lookup", err)
	}
	if store.writeCalls != 0 {
		t.Errorf("the refusal reached the store")
	}
}

// TestCreateEvidenceAssertionAcceptsLiteratureThatNamesAnyUnit: the OTHER
// direction, and the half that keeps the rule honest. Whether a note names a
// SUFFICIENT unit is a scientific call the check must not make for the author
// (internal/rsg/semantics/evidence_assertion.go: "Both rules warn only"), so
// this write path judges nothing about the note's content — every non-empty
// note below is accepted and stored as written, including the ones no reviewer
// would credit. An implementation that only ever refused could pass the
// negative test above; it cannot pass this one.
func TestCreateEvidenceAssertionAcceptsLiteratureThatNamesAnyUnit(t *testing.T) {
	// "the paper" and "?" name no usable location in any scientific reading —
	// and are exactly what this command must NOT be the judge of.
	for _, note := range []string{
		"figure 3b, 298 K isotherm",
		"Table 2",
		"the paper",
		"somewhere in section 4",
		"?",
	} {
		t.Run(note, func(t *testing.T) {
			store := networkedEvidence()
			in := validEvidenceInput()
			in.EvidenceType = "literature"
			in.ReasoningNote = note
			svc, rec, sts := newEvidenceService(t, store, nil)

			res, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", in)
			if err != nil {
				t.Fatalf("a literature assertion with a note was refused: %v", err)
			}
			if store.writeCalls != 1 || sts.committed != 1 {
				t.Errorf("writes = %d, commits = %d, want the assertion to land as one state commit", store.writeCalls, sts.committed)
			}
			if len(rec.recorded) == 0 {
				t.Error("no event was recorded for the accepted assertion")
			}
			if store.gotWrite.ReasoningNote != note {
				t.Errorf("stored reasoning_note = %q, want %q — the location is the caller's text, never rewritten", store.gotWrite.ReasoningNote, note)
			}
			if store.gotWrite.EvidenceType != string(domain.EvidenceTypeLiterature) {
				t.Errorf("stored evidence_type = %q, want literature", store.gotWrite.EvidenceType)
			}
			if res.Assertion.ReasoningNote != note {
				t.Errorf("returned reasoning_note = %q, want %q", res.Assertion.ReasoningNote, note)
			}
			for _, hint := range res.Hints {
				if hint.Code == semantics.HintLiteratureEvidenceUnitUnnamed {
					t.Errorf("the unnamed-unit advisory fired for a note that IS present: %+v", hint)
				}
			}
		})
	}
}

// TestCreateEvidenceAssertionLeavesNonLiteratureNotesAlone: the rule is scoped
// by the EVIDENCE TYPE, never by the relation or by a guess about the text
// (docs/10 §6 is about literature evidence specifically). An experimental
// assertion with no note — the shape the existing integration cases post — is
// untouched.
func TestCreateEvidenceAssertionLeavesNonLiteratureNotesAlone(t *testing.T) {
	for _, evidenceType := range []domain.EvidenceType{
		domain.EvidenceTypeExperimental,
		domain.EvidenceTypeComputational,
		domain.EvidenceTypeDataset,
		domain.EvidenceTypeExternalAttestation,
		domain.EvidenceTypeOther,
	} {
		t.Run(string(evidenceType), func(t *testing.T) {
			store := networkedEvidence()
			in := validEvidenceInput()
			in.EvidenceType = string(evidenceType)
			in.ReasoningNote = ""
			svc, _, _ := newEvidenceService(t, store, nil)

			if _, err := svc.CreateEvidenceAssertion(context.Background(), ownerActor(), "project-1", "branch-1", in); err != nil {
				t.Fatalf("%s assertion with no note was refused: %v", evidenceType, err)
			}
			if store.writeCalls != 1 {
				t.Errorf("writes = %d, want 1", store.writeCalls)
			}
		})
	}
}
