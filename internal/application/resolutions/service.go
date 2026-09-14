package resolutions

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/lichman0405/post/internal/application/diffs"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/conflict"
)

// Service orchestrates the conflict resolution writes and reads. It owns
// input validation (the ports trust, the service verifies) and the write
// authorization; the reads perform no authorization of their own — the
// HTTP surface resolves visibility through the project read gate before
// calling them (package doc).
type Service struct {
	conflicts ConflictsPort
	store     StorePort
	projects  ProjectGate
	authz     Authz
}

// NewService wires the resolution service. Every port is required: an
// unwired surface refuses at call time rather than guessing (default
// deny, same shape as the RSG service).
func NewService(conflicts ConflictsPort, store StorePort, projects ProjectGate, authz Authz) *Service {
	return &Service{conflicts: conflicts, store: store, projects: projects, authz: authz}
}

// Save records one plan submission: the decisions the human made about
// the conflicts of the pinned triple. It authorizes the actor first (the
// refusal happens before any state lookup), recomputes the conflict
// report for the triple — which also validates the three states — and
// verifies every decision against it: a decision naming a conflict the
// detector did not classify is refused, never stored. The accepted
// decisions are upserted with one audit entry in the same transaction,
// and the full updated plan is returned.
func (s *Service) Save(ctx context.Context, actor domain.User, in SaveInput) ([]domain.ConflictResolution, error) {
	if s.projects == nil || s.authz == nil || s.conflicts == nil || s.store == nil {
		return nil, fmt.Errorf("%w: resolution service not fully wired", ErrStore)
	}
	if err := validateSaveInput(in); err != nil {
		return nil, err
	}
	if err := s.requireWrite(ctx, actor, in.ProjectID); err != nil {
		return nil, err
	}
	report, err := s.conflicts.Conflicts(ctx, diffs.Params{
		ProjectID:     in.ProjectID,
		BaseStateID:   in.BaseStateID,
		SourceStateID: in.SourceStateID,
		TargetStateID: in.TargetStateID,
	})
	if err != nil {
		return nil, err
	}
	index := conflictIndex(report)
	rows := make([]domain.ConflictResolution, 0, len(in.Decisions))
	for _, d := range in.Decisions {
		if !index[d.key()] {
			return nil, fmt.Errorf("%w: %s %s (%s)", ErrConflictNotFound, d.TargetKind, d.TargetID, d.Code)
		}
		rows = append(rows, domain.ConflictResolution{
			ProjectID:     in.ProjectID,
			BaseStateID:   in.BaseStateID,
			SourceStateID: in.SourceStateID,
			TargetStateID: in.TargetStateID,
			TargetKind:    d.TargetKind,
			TargetID:      d.TargetID,
			Code:          d.Code,
			Fields:        normalizeStrings(d.Fields),
			PayloadKeys:   normalizeStrings(d.PayloadKeys),
			OtherObjectID: d.OtherObjectID,
			Kind:          d.Kind,
			Note:          d.Note,
			DecidedBy:     actor.ID,
		})
	}
	plan, err := s.store.SavePlan(ctx, SavePlanParams{
		ProjectID:     in.ProjectID,
		BaseStateID:   in.BaseStateID,
		SourceStateID: in.SourceStateID,
		TargetStateID: in.TargetStateID,
		Decisions:     rows,
	})
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return plan, nil
}

