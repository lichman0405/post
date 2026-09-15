package merge

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/conflict"
	"github.com/lichman0405/post/internal/rsg/diff"
)

// FormatV1 is the plan format version this package emits.
const FormatV1 = "v1"

// Effect names what the plan does with ONE source-side change. It is the
// machine-readable outcome the merge executor switches on.
type Effect string

const (
	// EffectApply: the source-side content is written into the accepted
	// state (an auto-mergeable change, or "Accept A").
	EffectApply Effect = "apply_source"
	// EffectKeepTarget: the source-side change does not land; the target
	// keeps its own content ("Accept B").
	EffectKeepTarget Effect = "keep_target"
	// EffectCarryBoth: both sides survive. The plan carries the conflict
	// into the accepted state (naming both versions) and writes no new
	// content for the change — no winner is picked, nothing is dropped.
	EffectCarryBoth Effect = "carry_both"
	// EffectAbortProposed: the proposed change is abandoned ("Abort
	// proposed change"). It does not land and is not carried either.
	EffectAbortProposed Effect = "abort_proposed_change"
	// EffectBlocked: the plan cannot execute while this change stands as
	// it does (an undecided conflict, a validation branch, a request for
	// more evidence).
	EffectBlocked Effect = "blocked"
)

// Blocker codes: the stable vocabulary for why a plan is not executable.
const (
	// CodeConflictUndecided: a conflicted change has no decision covering
	// every one of its conflicts. Nothing is inferred for it.
	CodeConflictUndecided = "MERGE_CONFLICT_UNDECIDED"
	// CodeDecisionsDiverge: one change's conflicts carry decisions whose
	// effects contradict each other (Accept A on one conflict and Accept B
	// on another of the same change). The change is a single version row
	// and cannot be written half-way; the merge refuses to invent a
	// precedence instead.
	CodeDecisionsDiverge = "MERGE_CONFLICT_DECISIONS_DIVERGE"
	// CodeUnclassified: the detector refused to vouch for a change yet
	// classified no conflict for it. Blocking beats merging on trust.
	CodeUnclassified = "MERGE_CHANGE_UNCLASSIFIED"
	// CodeValidationBranch: the human asked for a validation branch; the
	// merge into the target waits for it.
	CodeValidationBranch = "MERGE_VALIDATION_BRANCH_REQUIRED"
	// CodeMoreEvidence: the human asked for more evidence; the merge waits.
	CodeMoreEvidence = "MERGE_MORE_EVIDENCE_REQUIRED"
	// CodeNothingToMerge: no source-side change would be written into the
	// accepted state — a merge with an empty result is refused rather than
	// committed as a no-op state.
	CodeNothingToMerge = "MERGE_NOTHING_TO_MERGE"
)

// Withhold reasons: why an otherwise-mergeable change is not materialized.
const (
	// ReasonPublicationGate: the source branch is private and the target
	// branch is public, so writing the content would publish private
	// state. docs/09 §9 defers that to the independent Publication Gate;
	// the merge withholds the change and names it for that gate.
	ReasonPublicationGate = "PUBLICATION_GATE_REQUIRED"
	// ReasonFoundationHeld: a relation pins an object version the merged
	// state would not contain (the object change did not land and the pin
	// is not in the target's lineage), so the edge cannot be materialized
	// faithfully.
	ReasonFoundationHeld = "FOUNDATION_WITHHELD"
)

// Resolution records where a change's treatment came from.
type Resolution string

const (
	// ResolutionAuto: the T0405 detector marked the change auto_mergeable
	// (docs/09 §7); no human decision was needed.
	ResolutionAuto Resolution = "auto"
	// ResolutionUndecided: the change carries conflicts and no complete set
	// of decisions matches them.
	ResolutionUndecided Resolution = "undecided"
)

// Decision is one human decision the plan is computed against. Its
// classifier key is the full key the conflict report carries — the same
// key the resolution store persists (T0407): a decision can only ever
// address the conflict it was recorded for, and a decision that matches no
// reported conflict is refused (ErrDecisionNotInReport) rather than
// silently ignored.
type Decision struct {
	TargetKind    domain.ConflictResolutionTargetKind
	TargetID      string
	Code          string
	Fields        []string
	PayloadKeys   []string
	OtherObjectID string
	Kind          domain.ResolutionKind
	// DecidedBy names the human who decided; the plan records it so the
	// accepted state can say who resolved what (never an agent).
	DecidedBy string
	// Note is the decider's free-form reasoning.
	Note string
}

