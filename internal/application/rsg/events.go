package rsg

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// The domain events this service produces (specs/events/event-types.yaml):
// one state.committed per scientific-state write, one
// scientific_object.version_created per object version the write created,
// and — since T0806 — one knowledge.external_evidence_added per
// cross-project evidence assertion. Payloads carry identity/reference only
// (docs/52「Workers」关于 job payload 的口径，事件沿用同一原则) — never
// bulk sensitive data; consumers resolve rows by id.
//
// The external-evidence name is the YAML's. docs/18 §2 spells the same event
// `external_evidence.added` in its prose tour and says itself that the
// vocabulary file is canonical, so the wire name is the YAML's — the
// document is not edited from here.
const (
	eventStateCommitted                 = "state.committed"
	eventScientificObjectVersionCreated = "scientific_object.version_created"
	eventKnowledgeExternalEvidenceAdded = "knowledge.external_evidence_added"
)

// eventVisibility maps a branch's visibility to the event vocabulary:
// docs/12 — an event is never more visible than its subject, and a
// scientific-state write's subject is the branch it committed to. Anything
// but the canonical public value falls back to private (fail closed — the
// branch adapter validates its own values, but the event never guesses).
func eventVisibility(v domain.BranchVisibility) string {
	if v == domain.BranchVisibilityPublic {
		return events.VisibilityPublic
	}
	return events.VisibilityPrivate
}

// recordCommitEvents records the commit's domain events into the outbox
// inside the open transaction: always the state.committed event, plus one
// extra event per semantic write when the caller supplies a builder (the
// object paths append scientific_object.version_created). Fail-closed like
// the audit write: a service wired without a recorder, or a recorder that
// fails, fails the whole commit — an action whose event was silently
// dropped would have lost the event, the one thing the outbox exists to
// prevent.
func (s *Service) recordCommitEvents(ctx context.Context, tx states.Transaction, params states.CommitParams, stateID, visibility string, extra func(stateID string) (events.Event, error)) error {
	if s.events == nil {
		return fmt.Errorf("%w: no outbox recorder configured", ErrStore)
	}
	committed, err := stateCommittedEvent(params, stateID, visibility)
	if err != nil {
		return err
	}
	if err := s.events.Record(ctx, tx, committed); err != nil {
		return err
	}
	if extra != nil {
		e, err := extra(stateID)
		if err != nil {
			return err
		}
		if err := s.events.Record(ctx, tx, e); err != nil {
			return err
		}
	}
	return nil
}

// operationSummary is one commit operation as the state.committed payload
// names it — kind, entity id and log position, the commit_linkage identity.
type operationSummary struct {
	Kind      string `json:"kind"`
	EntityID  string `json:"entity_id"`
	VersionNo int    `json:"version_no"`
}

// stateCommittedEvent builds the state.committed event for one commit.
func stateCommittedEvent(params states.CommitParams, stateID, visibility string) (events.Event, error) {
	ops := make([]operationSummary, 0, len(params.Operations))
	for _, op := range params.Operations {
		ops = append(ops, operationSummary{
			Kind:      string(op.Kind),
			EntityID:  op.EntityID,
			VersionNo: op.VersionNo,
		})
	}
	payload, err := json.Marshal(struct {
		ProjectID   string             `json:"project_id"`
		BranchID    string             `json:"branch_id"`
		StateID     string             `json:"state_id"`
		BaseStateID string             `json:"base_state_id,omitempty"`
		Message     string             `json:"message"`
		Via         string             `json:"via"`
		Gate        string             `json:"gate"`
		Operations  []operationSummary `json:"operations"`
	}{
		ProjectID:   params.ProjectID,
		BranchID:    params.BranchID,
		StateID:     stateID,
		BaseStateID: derefString(params.BaseStateID),
		Message:     params.Message,
		Via:         string(params.Via),
		Gate:        string(params.Gate),
		Operations:  ops,
	})
	if err != nil {
		return events.Event{}, fmt.Errorf("rsg: state.committed payload: %w", err)
	}
	return events.Event{
		EventType:  eventStateCommitted,
		ActorID:    params.ActorID,
		ProjectID:  params.ProjectID,
		Visibility: visibility,
		// The channel is the commit's own declaration (state_commits.via,
		// already in the payload above), so the envelope column and the
		// payload cannot disagree: one source, read twice. It is filled
		// from params.Via rather than spelled "api" here because the
		// declaration belongs to the write path that made the commit
		// (docs/15 §5, T0813 ruling).
		Via:     params.Via,
		Payload: payload,
	}, nil
}

// versionCreatedEvent builds the scientific_object.version_created event
// for one object version written by the commit.
//
// via is the commit's own channel declaration (the same params.Via the
// state.committed event carries): the object version was written BY that
// commit, so the two events of one transition report the same channel.
func versionCreatedEvent(projectID, objectID, objectType, stateID, branchID, actorID, title string, versionNo int, visibility string, via domain.StateVia) (events.Event, error) {
	payload, err := json.Marshal(struct {
		ObjectID   string `json:"object_id"`
		ObjectType string `json:"object_type"`
		VersionNo  int    `json:"version_no"`
		StateID    string `json:"state_id"`
		BranchID   string `json:"branch_id"`
		Title      string `json:"title"`
	}{
		ObjectID:   objectID,
		ObjectType: objectType,
		VersionNo:  versionNo,
		StateID:    stateID,
		BranchID:   branchID,
		Title:      title,
	})
	if err != nil {
		return events.Event{}, fmt.Errorf("rsg: scientific_object.version_created payload: %w", err)
	}
	return events.Event{
		EventType:  eventScientificObjectVersionCreated,
		ActorID:    actorID,
		ProjectID:  projectID,
		Visibility: visibility,
		Via:        via,
		Payload:    payload,
	}, nil
}

// derefString renders a nil *string as "" (the payload omits the field).
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
