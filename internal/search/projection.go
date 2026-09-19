package search

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The search projection's decision surface (T0901, docs/14 §2): which domain
// events become search_documents rows, and which entity each event's payload
// identity addresses.
//
// This file is the projection's table and its two fail-closed rules, and it
// reads no database: the source queries live in sources.go, the consumer in
// projector.go, the rebuild in rebuild.go. Keeping the decision here means
// "what is searchable" is one screen long and readable top to bottom, the
// shape internal/contribution/ledger.go uses for the Contribution Ledger.
//
// # The event vocabulary is the spec's, and it is closed here on purpose
//
// The event names are the machine-readable ones from
// specs/events/event-types.yaml, and every producer in this repository
// already spells them that way. The table below is the COMPLETE answer to
// "which events project a document": an event whose type has no row here
// projects nothing, and the consumer counts it (PassReport.Unmapped) and
// logs it once — the same "counted, never projected" treatment T0807 gave
// the ledger's coverage gap. An event that addressed a document through a
// rule this file does not have would be a silent hole; a counted one is a
// standing fact.
//
// # Where the row's content comes from
//
// A rule declares the payload key that IDENTIFIES the entity and nothing
// else. Everything a document carries — title, content, structured, and the
// visibility — is read from the entity's own row(s) at projection time, not
// from the event payload: the payload of a domain event carries identity and
// references by convention (docs/52「Workers」, internal/events.Event), and a
// projection that rendered its text out of an event would be rendering a
// historical message rather than the state of the thing it indexes.
//
// That choice also makes the incremental path and the rebuild agree by
// construction: both call the same source query and the same scan, so
// "rebuild it from the sources" and "replay the events into it" write the
// same rows.
//
// # Visibility is copied from the source's own axis, and defaults to private
//
// search_documents.visibility is the one column the read query
// (internal/persistence/queries/search.sql: SearchDocuments) trusts: a row
// is returned to anyone when it says 'public', and only to a caller whose
// project scope lists it otherwise. So the value is decided with the same
// fail-closed conjunction the platform decides every other access question
// with (docs/12 §3: an event is never more visible than its subject; docs/54
// ranks a private project's content appearing in Search as its top-severity
// scenario):
//
//   - the entity's OWN axis must permit the network, and
//   - the project that owns it must be public.
//
// Anything else — including "the axis could not be read", an unknown token,
// a NULL where a value was expected — is VisibilityPrivate. There is
// deliberately no path that answers "uncertain, so public", which is why the
// decision is a conjunction and not a lookup: the default branch of
// projectedVisibility IS the fail-closed answer.
//
// The column carries no CHECK constraint (migration 00013), so this rule is
// the only thing standing between a mis-derived value and a leak; the unit
// suite pins the default branch directly.

// The entity_type a projected document carries, and the prefix of its
// entity_ref. The vocabulary is the subscription target vocabulary
// (internal/events: "asset", "knowledge", plus the project-scoped release
// and state), so a document and a follow name the same kind of thing.
const (
	// EntityAsset is a research asset (research_assets), addressed by its
	// pid — the identity /assets/{pid} is built from.
	EntityAsset = "asset"
	// EntityKnowledge is a published knowledge object
	// (knowledge_publications), addressed by its pid — the identity
	// /api/v1/knowledge/{pid} is built from.
	EntityKnowledge = "knowledge"
	// EntityRelease is a release (releases), addressed by its row id: a
	// release has no public identifier of its own, and the releases surface
	// is project-scoped.
	EntityRelease = "release"
	// EntityState is an accepted project state (project_states), addressed
	// by its row id — the same identity a commit, a branch and a merge
	// record name a state by.
	EntityState = "state"
)

// EntityRef renders the canonical "kind:value" reference a document is keyed
// by — the shape research_asset_versions.origin_refs (00064) and the
// contribution ledger's refs already use. The identity is the entity's own:
// a pid where the platform publishes one (asset, knowledge), a row id
// otherwise (release, state).
func EntityRef(entityType, identity string) string {
	return entityType + ":" + identity
}

