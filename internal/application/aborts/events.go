package aborts

import (
	"encoding/json"
	"fmt"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// eventScientificObjectAborted is the abort's domain event. The name is
// NOT coined here: specs/events/event-types.yaml:22 registers
// `scientific_object.aborted`, and this constant spells it exactly. The
// sibling `scientific_object.reopened` (:23) belongs to T0610; nothing in
// this package emits it.
const eventScientificObjectAborted = "scientific_object.aborted"

// eventVisibility maps the proposal branch's visibility onto the event
// vocabulary, exactly as the RSG write path does
// (internal/application/rsg/events.go): an event is never more visible than
// its subject, the subject here is the branch the version was committed to,
// and anything but the canonical public value falls back to private. Fail
// closed: the adapter validates its own values, but the event never guesses
// one.
func eventVisibility(v domain.BranchVisibility) string {
	if v == domain.BranchVisibilityPublic {
		return events.VisibilityPublic
	}
	return events.VisibilityPrivate
}

// abortedEvent builds the scientific_object.aborted event for one abort.
//
// What the payload carries and why it carries only that: identity and
// reference, the same rule every other event follows (docs/52's job-payload
// standard, which docs/18's event vocabulary adopts) — the object, the
// version that was aborted, the version that now records it, the branch and
// state it landed on, plus the two token-shaped fields a subscriber may
// reasonably route on. The human explanation is deliberately NOT here: it is
// free text, it is stored where docs/46:7 requires it (the version row and
// the audit row), and a consumer that needs the words reads the row by the
// version id this payload names.
func abortedEvent(projectID, objectID, objectType, stateID, branchID, actorID string, abortedVersionNo, versionNo int, rec domain.AbortRecord, visibility string) (events.Event, error) {
	payload, err := json.Marshal(struct {
		ObjectID       string `json:"object_id"`
		ObjectType     string `json:"object_type"`
		VersionNo      int    `json:"version_no"`
		StateID        string `json:"state_id"`
		BranchID       string `json:"branch_id"`
		AbortedVersion int    `json:"aborted_version_no"`
		ReasonCode     string `json:"reason_code"`
		ReplacementRef string `json:"replacement_ref,omitempty"`
	}{
		ObjectID:       objectID,
		ObjectType:     objectType,
		VersionNo:      versionNo,
		StateID:        stateID,
		BranchID:       branchID,
		AbortedVersion: abortedVersionNo,
		ReasonCode:     rec.ReasonCode,
		ReplacementRef: rec.ReplacementRef,
	})
	if err != nil {
		return events.Event{}, fmt.Errorf("aborts: scientific_object.aborted payload: %w", err)
	}
	return events.Event{
		EventType:  eventScientificObjectAborted,
		ActorID:    actorID,
		ProjectID:  projectID,
		Visibility: visibility,
		Payload:    payload,
	}, nil
}
