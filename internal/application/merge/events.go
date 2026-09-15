package merge

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// The domain event this service produces (specs/events/event-types.yaml:
// `pull_request.merged` — docs/18 §2 names the same event `pr.merged` in
// prose; the machine-readable vocabulary is the spec's, and every consumer
// switches on the spec's spelling). One event per merge, recorded into the
// outbox inside the merge's own transaction (docs/53): the event commits
// with the accepted state or not at all.
//
// The payload carries identity/reference only — the docs/52「Workers」口径 the
// RSG events also follow — never bulk content. It carries enough to LOCATE
// the merge without re-deriving anything: the project (the envelope column
// and the payload), the PR (id and number), the accepted state the merge
// committed and the merge record itself. The Contribution Ledger projection
// (T0807) reads exactly these to build contribution events; that projection
// is its own task and this event does not write contribution rows.
const eventPullRequestMerged = "pull_request.merged"

// mergeEventPayload is the payload of pull_request.merged.
type mergeEventPayload struct {
	// PayloadVersion is the envelope's payload_version; it has no column of
	// its own, so it travels inside the payload (same shape as
	// release.published).
	PayloadVersion int `json:"payload_version"`
	// MergeID is the semantic_merges row this event records.
	MergeID string `json:"merge_id"`
	// PullRequestID and PullRequestNumber locate the merged proposal. The
	// number is per project and is what the wire addresses.
	PullRequestID     string `json:"pull_request_id"`
	PullRequestNumber int64  `json:"pull_request_number"`
	// StateID is the accepted state the merge committed — the state main's
	// head answers with afterwards.
	StateID string `json:"state_id"`
	// TargetBranchID is the branch that advanced.
	TargetBranchID string `json:"target_branch_id"`
	// PlanDigest is the sha256 of the plan's canonical bytes: the reference
	// an auditor recomputes from the recorded triple and decisions.
	PlanDigest string `json:"plan_digest"`
}

// recordMergeEvent writes pull_request.merged into the outbox on the
// commit transaction. Fail-closed like the RSG service's recorder: a
// service wired without a recorder, or a recorder that fails, fails the
// whole merge — an accepted state whose event was silently dropped would
// have lost the one thing the outbox exists to prevent, and the
// Contribution Ledger downstream would never see the merge at all.
func (s *Service) recordMergeEvent(ctx context.Context, tx states.Transaction, p *prepared, actorID, mergeID, stateID string) error {
	if s.events == nil {
		return fmt.Errorf("%w: no outbox recorder configured", ErrStore)
	}
	payload, err := json.Marshal(mergeEventPayload{
		PayloadVersion:    1,
		MergeID:           mergeID,
		PullRequestID:     p.pr.ID,
		PullRequestNumber: p.pr.Number,
		StateID:           stateID,
		TargetBranchID:    p.targetBranch.ID,
		PlanDigest:        p.digest,
	})
	if err != nil {
		return fmt.Errorf("%w: render the merge event payload: %v", ErrStore, err)
	}
	return s.events.Record(ctx, tx, events.Event{
		EventType:  eventPullRequestMerged,
		ActorID:    actorID,
		ProjectID:  p.input.ProjectID,
		Visibility: eventVisibility(p.targetBranch.Visibility),
		Payload:    payload,
	})
}

// eventVisibility maps a branch's visibility to the event vocabulary
// (docs/12: an event is never more visible than its subject, and the
// merge's subject is the branch whose accepted state moved). Anything but
// the canonical public value falls back to private — fail closed: the
// branch adapter validates its own values, and the event never guesses.
func eventVisibility(v domain.BranchVisibility) string {
	if v == domain.BranchVisibilityPublic {
		return events.VisibilityPublic
	}
	return events.VisibilityPrivate
}