// Inputs carries everything the plan is a function of: the three states
// (the detector recomputes its report from them), the human decisions and
// the two branch visibilities — the one fact the states do not carry and
// which the docs/09 §9 publication rule needs.
type Inputs struct {
	ProjectID string
	// Diff is the same three-state input diff.Compute and conflict.Detect
	// take; the report is recomputed from it, so the plan and the detector
	// can never disagree about what changed.
	Diff diff.Inputs
	// Decisions is the human resolution plan (docs/09 §8). Empty is legal:
	// a diff with no conflicts needs none, and a diff with conflicts is
	// then planned as blocked, never merged.
	Decisions []Decision
	// SourceVisibility and TargetVisibility are the two branches'
	// visibilities (domain.BranchVisibility). A private source merging into
	// a public target is the docs/09 §9 case: the merge withholds instead
	// of publishing.
	SourceVisibility domain.BranchVisibility
	TargetVisibility domain.BranchVisibility
}

// Change is the plan's treatment of one source-side change.
type Change struct {
	TargetKind domain.ConflictResolutionTargetKind `json:"target_kind"`
	TargetID   string                              `json:"target_id"`
	// ObjectType names the scientific type ("" for relations).
	ObjectType string          `json:"object_type"`
	Kind       diff.ChangeKind `json:"kind"`
	// Resolution is "auto", "undecided", or the decision kind that drove
	// the effect.
	Resolution Resolution `json:"resolution"`
	Effect     Effect     `json:"effect"`
	// Materialize reports whether the merge writes the source-side content
	// into the accepted state. False for every carried, kept, aborted,
	// blocked and withheld change.
	Materialize bool `json:"materialize"`
	// Withheld reports that the change is held back: it is not
	// materialized, and WithholdReason names why.
	Withheld       bool   `json:"withheld"`
	WithholdReason string `json:"withhold_reason,omitempty"`
	// Blocked reports that this change stops the plan from executing; Block
	// names the blocker code.
	Blocked bool   `json:"blocked"`
	Block   string `json:"block,omitempty"`
	// blocks is the blocking decisions behind Blocked, one per distinct
	// decision kind, in the order the conflicts were classified. Not
	// serialized: finish() renders them as the plan's blockers, each with
	// the line that explains it (a code alone tells an operator which rule
	// stopped the merge, never what to lift).
	blocks []planBlock
	// SourceVersionID and TargetVersionID name the exact version rows on
	// each side ("" when a side has none) — the evidence a carried conflict
	// pins.
	SourceVersionID string `json:"source_version_id"`
	TargetVersionID string `json:"target_version_id,omitempty"`
	// EndpointRewrites re-pins the source-side relation version's
	// endpoints: each entry maps an endpoint object-version id onto the
	// object id whose merge-written version must replace it. The executor
	// resolves the object id to the version row it just wrote. Empty for
	// objects, and for relations whose pins the accepted state already
	// contains.
	EndpointRewrites map[string]string `json:"endpoint_rewrites,omitempty"`
	// Conflicts is the detector's classification of this change, verbatim
	// (empty for an auto-mergeable change).
	Conflicts []conflict.Conflict `json:"conflicts"`
}

// Carried is one conflict the plan carries into the accepted state: both
// sides' versions stay, named explicitly, with the human decision that kept
// them (docs/09 §8: keep both versions / explicit coexistence, and
// scientific conflict 可在 main 中保持 contested/unresolved).
type Carried struct {
	TargetKind    domain.ConflictResolutionTargetKind `json:"target_kind"`
	TargetID      string                              `json:"target_id"`
	Code          string                              `json:"code"`
	Category      conflict.ConflictCategory           `json:"category"`
	Fields        []string                            `json:"fields"`
	PayloadKeys   []string                            `json:"payload_keys"`
	OtherObjectID string                              `json:"other_object_id,omitempty"`
	Detail        string                              `json:"detail"`
	// Decision is the human decision that carried the conflict (keep_both,
	// explicit_coexistence or unresolved).
	Decision  domain.ResolutionKind `json:"decision"`
	DecidedBy string                `json:"decided_by"`
	Note      string                `json:"note"`
	// SourceVersionID and TargetVersionID are the two versions that stay.
	SourceVersionID string `json:"source_version_id"`
	TargetVersionID string `json:"target_version_id,omitempty"`
}

