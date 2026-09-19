package rsg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/rsg/semantics"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// The evidence-assertion write command (T0806, docs/10 §3/§4/§7).
//
// # What it writes, and what it does not
//
// One evidence_assertions row, inside one state commit — the same shape
// every other scientific-state write has here (states.Commit, gate draft,
// via api, one state.committed event). It writes NO relation row: the
// Evidence Graph and the Provenance Graph share nodes and never relation
// semantics (domain invariant 10, docs/10 §1), which is why this is a
// command of its own rather than a relation type.
//
// # The rules, in the order they run
//
//  1. authorize — requireWrite, before anything is read, so a denial never
//     discloses whether the versions named exist (docs/45).
//  2. shape — the domain's EvidenceAssertion.Validate over enums, the two
//     version pins and the scope object, plus the uuid shape of both pins
//     (a ref that could never name a version row is a caller error, not a
//     store failure), plus the one semantic check that is a REFUSAL here
//     rather than an advisory: a literature assertion that names no evidence
//     unit at all (an empty reasoning note). The check itself keeps warning
//     — whether a note names a SUFFICIENT unit is not its call — and this
//     command promotes that one hint, and only that one, by its code; see
//     the loop below. It lands before step 3's lookups, for the reason
//     step 1 does: the semantic checks are payload-pure, so nothing about
//     the repository is consulted to reach it.
//  3. resolve both ends — the target version and the cited version, each
//     through scientific_object_versions -> scientific_objects -> projects.
//     The cited version must belong to THIS project: an assertion cites the
//     asserting project's own evidence, and a version of another project is
//     refused with the same outcome as one that does not exist.
//  4. the target must be PUBLISHED. docs/10 §7 is about the evidence a
//     Published Knowledge Object carries, and the write side keeps that
//     honest: an assertion lands on a version that carries a
//     knowledge_publications row.
//  5. cross-project assertions need the target to be NETWORK-visible: the
//     publication's audience (knowledgepublish.AudienceFor — the one rule)
//     must be the network. A members-only publication is refused the same
//     existence-hiding way, so another project cannot attach evidence to
//     work its own members cannot even see (owner ruling L3-20260916-1:
//     发布不等于公开).
//  6. the two axes are DERIVED, never client input: evidence_origin from
//     the project comparison, visibility from the asserting project and the
//     branch the assertion is committed to (plus the cited version's own
//     policy pin). Neither is spelled in the request shape — there is
//     nothing to trust and nothing to override.
//  7. the event. A cross-project assertion emits
//     knowledge.external_evidence_added, explicitly visible or not; an
//     origin assertion does not (its own project's evidence is not
//     "external evidence added", and the generic
//     evidence_assertion.created event is not this task's requirement).
//
// # Fail closed
//
// Every read failure — an unresolvable version, an unreadable publication
// record, a rights document that does not parse — refuses the write. A
// refused write leaves no row: the assertion and its events commit or roll
// back together (the transactional outbox).

// CreateEvidenceAssertionInput carries one evidence-assertion creation.
//
// The two refs are version ids; the transport strips the platform's
// `object_version:` prefix, exactly as the publish surface does for
// knowledge_version_ref (specs/mcp/tools.json names the MCP arguments
// target_version_ref / evidence_version_ref, and the evidence-assertion
// schema names the same two pins).
type CreateEvidenceAssertionInput struct {
	// TargetVersionRef names the published object version the assertion is
	// about (the schema's target_version_ref).
	TargetVersionRef string
	// EvidenceVersionRef names the version the assertion cites as its
	// evidence (the schema's evidence_version_ref). It must be a version of
	// the asserting project.
	EvidenceVersionRef string
	// Relation is one of the nine canonical evidence relations (docs/10 §4).
	Relation string
	// EvidenceType is one of the six canonical evidence types.
	EvidenceType string
	// Scope bounds where the assertion holds; empty means {} — the empty
	// object this command writes for it, since the column's own DEFAULT
	// cannot apply (the INSERT names the column).
	Scope json.RawMessage
	// Directness and InferenceNature are the schema's declarations; empty
	// means "not declared", which this command stores as the schema's own
	// 'unknown' — the value 00007 defaults both columns to and 00058's
	// CHECKs admit.
	Directness      string
	InferenceNature string
	// ReasoningNote is the author's explanation (docs/10 §6: a literature
	// assertion names its specific evidence unit here).
	ReasoningNote string
}

// EvidenceAssertionResult is one assertion creation outcome: the stored row
// (as the database wrote it — created_at, the default review state and the
// derived axes are read back, never invented here) and the advisory semantic
// hints.
type EvidenceAssertionResult struct {
	Assertion EvidenceAssertionRow
	Hints     []semantics.Hint
}

