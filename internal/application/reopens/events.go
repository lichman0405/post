package reopens

import (
	"encoding/json"
	"fmt"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// eventScientificObjectReopened is the reopen's domain event. The name is NOT
// coined here: specs/events/event-types.yaml:23 registers
// `scientific_object.reopened` (its sibling `:22` `scientific_object.aborted`
// is T0602's), and this constant spells it exactly. specs/events is not this
// task's to change (the task book's scope note), so the constant is the only
// place the name appears.
const eventScientificObjectReopened = "scientific_object.reopened"

// eventVisibility maps the proposal branch's visibility onto the event
// vocabulary, exactly as the abort write path does
// (internal/application/aborts/events.go): an event is never more visible
// than its subject, the subject here is the branch the version was committed
// to, and anything but the canonical public value falls back to private. Fail
// closed: the adapter validates its own values, but the event never guesses
// one.
func eventVisibility(v domain.BranchVisibility) string {
	if v == domain.BranchVisibilityPublic {
		return events.VisibilityPublic
	}
	return events.VisibilityPrivate
}

// reopenedEvent builds the scientific_object.reopened event for one reopen.
//
// What the payload carries and why it carries only that: identity and
// reference, the same rule every other event follows (docs/52's job-payload
// standard, which docs/18's event vocabulary adopts) — the object, the version
// whose lifecycle the reopen moves, the version that now records it, the
// branch and state it landed on, plus the token-shaped reason code a
// subscriber may reasonably route on. The human explanation is deliberately
// NOT here: it is free text, it is stored where this command requires it (the
// version row and the audit row), and a consumer that needs the words reads
// the row by the version id this payload names.
func reopenedEvent(projectID, objectID, objectType, stateID, branchID, actorID string, reopenedVersionNo, versionNo int, rec domain.ReopenRecord, visibility string) (events.Event, error) {
	payload, err := json.Marshal(struct {
		ObjectID   string `json:"object_id"`
		ObjectType string `json:"object_type"`
		VersionNo  int    `json:"version_no"`
		StateID    string `json:"state_id"`
		BranchID   string `json:"branch_id"`
		// ReopenedVersionNo is the version the reopen MOVES: the row that
		// carries the abort, whose lifecycle this transition leaves as it
		// stands (the reopen appends; it never rewrites) and whose successor
		// is version_no above. Named for what a subscriber looks up, not for
		// what it becomes.
		ReopenedVersionNo int    `json:"reopened_version_no"`
		ReasonCode        string `json:"reason_code"`
	}{
		ObjectID:          objectID,
		ObjectType:        objectType,
		VersionNo:         versionNo,
		StateID:           stateID,
		BranchID:          branchID,
		ReopenedVersionNo: reopenedVersionNo,
		ReasonCode:        rec.ReasonCode,
	})
	if err != nil {
		return events.Event{}, fmt.Errorf("reopens: scientific_object.reopened payload: %w", err)
	}
	return events.Event{
		EventType:  eventScientificObjectReopened,
		ActorID:    actorID,
		ProjectID:  projectID,
		Visibility: visibility,
		Payload:    payload,
	}, nil
}