// Withheld names one change that will not be materialized.
type Withheld struct {
	TargetKind      domain.ConflictResolutionTargetKind `json:"target_kind"`
	TargetID        string                              `json:"target_id"`
	SourceVersionID string                              `json:"source_version_id"`
	Reason          string                              `json:"reason"`
}

// Blocker names why the plan cannot execute. A blocker on one change stops
// the whole merge: a partial merge would accept a state nobody planned.
type Blocker struct {
	Code     string `json:"code"`
	TargetID string `json:"target_id,omitempty"`
	Detail   string `json:"detail"`
}

// Summary rolls the plan up for the merge record and the PR page.
type Summary struct {
	Applied    int `json:"applied"`
	KeptTarget int `json:"kept_target"`
	Carried    int `json:"carried"`
	Aborted    int `json:"aborted"`
	Blocked    int `json:"blocked"`
	Withheld   int `json:"withheld"`
	// ConflictsByCategory counts the conflicts the report classified, by
	// category name (the docs/09 §6 taxonomy), sorted.
	ConflictsByCategory []CategoryCount `json:"conflicts_by_category"`
}

// CategoryCount is one entry of the per-category roll-up.
type CategoryCount struct {
	Category conflict.ConflictCategory `json:"category"`
	Count    int                       `json:"count"`
}

// Plan is the engine's output: what the merge of these three states under
// these decisions writes into the accepted state. The field order below is
// canonical — it is the serialization order.
type Plan struct {
	FormatVersion string           `json:"format_version"`
	ProjectID     string           `json:"project_id"`
	Base          diff.StateRef    `json:"base"`
	Source        diff.StateRef    `json:"source"`
	Target        diff.StateRef    `json:"target"`
	Report        *conflict.Report `json:"report"`
	Changes       []Change         `json:"changes"`
	Carried       []Carried        `json:"carried_conflicts"`
	Withheld      []Withheld       `json:"withheld"`
	Blockers      []Blocker        `json:"blockers"`
	// Executable reports whether the merge may run: no blockers, and at
	// least one change materialized.
	Executable bool    `json:"executable"`
	Summary    Summary `json:"summary"`

	// applied is the decision each conflict was resolved with, so the
	// carried records and the change effects cannot disagree. Not
	// serialized.
	applied map[classifierKey]Decision
}

// Sentinel errors.
var (
	// ErrInvalidInputs: the inputs cannot form a plan (missing project).
	ErrInvalidInputs = fmt.Errorf("merge: invalid inputs")
	// ErrInvalidDecision: a decision is malformed (unknown kind, missing
	// target, no human decider).
	ErrInvalidDecision = fmt.Errorf("merge: invalid decision")
	// ErrDecisionNotInReport: a decision addresses a conflict the detector
	// did not classify for this triple. Refused, never ignored — a plan
	// that quietly dropped a human decision would hide a scientific
	// disagreement.
	ErrDecisionNotInReport = fmt.Errorf("merge: decision does not address a conflict of this report")
	// ErrDuplicateDecision: two decisions address the same conflict.
	ErrDuplicateDecision = fmt.Errorf("merge: duplicate decision for one conflict")
)