// The event types the projection maps, spelled as
// specs/events/event-types.yaml spells them and as their producers write
// them. They are constants rather than literals so a call site cannot typo
// one into a silent no-op.
const (
	// EventTypeAssetVersionPublished: internal/persistence/asset_publish_store.go.
	EventTypeAssetVersionPublished = "research_asset.version_published"
	// EventTypeKnowledgeVersionPublished: internal/persistence/knowledge_publish_store.go.
	EventTypeKnowledgeVersionPublished = "knowledge.version_published"
	// EventTypeReleasePublished: internal/persistence/release_store.go.
	EventTypeReleasePublished = "release.published"
	// EventTypeStateCommitted: internal/application/rsg/events.go.
	EventTypeStateCommitted = "state.committed"
)

// projectionRule is one row of the table: the entity one event type
// addresses, and the payload key its identity travels in.
type projectionRule struct {
	// EventType is the canonical event name.
	EventType string
	// EntityType is the kind of document the event projects.
	EntityType string
	// PayloadKey is the payload member naming the entity. It is the key the
	// event's own producer writes, named on each row below; a payload that
	// does not carry it (absent, empty, not a string) addresses no entity,
	// and the event projects nothing — the fail-closed direction, and the
	// same one internal/events EventTargets takes for subscription targets.
	PayloadKey string
	// Why is the citation for this row: the producer whose payload pins the
	// key. Every row needs one — a key nobody writes is a guess.
	Why string
}

// projectionRules is the projection's table, in declaration order (the
// order Rebuild scans, and the order Rules reports).
var projectionRules = []projectionRule{
	{
		EventType:  EventTypeStateCommitted,
		EntityType: EntityState,
		PayloadKey: "state_id",
		Why: "internal/application/rsg/events.go (stateCommittedEvent): the payload's state_id is " +
			"the state this commit produced, the same id the audit row and the commit linkage name.",
	},
	{
		EventType:  EventTypeReleasePublished,
		EntityType: EntityRelease,
		PayloadKey: "release_id",
		Why: "internal/persistence/release_store.go (releaseEventPayload): release_id is the " +
			"releases row id. A release has no pid — its surface is project-scoped.",
	},
	{
		EventType:  EventTypeAssetVersionPublished,
		EntityType: EntityAsset,
		PayloadKey: "asset_id",
		Why: "internal/persistence/asset_publish_store.go (assetVersionPublishedPayload): asset_id " +
			"carries the asset's pid, the identity /assets/{pid} resolves (00064).",
	},
	{
		EventType:  EventTypeKnowledgeVersionPublished,
		EntityType: EntityKnowledge,
		PayloadKey: "publication_id",
		Why: "internal/persistence/knowledge_publish_store.go (knowledgeVersionPublishedPayload): " +
			"publication_id carries the publication's pid, the identity /api/v1/knowledge/{pid} " +
			"resolves (00083). The publication's row uuid never leaves the process.",
	},
}

// RuleFor returns the rule for an event type; ok is false when the event
// projects no document (the consumer counts those rather than dropping them
// silently).
func RuleFor(eventType string) (projectionRule, bool) {
	for _, r := range projectionRules {
		if r.EventType == eventType {
			return r, true
		}
	}
	return projectionRule{}, false
}

// mappedEventTypes returns every event type the table maps, sorted. It is
// what the projection's coverage is asserted against, so the declared and
// the test-pinned vocabularies cannot drift.
func mappedEventTypes() []string {
	out := make([]string, 0, len(projectionRules))
	for _, r := range projectionRules {
		out = append(out, r.EventType)
	}
	sort.Strings(out)
	return out
}