// CreateEvidenceAssertion writes one evidence assertion as a state commit.
// See the file header for the rules and their order.
func (s *Service) CreateEvidenceAssertion(ctx context.Context, actor domain.User, projectID, branchID string, in CreateEvidenceAssertionInput) (EvidenceAssertionResult, error) {
	if actor.ID == "" {
		return EvidenceAssertionResult{}, fmt.Errorf("%w: actor is required", ErrValidation)
	}
	if err := s.requireWrite(ctx, actor, projectID); err != nil {
		return EvidenceAssertionResult{}, err
	}
	if s.evidence == nil {
		return EvidenceAssertionResult{}, fmt.Errorf("%w: no evidence store configured", ErrStore)
	}
	branch, err := s.branches.Get(ctx, projectID, branchID)
	if err != nil {
		return EvidenceAssertionResult{}, wrapError(err)
	}
	target := strings.TrimSpace(in.TargetVersionRef)
	evidenceRef := strings.TrimSpace(in.EvidenceVersionRef)
	scope := in.Scope
	if len(scope) == 0 || string(scope) == "null" {
		scope = json.RawMessage("{}")
	}
	// Directness and InferenceNature are declarations the caller may omit,
	// and 'unknown' is the value the schema itself gives them (00007's
	// DEFAULT, inside both CHECK vocabularies): on these two columns "not
	// declared" and "unknown" are the same statement, which is why the
	// platform's own declaration for this operation lists neither
	// (specs/mcp/tools.json's evidence.create_assertion). The normalization
	// has to happen here rather than at the table: the INSERT names both
	// columns explicitly, so their DEFAULT never applies, and 00058's CHECK
	// would reject the empty string — turning a caller's omission into a
	// store failure. Same shape as Scope's {} just above.
	directness := domain.EvidenceDirectness(strings.TrimSpace(in.Directness))
	if directness == "" {
		directness = domain.EvidenceDirectnessUnknown
	}
	inferenceNature := domain.EvidenceInferenceNature(strings.TrimSpace(in.InferenceNature))
	if inferenceNature == "" {
		inferenceNature = domain.EvidenceInferenceUnknown
	}
	content := domain.EvidenceAssertion{
		TargetVersionRef:   target,
		EvidenceVersionRef: evidenceRef,
		Relation:           domain.EvidenceRelation(strings.TrimSpace(in.Relation)),
		EvidenceType:       domain.EvidenceType(strings.TrimSpace(in.EvidenceType)),
		Scope:              scope,
		Directness:         directness,
		InferenceNature:    inferenceNature,
		ReasoningNote:      strings.TrimSpace(in.ReasoningNote),
		// An assertion is born unreviewed: the review state is the
		// REVIEWER's position on it (docs/10 §3), so the author's write
		// never sets it. The default is the column's own; it is stated
		// here so the stored row and the response agree without a reread.
		ReviewState: domain.EvidenceReviewUnreviewed,
	}
	if err := content.Validate(); err != nil {
		return EvidenceAssertionResult{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if !domain.ValidUUID(target) {
		return EvidenceAssertionResult{}, fmt.Errorf("%w: target_version_ref must name an object version (object_version:<uuid>)", ErrValidation)
	}
	if !domain.ValidUUID(evidenceRef) {
		return EvidenceAssertionResult{}, fmt.Errorf("%w: evidence_version_ref must name an object version (object_version:<uuid>)", ErrValidation)
	}

	// The semantic checks (docs/10 §5/§6) run here, with the shape rules and
	// before either VERSION is resolved, because they are payload-PURE: they
	// never touch storage (internal/rsg/semantics says so by contract). A
	// payload this write will refuse anyway is therefore refused before the
	// first lookup of the objects it names, which is the half that matters for
	// disclosure: a caller whose payload was the problem learns only that,
	// whatever the repository holds (docs/45).
	//
	// The target's structured claim is not resolved: this command has no claim
	// port, and the causal-basis hint is the one check that needs it; the
	// structural checks and the literature-unit hint run regardless.
	_, hints := semantics.CheckEvidenceAssertion(content, nil)

	// One of those advisories is a REFUSAL on this path. A literature
	// assertion whose reasoning note is blank names no evidence unit at all
	// (docs/10 §6: a DOI may not support a claim directly; docs/19 §4: the
	// assertion points at a specific location/excerpt/figure/table/dataset/
	// method), and the check's own predicate for that is exactly "the place is
	// empty" — the place being the reasoning note, because V1's schema carries
	// no excerpt field for it.
	//
	// The promotion is keyed on the hint CODE, never on a second condition
	// spelled out here: one predicate, one definition. It deliberately does
	// NOT promote the other advisory, and it makes no judgement about a note
	// that IS present — whether the note names a SUFFICIENT unit stays the
	// author's and the reviewer's call, which the check must not make for them
	// and neither does this command (docs/10 §4: V1 不自动赋数值权重;
	// CLAUDE.md §9.12).
	for _, hint := range hints {
		if hint.Code == semantics.HintLiteratureEvidenceUnitUnnamed {
			// The advisory is not dropped on the way out: the refusal carries
			// it, so the author still reads the guidance the hint would have
			// rendered (docs/10 §6's locate-the-unit list).
			return EvidenceAssertionResult{}, &LiteratureEvidenceUnitUnnamedError{Hint: hint}
		}
	}

	// Both ends are resolved from storage; a version that does not exist and
	// a version this caller may not pin both answer the one outcome below.
	targetFacts, err := s.evidence.GetVersionProjectFacts(ctx, target)
	if err != nil {
		if errors.Is(err, sciobjects.ErrVersionNotFound) {
			return EvidenceAssertionResult{}, &EvidenceRefUnavailableError{Side: "target", VersionID: target}
		}
		return EvidenceAssertionResult{}, wrapError(err)
	}
	evidenceFacts, err := s.evidence.GetVersionProjectFacts(ctx, evidenceRef)
	if err != nil {
		if errors.Is(err, sciobjects.ErrVersionNotFound) {
			return EvidenceAssertionResult{}, &EvidenceRefUnavailableError{Side: "evidence", VersionID: evidenceRef}
		}
		return EvidenceAssertionResult{}, wrapError(err)
	}
	if evidenceFacts.ProjectID != projectID {
		return EvidenceAssertionResult{}, &EvidenceRefUnavailableError{Side: "evidence", VersionID: evidenceRef}
	}

	// The target must be published knowledge, and a cross-project assertion
	// needs more than that: the publication must be readable by the network.
	publication, published, err := s.evidence.GetKnowledgePublicationForVersion(ctx, target)
	if err != nil {
		return EvidenceAssertionResult{}, wrapError(err)
	}
	if !published {
		return EvidenceAssertionResult{}, &EvidenceRefUnavailableError{Side: "target", VersionID: target}
	}
	external := domain.AssertionIsExternal(projectID, targetFacts.ProjectID)
	if external && publication.Audience() != knowledgepublish.AudienceNetwork {
		return EvidenceAssertionResult{}, &EvidenceRefUnavailableError{Side: "target", VersionID: target}
	}

	// The two axes, derived from the facts just read. Visibility is public
	// only when every axis a reader would need is public: the asserting
	// project's preset, the branch the assertion is committed to, and the
	// cited version's own visibility (a version that pins a policy is
	// governed by something other than the project default — the same
	// fail-closed reading knowledgepublish.AudienceFor applies). Anything
	// unclear stays private.
	visibility := domain.EvidenceVisibilityPrivate
	if evidenceFacts.ProjectVisibility == string(domain.VisibilityPublic) &&
		evidenceFacts.VisibilityPolicyID == nil &&
		branch.Visibility == domain.BranchVisibilityPublic {
		visibility = domain.EvidenceVisibilityPublic
	}
	origin := domain.EvidenceOriginInternal
	if external {
		origin = domain.EvidenceOriginExternal
	}

	assertionID, err := newID()
	if err != nil {
		return EvidenceAssertionResult{}, fmt.Errorf("%w: generating assertion id: %v", ErrStore, err)
	}
	head, err := s.states.GetBranchHead(ctx, branchID)
	if err != nil {
		return EvidenceAssertionResult{}, wrapError(err)
	}
	params := states.CommitParams{
		ProjectID:       projectID,
		BranchID:        branchID,
		ActorID:         actor.ID,
		Via:             domain.ViaAPI,
		Message:         fmt.Sprintf("assert %s evidence", content.Relation),
		Operations:      []domain.StateOperation{evidenceOperation(assertionID, string(content.Relation), string(content.EvidenceType))},
		BaseStateID:     &head.ID,
		ManifestVersion: manifestVersion,
		Gate:            rsgvalidation.GateDraft,
	}
	var extra func(stateID string) (events.Event, error)
	if external {
		extra = func(stateID string) (events.Event, error) {
			return externalEvidenceAddedEvent(externalEvidenceEventParams{
				AssertionID:             assertionID,
				ProjectID:               projectID,
				TargetObjectVersionID:   target,
				EvidenceObjectVersionID: evidenceRef,
				Relation:                string(content.Relation),
				EvidenceType:            string(content.EvidenceType),
				StateID:                 stateID,
				BranchID:                branchID,
				ActorID:                 actor.ID,
				KnowledgePID:            publication.PID,
				// The event is never more visible than its subject (docs/12
				// §3): public only when the assertion itself is, and the
				// publication is network-visible (which the cross-project
				// branch above already required).
				Visibility: eventVisibilityFor(visibility, publication.Audience()),
			})
		}
	}
	var row EvidenceAssertionRow
	write := func(ctx context.Context, tx states.Transaction, stateID string) error {
		var werr error
		row, werr = s.evidence.CreateEvidenceAssertionInTx(ctx, tx, CreateEvidenceAssertionInTxParams{
			ID:                      assertionID,
			ProjectID:               projectID,
			StateID:                 stateID,
			TargetObjectVersionID:   target,
			EvidenceObjectVersionID: evidenceRef,
			RelationType:            string(content.Relation),
			EvidenceType:            string(content.EvidenceType),
			Scope:                   content.Scope,
			Directness:              string(content.Directness),
			InferenceNature:         string(content.InferenceNature),
			ReasoningNote:           content.ReasoningNote,
			CreatedBy:               actor.ID,
			EvidenceOrigin:          string(origin),
			Visibility:              string(visibility),
		})
		if werr != nil {
			return werr
		}
		return s.recordCommitEvents(ctx, tx, params, stateID, eventVisibility(branch.Visibility), extra)
	}
	_, _, err = s.states.Commit(ctx, params, write)
	if err != nil {
		return EvidenceAssertionResult{}, wrapError(err)
	}
	return EvidenceAssertionResult{Assertion: row, Hints: hints}, nil
}

// evidenceOperation builds the commit operation summary for one assertion
// (the same commit_linkage identity every other member row carries: the
// pre-generated assertion id, and VersionNo 0 because an assertion has no
// version dimension — domain.StateOperation says so by name).
func evidenceOperation(assertionID, relation, evidenceType string) domain.StateOperation {
	detail, _ := json.Marshal(map[string]string{"relation": relation, "evidence_type": evidenceType})
	return domain.StateOperation{
		Kind:      domain.OperationEvidenceAsserted,
		EntityID:  assertionID,
		VersionNo: 0,
		Detail:    detail,
	}
}

// eventVisibilityFor is the network evidence event's visibility: public only
// when the assertion's own axis is public AND the publication the assertion
// lands on is network-visible. Either axis being anything else (or unclear)
// answers private — an event is never more visible than its subject
// (docs/12 §3).
func eventVisibilityFor(assertion domain.EvidenceVisibility, audience knowledgepublish.Audience) string {
	if assertion == domain.EvidenceVisibilityPublic && audience == knowledgepublish.AudienceNetwork {
		return events.VisibilityPublic
	}
	return events.VisibilityPrivate
}

// externalEvidenceEventParams carries the identity facts the event's payload
// names. Everything here is an id or an enum: payloads carry
// identity/reference only, never bulk content (the outbox carries what a
// consumer needs to resolve the rows itself).
type externalEvidenceEventParams struct {
	AssertionID             string
	ProjectID               string
	TargetObjectVersionID   string
	EvidenceObjectVersionID string
	Relation                string
	EvidenceType            string
	StateID                 string
	BranchID                string
	ActorID                 string
	KnowledgePID            string
	Visibility              string
}

// externalEvidenceAddedEvent builds the knowledge.external_evidence_added
// event (specs/events/event-types.yaml:28) for one cross-project assertion.
//
// The name is the YAML's, not docs/18 §2's older `external_evidence.added`
// spelling: the vocabulary file is canonical and names it under the
// knowledge family (docs/18 says so itself).
func externalEvidenceAddedEvent(p externalEvidenceEventParams) (events.Event, error) {
	payload, err := json.Marshal(struct {
		AssertionID             string `json:"assertion_id"`
		ProjectID               string `json:"project_id"`
		TargetObjectVersionID   string `json:"target_object_version_id"`
		EvidenceObjectVersionID string `json:"evidence_object_version_id"`
		Relation                string `json:"relation"`
		EvidenceType            string `json:"evidence_type"`
		EvidenceOrigin          string `json:"evidence_origin"`
		StateID                 string `json:"state_id"`
		BranchID                string `json:"branch_id"`
		KnowledgePID            string `json:"knowledge_pid,omitempty"`
	}{
		AssertionID:             p.AssertionID,
		ProjectID:               p.ProjectID,
		TargetObjectVersionID:   p.TargetObjectVersionID,
		EvidenceObjectVersionID: p.EvidenceObjectVersionID,
		Relation:                p.Relation,
		EvidenceType:            p.EvidenceType,
		EvidenceOrigin:          string(domain.EvidenceOriginExternal),
		StateID:                 p.StateID,
		BranchID:                p.BranchID,
		KnowledgePID:            p.KnowledgePID,
	})
	if err != nil {
		return events.Event{}, fmt.Errorf("rsg: knowledge.external_evidence_added payload: %w", err)
	}
	return events.Event{
		EventType:  eventKnowledgeExternalEvidenceAdded,
		ActorID:    p.ActorID,
		ProjectID:  p.ProjectID,
		Visibility: p.Visibility,
		Payload:    payload,
	}, nil
}