// Merge computes the plan. It is pure: the report is recomputed from the
// three states, the decisions are matched against it, and nothing outside
// the argument is read or written.
func Merge(in Inputs) (*Plan, error) {
	if in.ProjectID == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrInvalidInputs)
	}
	report, err := conflict.Detect(in.Diff)
	if err != nil {
		return nil, fmt.Errorf("merge: detect conflicts: %w", err)
	}
	if len(report.ObjectVerdicts) != len(report.Diff.ObjectChanges) ||
		len(report.RelationVerdicts) != len(report.Diff.RelationChanges) {
		return nil, fmt.Errorf("%w: the report's verdicts do not align with its change list", ErrInvalidInputs)
	}
	applied, err := indexDecisions(report, in.Decisions)
	if err != nil {
		return nil, err
	}
	pl := &planner{
		applied: applied,
		// docs/09 §9: the merge must not carry private state into a public
		// target; the Publication Gate owns that step.
		publishGuard: in.SourceVisibility == domain.BranchVisibilityPrivate &&
			in.TargetVisibility == domain.BranchVisibilityPublic,
	}
	// The object changes first: a relation's endpoints may have to be
	// re-pinned onto the versions the object changes write, so the object
	// outcomes must be known before the relations are planned.
	for i := range report.Diff.ObjectChanges {
		pl.changes = append(pl.changes, pl.objectChange(report.Diff.ObjectChanges[i], report.ObjectVerdicts[i]))
	}
	endpoints := newEndpointIndex(in.Diff, pl.changes)
	for i := range report.Diff.RelationChanges {
		pl.changes = append(pl.changes, pl.relationChange(report.Diff.RelationChanges[i], report.RelationVerdicts[i], endpoints))
	}
	p := &Plan{
		FormatVersion: FormatV1,
		ProjectID:     in.ProjectID,
		Base:          in.Diff.Base,
		Source:        in.Diff.Source,
		Target:        in.Diff.Target,
		Report:        report,
		Changes:       pl.changes,
		applied:       applied,
	}
	p.finish()
	return p, nil
}

// planner carries the state the per-change effects share: the human
// decisions and the publication rule.
type planner struct {
	applied      map[classifierKey]Decision
	publishGuard bool
	changes      []Change
}

// objectChange plans one object change.
func (pl *planner) objectChange(c diff.ObjectChange, v conflict.ObjectVerdict) Change {
	out := Change{
		TargetKind:      domain.ConflictResolutionTargetObject,
		TargetID:        c.ObjectID,
		ObjectType:      c.ObjectType,
		Kind:            c.Kind,
		SourceVersionID: c.SourceVersion.ID,
		Conflicts:       v.Conflicts,
	}
	if c.TargetVersion != nil {
		out.TargetVersionID = c.TargetVersion.ID
	}
	return pl.decideChange(out, v.AutoMergeable)
}

// relationChange plans one relation change. A relation is materialized only
// when both of its endpoints will exist in the accepted state; the endpoint
// index says which pins must be re-pointed at the versions this merge
// writes and which cannot be satisfied at all.
func (pl *planner) relationChange(c diff.RelationChange, v conflict.RelationVerdict, endpoints *endpointIndex) Change {
	out := Change{
		TargetKind:      domain.ConflictResolutionTargetRelation,
		TargetID:        c.RelationID,
		Kind:            c.Kind,
		SourceVersionID: c.SourceVersion.ID,
		Conflicts:       v.Conflicts,
	}
	if c.TargetVersion != nil {
		out.TargetVersionID = c.TargetVersion.ID
	}
	out = pl.decideChange(out, v.AutoMergeable)
	if !out.Materialize {
		return out
	}
	rewrites, reason, ok := endpoints.resolve(c.SourceVersion.SourceObjectVersionID, c.SourceVersion.TargetObjectVersionID)
	if !ok {
		out.Materialize = false
		out.Withheld = true
		out.WithholdReason = reason
		return out
	}
	out.EndpointRewrites = rewrites
	return out
}

// decideChange applies the auto-merge boundary and the human decisions to
// one change that carries no relation-specific state.
func (pl *planner) decideChange(out Change, autoMergeable bool) Change {
	if autoMergeable {
		out.Resolution = ResolutionAuto
		out.Effect = EffectApply
		out.Materialize = true
		return pl.guardPublication(out)
	}
	if len(out.Conflicts) == 0 {
		// The detector refused to vouch for the change but named no
		// conflict. Fail closed: an unexplained change is not merged.
		out.Resolution = ResolutionUndecided
		return pl.block(out, []planBlock{{Code: CodeUnclassified,
			Detail: "the conflict detector did not mark this change auto-mergeable and classified no conflict for it"}})
	}
	effect, blocks := pl.resolve(out.TargetKind, out.TargetID, out.Conflicts)
	out.Resolution = Resolution(pl.decisionKind(out.TargetKind, out.TargetID, out.Conflicts))
	if len(blocks) > 0 {
		out.Resolution = ResolutionUndecided
		return pl.block(out, blocks)
	}
	out.Effect = effect
	switch effect {
	case EffectApply:
		out.Materialize = true
		return pl.guardPublication(out)
	case EffectCarryBoth:
		// finish records both versions in the carried-conflict list, one
		// entry per conflict.
		return out
	default:
		// keep_target and abort_proposed_change both write nothing.
		return out
	}
}

