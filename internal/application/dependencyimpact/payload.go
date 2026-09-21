package dependencyimpact

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/lichman0405/post/internal/events"
)

// This file is the alert's payload contract: the fields, why each is there,
// and the envelope columns that travel beside them.
//
// # Why the payload is designed here at all
//
// This repository does not model event payloads. specs/events/ holds one
// file, event-types.yaml, and it carries the NAMES and the required
// envelope fields (event_id, event_type, occurred_at, actor_id,
// correlation_id, visibility, payload_version) and no payload schema; a
// payload is constructed in Go by the producer that emits it. The task's
// requirement is therefore that this package DESIGN the payload, and that
// every field below be justified rather than accumulated.
//
// # The fields, one by one
//
//   - trigger_event_id, trigger_event_type. The change that caused this
//     analysis, by the research_events row that recorded it. It is here
//     because an alert that does not say what it is alerting about is not
//     actionable, and it is the FIRST field because it is also the
//     analysis's own identity: migration 00111's unique index dedupes the
//     alert on (this, affected_kind, affected_id), so "the same upstream
//     change is not alerted twice" is enforced by the key the payload
//     carries rather than by any state the analysis keeps.
//   - trigger_occurred_at. When the cause happened, as an instant. It is
//     the only time in the message that is not this process's clock: an
//     alert replayed tomorrow must still say when the change it reports
//     was made, and a consumer that sorted alerts by their arrival would
//     otherwise order them by the worker's uptime.
//   - upstream_kind, upstream_id, upstream_version_no. WHAT changed, at
//     the landing point it changed on (SubjectKind). upstream_version_no
//     is present for object triggers and carries the version the trigger
//     named — "version 3 was aborted" is a different, more useful message
//     than "the protocol changed" — and it is OMITTED (not zero) when the
//     trigger named none, so a missing version is visibly missing rather
//     than looking like version zero.
//   - upstream_project_id. The project the change was made in, resolved
//     from the subject rather than copied from the trigger's envelope
//     (the envelope's project_id is the writer's; for an asset published
//     from one project and depended on by another they are the same, but
//     a payload that copies the envelope would break the day they are
//     not). It is what lets a downstream owner go and read the upstream
//     thing — subject to their own access, which is their read gate's
//     business, not this message's.
//   - affected_kind, affected_id, affected_project_id. WHAT IS AFFECTED —
//     the identity the requirement names first. affected_project_id is
//     what the alert is addressed to (the envelope's project_id, below).
//   - affected_object_id, affected_object_type. The affected object's own
//     identity, for object dependents only. A version id or an object id
//     alone is a pointer; the object type is what tells a reader what kind
//     of thing to go and look at, and both are omitted for a project
//     dependent rather than sent empty.
//   - directness, hops. The requirement's direct/indirect distinction, as
//     two fields: the classification a renderer branches on, and the hop
//     count it may display. Both, because a page that wanted "3 hops away"
//     cannot recover it from the word "indirect", and a page that wants a
//     binary cannot be trusted to re-derive it from a number whose cap is
//     this package's business.
//   - review_required. docs/18 §5's 「系统只标记 review required，不自动改科学
//     结论」. It is the alert's whole meaning and it is sent explicitly
//     rather than implied by the event type: the same event type is
//     registered for future producers (dependency.status_changed is its
//     sibling in the vocabulary), and a consumer must not have to know
//     which producer wrote the message to know whether a human is being
//     asked for something. It is always true — this package has no other
//     kind of alert — and a false would be a lie rather than a feature.
//   - payload_version. NOT set here: the envelope's own default injects it
//     (events.withPayloadVersion) so that the envelope field has one owner
//     in this tree. A producer that hand-rolled it would be the second.
//
// # The envelope, which is not the payload
//
// The alert is addressed to the AFFECTED project: envelope project_id is
// impact.ProjectID, so the subscription fan-out's project target resolves
// to the people who own the downstream work — which is who docs/18 §5's
// alert is for. The upstream project is NOT addressed, and that is a
// deliberate asymmetry: an alert addressed to the upstream publisher would
// say "at least one project depends on this", which is exactly the fact
// docs/23 §5 forbids a publisher from learning about a private dependent.
//
// The envelope's visibility is PRIVATE, always, and the reason is that the
// alert names two entities: the affected one and the upstream one. A
// reader may be shown it only when they may read BOTH, and the fan-out's
// audience model resolves access to the event's TARGET alone
// (events.TargetAudience takes the target; the second entity the payload
// names is not consulted). A public envelope would therefore deliver
// "private project Q's object changed" to a non-member subscriber of a
// public affected project. Since the analysis cannot ask the fan-out to
// consult a second project, the only fail-closed answer left is the least
// visible one — and it costs nothing here, because Delivers gives a member
// everything, so the affected project's own members still receive their
// alert (internal/events/subscription.go: "a member relationship:
// everything the target produces").
const (
	// AlertFieldReviewRequired is the payload field docs/18 §5's 「标记
	// review required」 lands in. Exported so a consumer's test and this
	// package's test name the same string.
	AlertFieldReviewRequired = "review_required"
	// AlertFieldDirectness is the payload field carrying Direct/Indirect.
	AlertFieldDirectness = "directness"
	// AlertFieldHops is the payload field carrying the hop count.
	AlertFieldHops = "hops"
	// ReviewRequired is the value the alert's review_required field always
	// carries: docs/18 §5's 「系统只标记 review required，不自动改科学结论」. It
	// is a constant rather than a parameter because there is nothing else
	// this analysis could legitimately say — an alert that did not ask for
	// a review would not be an alert, and a producer that wanted to
	// withdraw one would be a different event type.
	ReviewRequired = true
	// AlertFieldTriggerEventID is the payload field carrying the
	// research_events id of the change that caused the analysis. It is
	// also half of the alert's dedupe key (migration 00111).
	AlertFieldTriggerEventID = "trigger_event_id"
	// AlertFieldAffectedKind and AlertFieldAffectedID are the other half.
	AlertFieldAffectedKind = "affected_kind"
	AlertFieldAffectedID   = "affected_id"
)

