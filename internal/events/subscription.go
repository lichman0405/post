package events

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The subscription model (T1002, docs/18 §3): a user follows a TARGET and
// chooses which event types reach them (event_filters) and through which
// channels. docs/18 §3 names the five targets — "用户可 follow
// Project/Asset/Knowledge/Person/Organization" — and the same five are the
// dimensions of the network's public surfaces (specs/ui/page-inventory.csv:
// /assets/{id}, /knowledge/{id}, /{user}, /orgs/{org}), so a subscription's
// target_type is that vocabulary and target_id is that surface's stable
// identity.
//
// The model is deliberately one table's worth of vocabulary: this file owns
// the values, their shape rules and the ONE authorization rule the whole
// pipeline shares, and the fan-out owns the delivery decision built on it.
const (
	// Target types.
	TargetTypeProject      = "project"
	TargetTypeAsset        = "asset"
	TargetTypeKnowledge    = "knowledge"
	TargetTypeUser         = "user"
	TargetTypeOrganization = "organization"
)

// Channels are the output interfaces a subscription delivers through
// (docs/18 §4). Only the two the platform owns a surface for are V1
// values: the research inbox (web) and the digest sender's queue (email).
// RSS/Atom is not a per-user channel — it is a public, unauthenticated
// feed of public objects (T1004) — and a signed webhook is a transport
// registration, not a per-target subscription (the endpoint registry of
// T1006, migration 00059), so neither is a value here.
const (
	ChannelWeb   = "web"
	ChannelEmail = "email"
)

// MaxSubscriptionsPerUser bounds the live subscriptions one account may
// hold: a hostile account must not be able to make the fan-out's per-event
// candidate scan unbounded (the same reason the webhook registry caps
// endpoints).
const MaxSubscriptionsPerUser = 200

// Target is one (type, id) pair a subscription points at.
type Target struct {
	Type string
	ID   string
}

// Subscription is one user following one target.
type Subscription struct {
	ID           string
	UserID       string
	TargetType   string
	TargetID     string
	EventFilters []string
	Channels     []string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// DeletedAt is the unsubscribe marker: nil while live.
	DeletedAt *time.Time
}

// Target renders the subscription's target.
func (s Subscription) Target() Target { return Target{Type: s.TargetType, ID: s.TargetID} }

// SubscriptionDelivery is one notification row: the pointer the inbox
// (T1003) and the digest sender (T1005) read. It carries no event content —
// EventID names the append-only research event and the target names the
// subject, so rendering is a read under the renderer's own
// current-authorization gate.
type SubscriptionDelivery struct {
	ID             string
	SubscriptionID string
	UserID         string
	EventID        string
	Channel        string
	EventType      string
	TargetType     string
	TargetID       string
	Status         string
	CreatedAt      time.Time
	DeliveredAt    *time.Time
	CancelledAt    *time.Time
	// ReadAt is when the subscriber read this notification in the web
	// inbox (T1003); nil = unread. The column's CHECK pairs it with the
	// delivered status, so a pending or withdrawn row can never carry one.
	ReadAt *time.Time
}

// Subscription delivery statuses (subscription_deliveries.status).
const (
	// SubscriptionDeliveryPending: in flight for a channel that has a
	// sender (email — the digest sender consumes these).
	SubscriptionDeliveryPending = "pending"
	// SubscriptionDeliveryDelivered: delivered. For the web channel the
	// row IS the delivery (the research inbox is it), so it is born
	// delivered.
	SubscriptionDeliveryDelivered = "delivered"
	// SubscriptionDeliveryCancelled: withdrawn before delivery — the owner
	// unsubscribed, or the fan-out found the subscriber can no longer see
	// the target.
	SubscriptionDeliveryCancelled = "cancelled"
)

// ErrSubscriptionExists: the user already has a live subscription to that
// target (subscriptions_live_uniq).
var ErrSubscriptionExists = fmt.Errorf("events: subscription already exists")

// ErrSubscriptionNotFound: no live subscription with that id owned by that
// user ("exists but not yours" and "does not exist" answer identically —
// the orgs disclosure rule).
var ErrSubscriptionNotFound = fmt.Errorf("events: subscription not found")