// decisionKind returns the decision kind recorded for the change's first
// conflict — the change's own Resolution label; every conflict keeps its
// own decision in the carried record.
func (pl *planner) decisionKind(kind domain.ConflictResolutionTargetKind, id string, conflicts []conflict.Conflict) domain.ResolutionKind {
	for i := range conflicts {
		if d, ok := pl.applied[keyOf(kind, id, conflicts[i])]; ok {
			return d.Kind
		}
	}
	return ""
}

// planBlock pairs one blocker code with the line explaining it. A change
// carries one per blocking decision that applies to it, because a target
// whose conflicts were sent to a validation branch AND held for more
// evidence stops the plan for BOTH reasons: naming only the first would
// hide a decision the human made, and the blockers are what an operator
// reads to find out what to undo.
type planBlock struct {
	Code   string
	Detail string
}

// resolve computes one change's effect from the decisions covering its
// conflicts. It returns either a single effect, or the blocking decisions
// that stop it (never empty in that case).
//
// A change is one version row: it lands whole or not at all. So every
// conflict of the change must be decided (an undecided one would be merged
// over silently), and the decisions must agree on one effect — Accept A on
// one conflict and Accept B on another of the same row contradict each
// other, and synthesizing a half-source/half-target row is exactly the
// content invention docs/09 §7 forbids. The plan refuses instead of picking
// a precedence.
func (pl *planner) resolve(kind domain.ConflictResolutionTargetKind, id string, conflicts []conflict.Conflict) (Effect, []planBlock) {
	effect := Effect("")
	var firstKind domain.ResolutionKind
	var missing *conflict.Conflict
	var diverged []string
	// blocking collects one entry per DISTINCT blocking decision kind, in
	// the order the detector reported the conflicts — so a change carrying
	// two different blocking decisions is refused for both, and two
	// conflicts decided the same way produce one blocker rather than a
	// duplicate.
	var blocking []planBlock
	seen := make(map[string]bool, len(conflicts))
	for i := range conflicts {
		c := &conflicts[i]
		d, ok := pl.applied[keyOf(kind, id, *c)]
		if !ok {
			if missing == nil {
				missing = c
			}
			continue
		}
		e, valid := effectOf(d.Kind)
		if !valid {
			return EffectBlocked, []planBlock{{Code: CodeConflictUndecided,
				Detail: "conflict " + c.Code + " on " + string(kind) + " " + id +
					" carries an unusable decision kind " + string(d.Kind)}}
		}
		if e == EffectBlocked {
			code := blockerOf(d.Kind)
			if !seen[code] {
				seen[code] = true
				blocking = append(blocking, planBlock{Code: code, Detail: blockerDetail(d.Kind, kind, id)})
			}
		}
		if effect == "" {
			effect, firstKind = e, d.Kind
			continue
		}
		if e != effect {
			diverged = append(diverged, c.Code+":"+string(d.Kind))
		}
	}
	if missing != nil {
		return EffectBlocked, []planBlock{{Code: CodeConflictUndecided,
			Detail: "conflict " + missing.Code + " on " + string(kind) + " " + id +
				" has no human resolution decision (docs/09 §8); the merge never resolves a conflict itself"}}
	}
	if len(diverged) > 0 {
		return EffectBlocked, []planBlock{{Code: CodeDecisionsDiverge,
			Detail: "the conflicts of " + string(kind) + " " + id + " carry decisions that contradict each other (" +
				string(firstKind) + " vs " + joinSorted(diverged) +
				"); one version row cannot be written half-way, and the merge does not choose a precedence"}}
	}
	if effect == EffectBlocked {
		// Every decision blocks the change: name each one, not just the
		// first (a plan reporting one of two blocking decisions would read
		// as if lifting that one were enough).
		return EffectBlocked, blocking
	}
	return effect, nil
}