// The canonical visibility values. They are the ones the projects/branches
// surfaces use and the ones internal/events validates its events against —
// search_documents.visibility is the same vocabulary, and the read query
// compares against the literal 'public'.
const (
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"
)

// Document is one row of the projection: everything
// UpsertSearchDocument writes except the embedding, which is deliberately
// left NULL (vector retrieval is T0902's surface, not this task's).
type Document struct {
	// EntityRef is the row's primary key: EntityRef(EntityType, identity).
	EntityRef string
	// EntityType is one of the Entity* constants.
	EntityType string
	// Visibility is the fail-closed answer projectedVisibility produced.
	Visibility string
	// ProjectID is the owning project's id (uuid text). Every projected
	// entity belongs to exactly one project; an empty value would mean the
	// row could never be scoped to anybody, which is why no source query
	// can produce one.
	ProjectID string
	// Title is the entity's own title.
	Title string
	// Content is the entity's searchable text, assembled from its source
	// row(s) — see each source in sources.go for which fields and in which
	// order.
	Content string
	// Structured is the identity/facet JSON object. It carries references
	// and facets only: no content, no score, no rank (CLAUDE.md §9.13).
	Structured []byte
}

// projectedVisibility is the ONE place the projected visibility is decided.
//
// ownAxisPublic is the entity's own visibility axis read as "may the network
// see this" (an asset version's visibility, a branch's visibility for a
// state, and for a knowledge publication the three-axis AudienceFor answer).
// projectVisibility is projects.visibility of the project that owns it.
//
// The answer is public only when BOTH say so. Everything else is private:
// an unknown token, an empty string, a future value nobody taught this
// function about. That default branch is the whole point — search_documents
// carries no CHECK constraint on the column, so a wrong answer here is a
// leak, and the only safe default is the closed one.
func projectedVisibility(ownAxisPublic bool, projectVisibility string) string {
	if ownAxisPublic && projectVisibility == VisibilityPublic {
		return VisibilityPublic
	}
	return VisibilityPrivate
}

// payloadIdentityFor reads the identity a rule names out of an event payload
// and checks it against the entity's own identity shape.
//
// A payload that is not a JSON object, a missing key, a null, a non-string
// value or a string of the wrong shape reads as "" — "the event does not
// name this entity" — and the caller projects nothing. The shape check is
// the same one the read queries' casts require (internal/events makes the
// identical check for subscription targets, and for the identical reason: a
// value that cannot be the identity it claims to be is not an identity).
func (r projectionRule) payloadIdentityFor(payload []byte) string {
	id := payloadString(payload, r.PayloadKey)
	if id == "" {
		return ""
	}
	if !validIdentity(r.EntityType, id) {
		return ""
	}
	return id
}

// validIdentity reports whether id has the shape its entity type's identity
// has. A pid is 26 Crockford base32 characters (research_assets_pid_format,
// 00064; knowledge_publications_pid_format, 00083); a release and a state
// are addressed by a uuid.
func validIdentity(entityType, id string) bool {
	switch entityType {
	case EntityAsset, EntityKnowledge:
		return pidShape.MatchString(id)
	case EntityRelease, EntityState:
		return uuidShape.MatchString(id)
	}
	return false
}

// payloadString reads one top-level string field of a JSON object payload.
// Anything else (empty, malformed, a non-object, a non-string field) reads
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

// structuredFacets renders the facet object a document's structured column
// holds: the entity type, the owning project, and the identity/facets the
// entity's own source row carries. It marshals a map, so the keys are
// sorted and the bytes are stable — a rebuild and an incremental projection
// of the same state write byte-identical facets.
func structuredFacets(entityType, projectID string, facets map[string]string) ([]byte, error) {
	out := make(map[string]string, len(facets)+2)
	for k, v := range facets {
		out[k] = v
	}
	out["entity_type"] = entityType
	out["project_id"] = projectID
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("search: encode structured facets for %s: %w", entityType, err)
	}
	return encoded, nil
}