// Plan returns the decisions recorded for the triple, in the store's
// stable order. It performs no authorization of its own (package doc).
func (s *Service) Plan(ctx context.Context, projectID, baseStateID, sourceStateID, targetStateID string) ([]domain.ConflictResolution, error) {
	if s.store == nil {
		return nil, fmt.Errorf("%w: resolution service not fully wired", ErrStore)
	}
	if err := validateTriple(projectID, baseStateID, sourceStateID, targetStateID); err != nil {
		return nil, err
	}
	plan, err := s.store.ListPlan(ctx, projectID, baseStateID, sourceStateID, targetStateID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return plan, nil
}

// View composes the resolution UI's read: the conflict report of the
// triple (its changes carry the base/A/B values), the per-side evidence
// context of every conflicted object, and the recorded decisions. It
// performs no authorization of its own (package doc). The same inputs
// always render the same view except the resolution rows themselves (the
// report and evidence are derived from the immutable states).
func (s *Service) View(ctx context.Context, projectID, baseStateID, sourceStateID, targetStateID string) (*View, error) {
	if s.conflicts == nil || s.store == nil {
		return nil, fmt.Errorf("%w: resolution service not fully wired", ErrStore)
	}
	if err := validateTriple(projectID, baseStateID, sourceStateID, targetStateID); err != nil {
		return nil, err
	}
	report, err := s.conflicts.Conflicts(ctx, diffs.Params{
		ProjectID:     projectID,
		BaseStateID:   baseStateID,
		SourceStateID: sourceStateID,
		TargetStateID: targetStateID,
	})
	if err != nil {
		return nil, err
	}
	evidence, err := s.objectEvidence(ctx, report)
	if err != nil {
		return nil, err
	}
	plan, err := s.store.ListPlan(ctx, projectID, baseStateID, sourceStateID, targetStateID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return &View{Report: report, Evidence: evidence, Resolutions: plan}, nil
}

// objectEvidence collects, for every conflicted object, the evidence
// assertions of its source-side head version and its target-side head
// version (evidence pins exact object versions, so the pin itself is the
// side filter). A side the object does not exist on carries no evidence.
func (s *Service) objectEvidence(ctx context.Context, report *conflict.Report) ([]ObjectEvidence, error) {
	changes := make(map[string]*objectChangeRef, len(report.Diff.ObjectChanges))
	for i := range report.Diff.ObjectChanges {
		c := &report.Diff.ObjectChanges[i]
		changes[c.ObjectID] = &objectChangeRef{source: c.SourceVersion.ID, target: nil}
		if c.TargetVersion != nil {
			changes[c.ObjectID].target = &c.TargetVersion.ID
		}
	}
	out := make([]ObjectEvidence, 0)
	for _, v := range report.ObjectVerdicts {
		if len(v.Conflicts) == 0 {
			continue // an auto-mergeable change has no resolution context
		}
		ch, ok := changes[v.ObjectID]
		if !ok {
			return nil, fmt.Errorf("%w: verdict for object %s has no change", ErrStore, v.ObjectID)
		}
		ev := ObjectEvidence{ObjectID: v.ObjectID, SourceEvidence: []EvidenceItem{}, TargetEvidence: []EvidenceItem{}}
		src, err := s.store.ListEvidenceForObjectVersion(ctx, ch.source)
		if err != nil {
			return nil, wrapStoreError(err)
		}
		ev.SourceEvidence = src
		if ch.target != nil {
			tgt, err := s.store.ListEvidenceForObjectVersion(ctx, *ch.target)
			if err != nil {
				return nil, wrapStoreError(err)
			}
			ev.TargetEvidence = tgt
		}
		out = append(out, ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ObjectID < out[j].ObjectID })
	return out, nil
}

// objectChangeRef names the head versions of one conflicted object's
// change (the evidence pins).
type objectChangeRef struct {
	source string
	target *string
}

// conflictKey is the classifier key one decision must match in the
// report. The same key the persistence unique constraint uses — the
// report and the stored row can never disagree about which conflict a
// decision belongs to.
type conflictKey struct {
	kind          domain.ConflictResolutionTargetKind
	id            string
	code          string
	fields        string
	payloadKeys   string
	otherObjectID string
}

func (d Decision) key() conflictKey {
	return conflictKey{
		kind:          d.TargetKind,
		id:            d.TargetID,
		code:          d.Code,
		fields:        joinStrings(d.Fields),
		payloadKeys:   joinStrings(d.PayloadKeys),
		otherObjectID: ptrOrEmpty(d.OtherObjectID),
	}
}

// normalizeStrings returns the canonical form of one identity string set:
// a sorted copy (empty stays empty). It is the single normalization every
// identity consumer shares — the classifier key built by joinStrings AND
// the rows the store persists. The report does not guarantee a field
// order, so without normalizing the stored rows the DB's unique index
// (jsonb arrays compare order-sensitively) would let the same conflict
// submitted with shuffled fields/payload_keys insert a second row.
func normalizeStrings(ss []string) []string {
	if len(ss) == 0 {
		return []string{}
	}
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}

// joinStrings renders the classifier-key join of one identity string set:
// the canonical (sorted) order joined with NUL separators, so the same set
// matches the same key however it arrives.
func joinStrings(ss []string) string {
	sorted := normalizeStrings(ss)
	out := ""
	for i, s := range sorted {
		if i > 0 {
			out += "\x00"
		}
		out += s
	}
	return out
}

func ptrOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// conflictIndex flattens the report into the set of conflicts a decision
// may address. Object conflicts are keyed by object id, relation
// conflicts by relation id; the index mirrors the report's canonical
// verdict order, and both builds are deterministic.
func conflictIndex(report *conflict.Report) map[conflictKey]bool {
	index := make(map[conflictKey]bool)
	for _, v := range report.ObjectVerdicts {
		for _, c := range v.Conflicts {
			index[conflictKey{
				kind:          domain.ConflictResolutionTargetObject,
				id:            v.ObjectID,
				code:          c.Code,
				fields:        joinStrings(c.Fields),
				payloadKeys:   joinStrings(c.PayloadKeys),
				otherObjectID: c.OtherObjectID,
			}] = true
		}
	}
	for _, v := range report.RelationVerdicts {
		for _, c := range v.Conflicts {
			index[conflictKey{
				kind:          domain.ConflictResolutionTargetRelation,
				id:            v.RelationID,
				code:          c.Code,
				fields:        joinStrings(c.Fields),
				payloadKeys:   joinStrings(c.PayloadKeys),
				otherObjectID: c.OtherObjectID,
			}] = true
		}
	}
	return index
}

// requireWrite authorizes one resolution save: resolve the caller's
// membership/role, then evaluate ActionWriteScientificState (the L1
// decision in the package doc). The denial happens before any state
// lookup, so it never discloses whether the states exist.
func (s *Service) requireWrite(ctx context.Context, actor domain.User, projectID string) error {
	role, err := s.projectRole(ctx, actor, projectID)
	if err != nil {
		return err
	}
	decision, err := s.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionWriteScientificState,
		Class:  authz.ClassOf(true, role, false),
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
	if !decision.Permits() {
		return ErrForbidden
	}
	return nil
}

// projectRole resolves the actor's membership role through the project
// gate (the gate runs the project read check itself: a caller who may not
// read the project gets the existence-hiding error before any role is
// decided; a readable project without a membership answers "no role").
func (s *Service) projectRole(ctx context.Context, actor domain.User, projectID string) (*domain.ProjectRole, error) {
	membership, err := s.projects.GetMembership(ctx, actor, projectID)
	switch {
	case err == nil:
		role := membership.Role
		return &role, nil
	case errors.Is(err, projects.ErrMemberNotFound):
		return nil, nil
	default:
		return nil, wrapError(err)
	}
}

// validateSaveInput checks the request's shape: the project, the triple
// and at least one decision are required, and every decision carries a
// valid target kind, a valid decision kind and a target id. An empty plan
// submission is refused — the UI sends decisions, not a "clear".
func validateSaveInput(in SaveInput) error {
	if err := validateTriple(in.ProjectID, in.BaseStateID, in.SourceStateID, in.TargetStateID); err != nil {
		return err
	}
	if len(in.Decisions) == 0 {
		return fmt.Errorf("%w: at least one resolution is required", ErrValidation)
	}
	for _, d := range in.Decisions {
		if !domain.ValidResolutionTargetKind(d.TargetKind) {
			return fmt.Errorf("%w: unknown target kind %q", ErrValidation, d.TargetKind)
		}
		if d.TargetID == "" {
			return fmt.Errorf("%w: target_id is required", ErrValidation)
		}
		if !domain.ValidResolutionKind(d.Kind) {
			return fmt.Errorf("%w: unknown resolution kind %q", ErrValidation, d.Kind)
		}
		if d.Code == "" {
			return fmt.Errorf("%w: conflict code is required", ErrValidation)
		}
	}
	return nil
}

// validateTriple checks the four ids the reads and writes scope by.
func validateTriple(projectID, baseStateID, sourceStateID, targetStateID string) error {
	if projectID == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if baseStateID == "" {
		return fmt.Errorf("%w: base_state_id is required", ErrValidation)
	}
	if sourceStateID == "" {
		return fmt.Errorf("%w: source_state_id is required", ErrValidation)
	}
	if targetStateID == "" {
		return fmt.Errorf("%w: target_state_id is required", ErrValidation)
	}
	return nil
}

// wrapError maps a dependency's error vocabulary onto this package's for
// the transport (docs/45).
func wrapError(err error) error {
	switch {
	case errors.Is(err, projects.ErrProjectNotFound),
		errors.Is(err, projects.ErrMemberNotFound),
		errors.Is(err, ErrForbidden):
		return err
	default:
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
}

// wrapStoreError maps a store failure to ErrStore, passing the
// validation-class errors through unchanged.
func wrapStoreError(err error) error {
	if errors.Is(err, ErrValidation) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