// guardPublication applies the docs/09 §9 rule to a change that would
// otherwise be materialized: a private source merging into a public target
// is not published by the merge. The change is withheld instead — the
// accepted state does not carry private content, and the plan names it for
// the Publication Gate.
func (pl *planner) guardPublication(out Change) Change {
	if pl.publishGuard {
		out.Materialize = false
		out.Withheld = true
		out.WithholdReason = ReasonPublicationGate
	}
	return out
}

// block marks a change as blocking the plan, carrying the blocking
// decisions that stopped it (one per distinct decision kind, never empty).
// Block is the serialized single code — the canonical first of the list —
// while the whole list is what finish() renders as blockers.
func (pl *planner) block(out Change, blocks []planBlock) Change {
	out.Effect = EffectBlocked
	out.Blocked = true
	if len(blocks) > 0 {
		out.Block = blocks[0].Code
	}
	out.blocks = blocks
	out.Materialize = false
	return out
}

// effectOf maps one decision kind onto its structural effect (package doc).
// The bool is false for a kind this build does not implement.
func effectOf(k domain.ResolutionKind) (Effect, bool) {
	switch k {
	case domain.ResolutionAcceptSource:
		return EffectApply, true
	case domain.ResolutionAcceptTarget:
		return EffectKeepTarget, true
	case domain.ResolutionKeepBoth, domain.ResolutionExplicitCoexistence, domain.ResolutionUnresolved:
		return EffectCarryBoth, true
	case domain.ResolutionAbortChange:
		return EffectAbortProposed, true
	case domain.ResolutionValidationBranch, domain.ResolutionRequestEvidence:
		return EffectBlocked, true
	}
	return "", false
}

// blockerOf maps a blocking decision kind onto its blocker code.
func blockerOf(k domain.ResolutionKind) string {
	switch k {
	case domain.ResolutionValidationBranch:
		return CodeValidationBranch
	case domain.ResolutionRequestEvidence:
		return CodeMoreEvidence
	}
	return CodeConflictUndecided
}

// blockerDetail renders the human-readable line for a blocking decision.
func blockerDetail(k domain.ResolutionKind, target domain.ConflictResolutionTargetKind, id string) string {
	switch k {
	case domain.ResolutionValidationBranch:
		return "the human sent " + string(target) + " " + id +
			" to a validation branch (docs/09 §8): the combined content is validated before it can reach the target"
	case domain.ResolutionRequestEvidence:
		return "the human requested more evidence for " + string(target) + " " + id +
			" (docs/09 §8): the proposal may not land while that decision stands"
	}
	return string(target) + " " + id + " cannot be merged as decided"
}

