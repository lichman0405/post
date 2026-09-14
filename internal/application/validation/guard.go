package validation

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lichman0405/post/internal/domain"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Guard is the server-side re-validation of a state commit (docs/22 §7:
// "command 再次 server validate"): it runs the commit's gate inside the
// commit transaction, after the semantic writes, over the rows as
// persisted. Whatever the caller validated beforehand — the :validate
// endpoint, a client-side precheck, a previous commit — is advisory: the
// server checks the truth it is about to keep, and a blocked gate rolls
// the whole transition back.
type Guard struct {
	val   *rsgvalidation.Validator
	probe TxProbe
}

// NewGuard wires the guard over the gate engine and the transaction probe.
func NewGuard(val *rsgvalidation.Validator, probe TxProbe) *Guard {
	return &Guard{val: val, probe: probe}
}

// RequireCommitGate runs gate over the commit's post-write truth and
// returns *rsgvalidation.GateBlockedError when the gate blocks. The caller
// (the states service) invokes it as the last step inside the commit
// transaction: every row the guard checks — the new state, the new members,
// the branch's whole chain — is read through tx, and the commit row itself
// is assembled from facts (it is inserted after the guard ran).
func (g *Guard) RequireCommitGate(ctx context.Context, tx TxQuerier, gate rsgvalidation.Gate, facts CommitFacts) error {
	if !rsgvalidation.ValidCommitGate(gate) {
		return fmt.Errorf("%w: gate %q cannot guard a commit (commit gates: draft, pr, main)", ErrValidation, gate)
	}
	states, err := g.probe.ListStatesTx(ctx, tx, facts.BranchID)
	if err != nil {
		return fmt.Errorf("%w: read branch states: %v", ErrStore, err)
	}
	commits, err := g.probe.ListCommitsTx(ctx, tx, facts.BranchID)
	if err != nil {
		return fmt.Errorf("%w: read branch commits: %v", ErrStore, err)
	}
	var head *domain.ProjectState
	for i := range states {
		if states[i].ID == facts.ResultStateID {
			head = &states[i]
			break
		}
	}
	if head == nil {
		return fmt.Errorf("%w: result state %s is not on branch %s (the commit's own state must be in the chain)", ErrStore, facts.ResultStateID, facts.BranchID)
	}
	seenObjects := make(map[string]bool)
	seenRelations := make(map[string]bool)
	var objects []domain.ScientificObjectVersion
	var relations []domain.RelationVersion
	for _, st := range states {
		ovs, err := g.probe.ListStateObjectVersionsTx(ctx, tx, st.ID)
		if err != nil {
			return fmt.Errorf("%w: read state %s object versions: %v", ErrStore, st.ID, err)
		}
		for _, ov := range ovs {
			if !seenObjects[ov.ID] {
				seenObjects[ov.ID] = true
				objects = append(objects, ov)
			}
		}
		rvs, err := g.probe.ListStateRelationVersionsTx(ctx, tx, st.ID)
		if err != nil {
			return fmt.Errorf("%w: read state %s relation versions: %v", ErrStore, st.ID, err)
		}
		for _, rv := range rvs {
			if !seenRelations[rv.ID] {
				seenRelations[rv.ID] = true
				relations = append(relations, rv)
			}
		}
	}
	// The in-flight commit: the commit row does not exist yet (the guard
	// runs before its insert), so it is assembled from the facts the states
	// service already validated — the exact operations that will be stored
	// verbatim on the row.
	summary, err := json.Marshal(facts.Operations)
	if err != nil {
		return fmt.Errorf("%w: marshal operation summary: %v", ErrValidation, err)
	}
	inFlight := domain.StateCommit{
		ID:               "in-flight",
		ProjectID:        facts.ProjectID,
		BranchID:         facts.BranchID,
		BaseStateID:      facts.BaseStateID,
		ResultStateID:    facts.ResultStateID,
		ActorID:          facts.ActorID,
		OperationSummary: summary,
	}
	report := g.val.Validate(gate, rsgvalidation.Snapshot{
		ProjectID:        facts.ProjectID,
		BranchID:         facts.BranchID,
		Head:             head,
		States:           states,
		Commits:          append(commits, inFlight),
		ObjectVersions:   objects,
		RelationVersions: relations,
	})
	if report.Blocked() {
		return &rsgvalidation.GateBlockedError{Report: report}
	}
	return nil
}