// alertPayload is the wire shape. It is a struct rather than a map so that
// the field set is a compile-time fact and a missing field is a build
// error, not a runtime absence.
type alertPayload struct {
	TriggerEventID     string `json:"trigger_event_id"`
	TriggerEventType   string `json:"trigger_event_type"`
	TriggerOccurredAt  string `json:"trigger_occurred_at"`
	UpstreamKind       string `json:"upstream_kind"`
	UpstreamID         string `json:"upstream_id"`
	UpstreamProjectID  string `json:"upstream_project_id,omitempty"`
	UpstreamVersionNo  int    `json:"upstream_version_no,omitempty"`
	AffectedKind       string `json:"affected_kind"`
	AffectedID         string `json:"affected_id"`
	AffectedProjectID  string `json:"affected_project_id,omitempty"`
	AffectedObjectID   string `json:"affected_object_id,omitempty"`
	AffectedObjectType string `json:"affected_object_type,omitempty"`
	Directness         string `json:"directness"`
	Hops               int    `json:"hops"`
	ReviewRequired     bool   `json:"review_required"`
}

// AlertEvent renders the `dependency.impact_detected` event for one
// (trigger, impact) pair.
//
// It takes no "review required" argument because this producer has exactly
// one thing to say: ReviewRequired is a constant of the event type here,
// and a parameter would invite a caller to send the other value — which
// would be a different message, not this one with a flag flipped.
//
// It returns an error for the two things that would make the message a
// lie: a hop count that names no entity, and a payload that will not
// marshal (impossible with this struct, and checked rather than assumed).
// Everything else about the event — validation, payload_version,
// correlation id — is the events package's, because that is where the
// envelope's contract lives.
func AlertEvent(t Trigger, upstreamProjectID string, imp Impact) (events.Event, error) {
	if imp.Hops < 1 {
		return events.Event{}, fmt.Errorf("%w: impact on %s carries hop count %d", ErrValidation, imp.ID, imp.Hops)
	}
	body, err := json.Marshal(alertPayload{
		TriggerEventID:     t.EventID,
		TriggerEventType:   t.EventType,
		TriggerOccurredAt:  t.OccurredAt.UTC().Format(time.RFC3339),
		UpstreamKind:       string(t.Subject.Kind),
		UpstreamID:         t.Subject.ID,
		UpstreamProjectID:  upstreamProjectID,
		UpstreamVersionNo:  t.Subject.VersionNo,
		AffectedKind:       string(imp.Kind),
		AffectedID:         imp.ID,
		AffectedProjectID:  imp.ProjectID,
		AffectedObjectID:   imp.ObjectID,
		AffectedObjectType: imp.ObjectType,
		Directness:         string(imp.Directness),
		Hops:               imp.Hops,
		ReviewRequired:     ReviewRequired,
	})
	if err != nil {
		return events.Event{}, fmt.Errorf("%w: render impact alert payload: %v", ErrStore, err)
	}
	return events.Event{
		EventType: EventImpactDetected,
		// ActorID is deliberately EMPTY: nobody acted. The alert is the
		// platform's own observation, and the envelope's actor column is
		// what the Contribution Ledger reads to attribute an act to a
		// person (docs/13 §1) — the ledger's candidate predicate requires a
		// non-NULL actor, so an alert with no actor can never be recorded
		// as somebody's contribution, which is the correct outcome. The
		// trigger's actor is in the payload's trigger_event_id for anyone
		// who needs to know who made the change.
		ActorID:   "",
		ProjectID: imp.ProjectID,
		// Private: see the file comment — the alert names two entities and
		// the fan-out can only authorize one of them.
		Visibility:    events.VisibilityPrivate,
		CorrelationID: t.CorrelationID,
		Payload:       body,
	}, nil
}