// finish computes the carried-conflict records, the withheld list, the
// blockers and the roll-ups, in the canonical order the plan serializes.
func (p *Plan) finish() {
	for _, c := range p.Changes {
		if c.Effect == EffectCarryBoth {
			p.Carried = append(p.Carried, p.carriedOf(c)...)
		}
		if c.Withheld {
			p.Withheld = append(p.Withheld, Withheld{
				TargetKind:      c.TargetKind,
				TargetID:        c.TargetID,
				SourceVersionID: c.SourceVersionID,
				Reason:          c.WithholdReason,
			})
		}
		if c.Blocked {
			// Every blocking decision this change carries gets its own
			// blocker entry, with the line that explains it. One entry per
			// decision, never one per change: a change held by a validation
			// branch AND by an evidence request stops the merge for two
			// reasons, and an operator has to lift both.
			for _, b := range c.blockers() {
				p.Blockers = append(p.Blockers, Blocker{Code: b.Code, TargetID: c.TargetID, Detail: b.Detail})
			}
		}
	}
	if p.Carried == nil {
		p.Carried = []Carried{}
	}
	if p.Withheld == nil {
		p.Withheld = []Withheld{}
	}
	if p.Blockers == nil {
		p.Blockers = []Blocker{}
	}
	for i := range p.Changes {
		switch p.Changes[i].Effect {
		case EffectApply:
			if p.Changes[i].Withheld {
				p.Summary.Withheld++
			} else {
				p.Summary.Applied++
			}
		case EffectKeepTarget:
			p.Summary.KeptTarget++
		case EffectCarryBoth:
			p.Summary.Carried++
		case EffectAbortProposed:
			p.Summary.Aborted++
		case EffectBlocked:
			p.Summary.Blocked++
		}
	}
	p.Summary.ConflictsByCategory = categoryCounts(p.Report)
	if p.Summary.Applied == 0 && len(p.Blockers) == 0 {
		// Nothing would land: refusing beats committing an empty state
		// that claims a PR was merged.
		p.Blockers = append(p.Blockers, Blocker{
			Code:   CodeNothingToMerge,
			Detail: "no source-side change would be written into the accepted state",
		})
	}
	sort.SliceStable(p.Blockers, func(i, j int) bool {
		if p.Blockers[i].TargetID != p.Blockers[j].TargetID {
			return p.Blockers[i].TargetID < p.Blockers[j].TargetID
		}
		return p.Blockers[i].Code < p.Blockers[j].Code
	})
	p.Executable = len(p.Blockers) == 0
}

// blockers returns the blocking decisions recorded for one change. A change
// marked blocked always carries them (block() sets both); the fallback
// covers a Change built by hand with only the serialized code, so the
// plan's blocker list is never silently empty for a blocked change.
func (c Change) blockers() []planBlock {
	if len(c.blocks) > 0 {
		return c.blocks
	}
	if c.Block == "" {
		return []planBlock{{Code: CodeConflictUndecided, Detail: "the change is blocked"}}
	}
	return []planBlock{{Code: c.Block, Detail: c.Block}}
}

// carriedOf renders the carried-conflict records of one change: one entry
// per conflict, naming both sides' versions and the decision that kept
// them. The conflict order is the detector's canonical order.
func (p *Plan) carriedOf(c Change) []Carried {
	out := make([]Carried, 0, len(c.Conflicts))
	for i := range c.Conflicts {
		cf := c.Conflicts[i]
		d, ok := p.applied[keyOf(c.TargetKind, c.TargetID, cf)]
		if !ok {
			// Unreachable: a change is carried only when every one of its
			// conflicts is decided. Skipping beats inventing a decision.
			continue
		}
		out = append(out, Carried{
			TargetKind:      c.TargetKind,
			TargetID:        c.TargetID,
			Code:            cf.Code,
			Category:        cf.Category,
			Fields:          append([]string{}, cf.Fields...),
			PayloadKeys:     append([]string{}, cf.PayloadKeys...),
			OtherObjectID:   cf.OtherObjectID,
			Detail:          cf.Detail,
			Decision:        d.Kind,
			DecidedBy:       d.DecidedBy,
			Note:            d.Note,
			SourceVersionID: c.SourceVersionID,
			TargetVersionID: c.TargetVersionID,
		})
	}
	return out
}

// CanonicalJSON renders the plan in its canonical form: fixed field order,
// every list in the order finish left it. Two plans over the same inputs
// marshal to the same bytes.
func (p *Plan) CanonicalJSON() ([]byte, error) {
	return json.Marshal(p)
}

// Materialized lists the change ids the merge writes into the accepted
// state, in the plan's order: the executor's work list.
func (p *Plan) Materialized() []Change {
	out := make([]Change, 0, len(p.Changes))
	for _, c := range p.Changes {
		if c.Materialize {
			out = append(out, c)
		}
	}
	return out
}

// categoryCounts rolls the report's conflicts up by category, sorted by
// category name.
func categoryCounts(r *conflict.Report) []CategoryCount {
	out := make([]CategoryCount, 0, len(r.Summary.ConflictsByCategory))
	for _, c := range r.Summary.ConflictsByCategory {
		out = append(out, CategoryCount{Category: c.Category, Count: c.Count})
	}
	return out
}

// joinSorted renders a sorted, comma-joined list — the deterministic form
// of a message built from a set.
func joinSorted(ss []string) string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return strings.Join(out, ", ")
}