// ErrSubscriptionLimit: the owner already holds MaxSubscriptionsPerUser
// live subscriptions.
var ErrSubscriptionLimit = fmt.Errorf("events: subscription limit reached")

// ErrTargetShape: the target's type or id does not have the canonical
// shape.
var ErrTargetShape = fmt.Errorf("events: invalid subscription target")

// uuidShape is PostgreSQL's own uuid text form (lower case, dashed) —
// the form pgtype renders and the columns store.
var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// assetPIDShape mirrors research_assets_pid_format (migration 00064): 26
// Crockford base32 characters, the identity internal/assets.NewPID
// generates and /assets/{pid} is built from.
var assetPIDShape = regexp.MustCompile(`^[0-9a-hjkmnp-tv-z]{26}$`)

// ValidateTargetType reports whether t is one of the five canonical target
// types.
func ValidateTargetType(t string) bool {
	switch t {
	case TargetTypeProject, TargetTypeAsset, TargetTypeKnowledge, TargetTypeUser, TargetTypeOrganization:
		return true
	}
	return false
}

// ValidateTargetID checks the id's shape for its type: a uuid for every
// target addressed by a canonical identifier, the asset pid for an asset.
// The shape is a precondition of the resolution queries — they cast
// project/knowledge/user/organization ids to uuid — so it is checked at
// the boundary rather than discovered as a query error, and the migration
// carries the same rule as a CHECK for sessions that write SQL directly.
func ValidateTargetID(targetType, id string) error {
	switch targetType {
	case TargetTypeAsset:
		if !assetPIDShape.MatchString(id) {
			return fmt.Errorf("%w: asset id must be a 26-character asset pid", ErrTargetShape)
		}
	case TargetTypeProject, TargetTypeKnowledge, TargetTypeUser, TargetTypeOrganization:
		if !uuidShape.MatchString(id) {
			return fmt.Errorf("%w: %s id must be a uuid", ErrTargetShape, targetType)
		}
	default:
		return fmt.Errorf("%w: unknown target type %q", ErrTargetShape, targetType)
	}
	return nil
}

// ValidateTarget checks both axes of a target.
func ValidateTarget(t Target) error {
	if !ValidateTargetType(t.Type) {
		return fmt.Errorf("%w: unknown target type %q", ErrTargetShape, t.Type)
	}
	return ValidateTargetID(t.Type, t.ID)
}

// ValidateChannels checks a subscription's channel list: non-empty, no
// duplicates, every value one of the V1 channels. The order the caller
// sent is preserved (it is the subscription's, not the fan-out's, to
// decide).
func ValidateChannels(channels []string) error {
	if len(channels) == 0 {
		return fmt.Errorf("events: at least one channel is required")
	}
	if len(channels) > 2 {
		return fmt.Errorf("events: too many channels (%d)", len(channels))
	}
	seen := map[string]bool{}
	for _, c := range channels {
		switch c {
		case ChannelWeb, ChannelEmail:
		default:
			return fmt.Errorf("events: unknown channel %q", c)
		}
		if seen[c] {
			return fmt.Errorf("events: duplicate channel %q", c)
		}
		seen[c] = true
	}
	return nil
}

// AudienceLevel is how a user is related to a target right now: none (no
// relationship the platform will route on), public (they may see the
// target because it is public), member (they hold a privileged
// relationship — a project membership, an active organization membership,
// or their own identity).
type AudienceLevel string

const (
	AudienceNone   AudienceLevel = "none"
	AudiencePublic AudienceLevel = "public"
	AudienceMember AudienceLevel = "member"
)

// ValidAudienceLevel reports whether l is one of the three levels.
func ValidAudienceLevel(l AudienceLevel) bool {
	switch l {
	case AudienceNone, AudiencePublic, AudienceMember:
		return true
	}
	return false
}

// Delivers reports whether a subscriber at level l receives an event of
// the given visibility.
//
// This is the whole public/private rule of the subscription pipeline
// (docs/12 §2-3: a private project is invisible by default, an event is
// never more visible than its subject, and visibility is never widened
// without an explicit, audited action):
//
//   - no relationship to the target: nothing, ever. This is what makes a
//     revoked membership stop the flow — the level is resolved per event
//     against current state, never remembered from subscribe time.
//   - a public relationship: only PUBLIC events. A private event (a
//     private branch's commit in a public project) is not theirs.
//   - a member relationship: everything the target produces.
//
// Both levels are computed by the store from live rows, so a target that
// became private, or a membership that ended, changes the answer at the
// next event rather than at the next login.
func Delivers(level AudienceLevel, eventVisibility string) bool {
	switch level {
	case AudienceMember:
		return true
	case AudiencePublic:
		return eventVisibility == VisibilityPublic
	default:
		return false
	}
}

// SubscribesTo reports whether a subscription's event filters admit
// eventType: an empty list admits everything the target addresses,
// otherwise the match is exact (the same convention the webhook
// registry's event_filters use, and the reason an unknown event type
// simply never matches). The canonical vocabulary is
// specs/events/event-types.yaml; it is not duplicated here.
func SubscribesTo(filters []string, eventType string) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		if f == eventType {
			return true
		}
	}
	return false
}

// EventTargets derives the subscription targets one published event
// addresses.
//
// The mapping uses the event's CANONICAL envelope (specs/events/
// event-types.yaml: actor_id, project_id) plus the payload keys a producer
// documents, and it is deliberately closed: an event addresses a target
// only through this function, so a target type with no addressing rule
// simply receives nothing rather than receiving something guessed.
//
//   - project: the envelope's project_id — the project the event happened
//     in (docs/26 §2: project_id travels on the event).
//   - organization: the organization that owns that project. It is
//     resolved from the schema (projects.organization_id) rather than
//     from a payload key, because no producer writes an organization id
//     into an event payload.
//   - user: the envelope's actor_id — "following a person" is following
//     what that person does. The actor is the event's own attribution,
//     not a payload convention.
//   - asset: eventTypeAssetVersionPublished's payload asset_id, the pid
//     its producer documents (internal/persistence/asset_publish_store.go
//     assetVersionPublishedPayload) and the identity /assets/{pid} uses.
//   - knowledge: no producer addresses a published knowledge object yet
//     (T0805 owns that surface), so an event names no knowledge target
//     today and a knowledge subscription receives nothing until one does.
//     The target type is still resolvable and followable (the store's
//     audience query is defined for it) — what is missing is the
//     producer, not the rule.
//
// The organization target needs one lookup; EventTargets therefore returns
// only what the event itself names, and the fan-out appends the
// organization after resolving it (OrganizationTarget).
func EventTargets(eventType, actorID, projectID string, payload []byte) []Target {
	targets := make([]Target, 0, 3)
	if projectID != "" {
		targets = append(targets, Target{Type: TargetTypeProject, ID: projectID})
	}
	if actorID != "" {
		targets = append(targets, Target{Type: TargetTypeUser, ID: actorID})
	}
	for _, key := range eventTargetPayloadKeys[eventType] {
		if id := payloadString(payload, key.Key); id != "" && ValidateTargetID(key.Type, id) == nil {
			targets = append(targets, Target{Type: key.Type, ID: id})
		}
	}
	return targets
}

// payloadTarget names one payload key and the target type it identifies.
type payloadTarget struct {
	Key  string
	Type string
}

// EventTypeAssetVersionPublished is the research_asset.version_published
// research event (specs/events/event-types.yaml), produced by the asset
// publish path.
const EventTypeAssetVersionPublished = "research_asset.version_published"

// eventTargetPayloadKeys is the closed half of the mapping: which payload
// key of which event type names a subscription target. A key that is
// absent, empty or not the target's shape is skipped — an event that does
// not carry the identity addresses no such target, which is the
// fail-closed direction (a subscription is never delivered an event whose
// target the event does not actually name).
var eventTargetPayloadKeys = map[string][]payloadTarget{
	EventTypeAssetVersionPublished: {{Key: "asset_id", Type: TargetTypeAsset}},
}

// payloadString reads one top-level string field of a JSON object payload.
// A payload that is not an object, or a field that is not a string, reads
// as "" — the caller treats that as "not named".
func payloadString(payload []byte, key string) string {
	if len(payload) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return ""
	}
	s, ok := obj[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}
