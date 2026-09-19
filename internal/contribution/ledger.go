package contribution

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The Contribution Ledger projection (docs/13 §1): the append-only ledger of
// objective events, built from the domain event log.
//
// This file is the projection's decision surface and nothing else: which
// domain events become ledger rows, what each row is tagged with, and what
// the row refers to. It reads no database and writes no SQL — the store
// (internal/persistence) executes the rows this file describes, and the
// service (internal/application/contribution) drives both.
//
// # The event vocabulary is the spec's, and it is open
//
// The event names are the machine-readable ones from
// specs/events/event-types.yaml — the single source of truth for event
// names, and the vocabulary every producer in this repository already
// spells. docs/13 §1 lists the ledger's categories in prose ("created
// object、proposed hypothesis、performed experiment、... 等") and ends the
// list with 等, so that list is NOT a closed switch: the table below is
// explicit and central (one row per event, readable top to bottom), and an
// event with no row here is not silently dropped — the projection counts it
// and logs it (Report.Unmapped), so "the ledger is not recording this kind
// of event" is a visible standing fact rather than a hole.
//
// # What a row records, and what it refuses to
//
// A ledger row records what the event said: the actor, the project, the
// channel (via), the timestamp, the roles docs/04 §4 defines, and whether
// the event is an accepted-contribution or release-inclusion fact. It
// carries NO score, weight, count, rank or aggregate of any kind — not even
// a convenience one for rendering — because docs/13 §4 forbids a single
// reputation score, docs/13 §6 refuses raw commit/experiment counts as
// quality proxies and CLAUDE.md §9 invariant 13 forbids a Truth Score. A
// Profile derives its several dimensions from these facts (docs/13 §4);
// that derivation is not this projection's job and must not be smuggled
// into a column.
//
// # Re-projecting is normal
//
// The projection is a rebuildable read of an append-only log (docs/21 §5),
// so running it again — after a crash, a redeploy, a backfill, or by
// accident — is expected rather than exceptional. Idempotency is the
// database's: every row pins its source event (re.research_event_id) under
// the partial unique index contribution_events_research_event_uniq, and the
// insert is ON CONFLICT DO NOTHING, so a second pass over the same event
// adds nothing (the same shape 00046 gave the outbox publish step).

// LedgerRefKind names the entity class a ledger row's object_refs element
// points at. The text form of a ref is "kind:value" — the canonical
// reference shape research_asset_versions.origin_refs already uses (00064,
// internal/assets). Every value is an internal identity as the event's
// payload spells it (a uuid in text form, or the 26-character pid an asset
// is addressed by).
type LedgerRefKind string

const (
	// RefObject names a scientific object (the container row).
	RefObject LedgerRefKind = "object"
	// RefObjectVersion names one version of a scientific object.
	RefObjectVersion LedgerRefKind = "object_version"
	// RefState names a project state (the accepted snapshot a transition
	// produced).
	RefState LedgerRefKind = "state"
	// RefPullRequest names a research pull request.
	RefPullRequest LedgerRefKind = "pull_request"
	// RefMerge names a semantic merge record.
	RefMerge LedgerRefKind = "merge"
	// RefRelease names a release.
	RefRelease LedgerRefKind = "release"
	// RefAsset names a research asset (by its pid — the identity a citation
	// resolves).
	RefAsset LedgerRefKind = "asset"
	// RefAssetVersion names one published research asset version.
	RefAssetVersion LedgerRefKind = "asset_version"
	// RefKnowledgePublication names a published knowledge object version.
	RefKnowledgePublication LedgerRefKind = "knowledge_publication"
	// RefCreditDispute names a credit dispute (credit_disputes.id), the
	// row docs/13 §3's open/resolve pair is about. It is a ref kind of its
	// own rather than a reuse of the "asset"/"release"/"finding" kinds a
	// credit declaration's target_ref carries: a dispute is an entity with
	// an identity, and a ledger row that pointed at the target only would
	// say a credit was disputed without saying by which dispute.
	RefCreditDispute LedgerRefKind = "credit_dispute"
)

// ledgerRefKinds is every kind above, in declaration order. It exists so a
// reader can ask whether a ref kind is one this projection produces
// (ValidLedgerRefKind) without restating the list — the same shape
// creditRoles has for the credit vocabulary, and for the same reason: one
// definition, checkable in one place.
var ledgerRefKinds = []LedgerRefKind{
	RefObject, RefObjectVersion, RefState, RefPullRequest, RefMerge, RefRelease,
	RefAsset, RefAssetVersion, RefKnowledgePublication, RefCreditDispute,
}

// ValidLedgerRefKind reports whether k is a ref kind this projection
// produces. A "kind:value" text whose kind is not one of these is not a
// ledger ref, whatever it looks like — which is what
// internal/application/credit checks a dispute's evidence refs against
// before it records them.
func ValidLedgerRefKind(k LedgerRefKind) bool {
	for _, v := range ledgerRefKinds {
		if v == k {
			return true
		}
	}
	return false
}

// LedgerRef is one canonical "kind:value" reference in a ledger row's
// object_refs.
type LedgerRef string

// NewLedgerRef builds a canonical ref, reporting false for an empty value —
// a ref that points at nothing is not a reference, and writing one would
// make the column look populated while saying nothing.
func NewLedgerRef(kind LedgerRefKind, value string) (LedgerRef, bool) {
	if kind == "" || value == "" {
		return "", false
	}
	return LedgerRef(string(kind) + ":" + value), true
}

// Kind returns the ref's kind ("" for a malformed ref).
func (r LedgerRef) Kind() LedgerRefKind {
	kind, _, ok := strings.Cut(string(r), ":")
	if !ok {
		return ""
	}
	return LedgerRefKind(kind)
}

// Value returns the ref's value ("" for a malformed ref).
func (r LedgerRef) Value() string {
	_, value, ok := strings.Cut(string(r), ":")
	if !ok {
		return ""
	}
	return value
}

// LedgerRefSpec declares one reference a mapping reads out of the event
// payload.
type LedgerRefSpec struct {
	// Kind is the entity class the ref names.
	Kind LedgerRefKind
	// PayloadKey is the payload member the identity travels in. It is the
	// key the event's own producer writes (the producers are named on each
	// mapping below): the projection reads what the event carries and never
	// resolves an entity from current state, because the ledger records the
	// event, not today's world.
	PayloadKey string
}

// LedgerMapping is one row of the projection's table: the ledger row a
// domain event produces.
//
// One event produces ONE ledger row (the dedupe key is the source event
// id, so an event cannot fan out) — a projection the acceptance pins.
type LedgerMapping struct {
	// EventType is the event name from specs/events/event-types.yaml. The
	// ledger row stores it verbatim, so a Profile query filters on the same
	// spelling every producer and subscriber uses.
	EventType string
	// Roles are the docs/04 §4 contribution roles every event of this type
	// records.
	Roles []ContributionRole
	// ObjectTypeRoles, when set, replaces Roles with the entry matching the
	// payload's object_type (a scientific_object.version_created event is
	// "created object" — what KIND of object decides whether it is a
	// hypothesis proposal, a dataset curation or a calculation). A payload
	// whose object_type is absent or unknown records no role; the row is
	// still written (the act happened, the tag is what is missing).
	ObjectTypeRoles map[string][]ContributionRole
	// AcceptedContext marks the row as an accepted-contribution fact
	// (docs/13 §4 "accepted contributions"): true only for
	// contribution.accepted, decided from the event itself and never
	// back-filled onto an earlier row.
	AcceptedContext bool
	// ReleasedContext marks the row as a release-inclusion fact (docs/13 §4
	// "Release inclusion"; docs/04 §5 lists it beside Accepted Contribution
	// as its own dimension): true only for release.published, decided from
	// the event itself.
	ReleasedContext bool
	// Refs are the references read out of the payload into object_refs.
	Refs []LedgerRefSpec
	// Why is the citation for this mapping — the document line that makes
	// this event a contribution act. It is required on every row: the table
	// is the place where "this is a contribution" is argued, and a row
	// without an argument is a guess.
	Why string
}

// RolesFor returns the mapping's roles for one payload: the object-type
// refinement when the mapping has one, the static list otherwise.
func (m LedgerMapping) RolesFor(payload map[string]any) []ContributionRole {
	if len(m.ObjectTypeRoles) == 0 {
		return m.Roles
	}
	return m.ObjectTypeRoles[payloadString(payload, "object_type")]
}

// RefsFor returns the mapping's refs for one payload: one ref per declared
// spec whose key the payload carries as a non-empty string. A key the event
// does not carry contributes no ref — the ledger records what the event
// said, and inventing a reference is how a ledger starts lying.
func (m LedgerMapping) RefsFor(payload map[string]any) []LedgerRef {
	refs := make([]LedgerRef, 0, len(m.Refs))
	for _, spec := range m.Refs {
		if ref, ok := NewLedgerRef(spec.Kind, payloadString(payload, spec.PayloadKey)); ok {
			refs = append(refs, ref)
		}
	}
	return refs
}

// scientificObjectTypeRoles maps a scientific object type (the object_type
// member of scientific_object.version_created, docs/08 / specs/schemas) to
// the docs/04 §4 roles that creating one records.
//
// The five entries docs/13 §1 names explicitly are here verbatim: created
// object, proposed hypothesis, performed experiment, ran/recorded
// calculation, curated dataset. The rest are the same judgement applied to
// the object types the schema registry defines (internal/rsg/schemareg):
//
//   - protocol is a method → Method Development;
//   - material and sample are the physical objects an experimental
//     investigation works on → Experimental Investigation;
//   - claim and finding are what Analysis produces → Analysis.
//
// external_reference and any future object type record no role: docs/04 §4
// has no value for registering a citation, and a role tag that was invented
// to fill a gap would be indistinguishable from a recorded one.
//
// Every key here must be a type a version_created event can actually carry,
// i.e. an object type the schema registry resolves (internal/rsg/schemareg):
// the RSG write path refuses any other type before a version row — and so a
// version_created event — can exist (internal/application/rsg schemaFor).
// The table carried "evidence_assertion" → Validation once, and it was a
// DEAD ROW: the registry's schema for that type is named
// evidence-assertion.schema.json, so `evidence_assertion` resolves to
// nothing and no event can ever spell it (the T0807 review caught it; the
// drift test now resolves every key so the next one cannot hide). A dead row
// reads as coverage that does not exist. Validation contributions are not
// unrecorded by its removal — the `evidence_assertion.created` mapping below
// carries RoleValidation.
var scientificObjectTypeRoles = map[string][]ContributionRole{
	"research_question": {RoleResearchQuestionProposal},
	"hypothesis":        {RoleHypothesisProposal},
	"protocol":          {RoleMethodDevelopment},
	"experiment":        {RoleExperimentalInvestigation},
	"calculation":       {RoleComputationalInvestigation},
	"dataset":           {RoleDataCuration},
	"material":          {RoleExperimentalInvestigation},
	"sample":            {RoleExperimentalInvestigation},
	"claim":             {RoleAnalysis},
	"finding":           {RoleAnalysis},
}

// ledgerMappings is the projection's table — the complete answer to "which
// domain events become ledger rows, and as what". Everything else in
// specs/events/event-types.yaml has no row here, which the projection
// reports rather than hides.
//
// The producers named in the Why lines are the emitters that exist today
// (internal/application/rsg, internal/application/merge and the persistence
// stores' RecordResearchEvent); entries whose payload is not yet pinned by
// a producer say so, and declare no refs, because a payload key nobody
// writes is a guess dressed as a fact.
var ledgerMappings = []LedgerMapping{
	{
		EventType:       "scientific_object.version_created",
		ObjectTypeRoles: scientificObjectTypeRoles,
		Refs:            []LedgerRefSpec{{Kind: RefObject, PayloadKey: "object_id"}},
		Why: "docs/13 §1 'created object' — and its named refinements: proposed hypothesis, " +
			"performed experiment, ran/recorded calculation, curated dataset. The event pins " +
			"both halves of the act (object_id and version_no; the version itself is named by " +
			"the source event this row points at). Producer: internal/application/rsg/events.go.",
	},
	{
		EventType: "evidence_assertion.created",
		Roles:     []ContributionRole{RoleValidation},
		Why: "docs/13 §1's list is open (等), and asserting evidence for or against a claim is a " +
			"contribution act docs/04 §4 names — Validation ('Validation: testing a claim against " +
			"evidence'). No producer exists yet (nothing in the repository emits this event), so " +
			"the payload's shape is not pinned and no refs are declared: the row records the act, " +
			"its actor and its project, and the refs land with the event.",
	},
	{
		EventType: "pull_request.reviewed",
		Roles:     []ContributionRole{RoleScientificReview},
		Refs:      []LedgerRefSpec{{Kind: RefPullRequest, PayloadKey: "pull_request_id"}},
		Why: "docs/13 §1 'reviewed PR' → docs/04 §4 Scientific Review. No producer exists yet, so " +
			"the ref key is the one this repository's other pull_request events use " +
			"(pull_request.merged, internal/application/merge/events.go) rather than an invented one.",
	},
	{
		EventType: "pull_request.merged",
		Refs: []LedgerRefSpec{
			{Kind: RefPullRequest, PayloadKey: "pull_request_id"},
			{Kind: RefMerge, PayloadKey: "merge_id"},
			{Kind: RefState, PayloadKey: "state_id"},
		},
		Why: "docs/13 §1 'approved merge'. The event records the merge, its actor, and by payload " +
			"the PR, the merge record and the accepted state (internal/application/merge/events.go). " +
			"No role is claimed: docs/04 §4 has no value for approving a merge, and tagging it " +
			"Scientific Review would attribute the review to the person who merged.",
	},
	{
		EventType:       "release.published",
		ReleasedContext: true,
		Refs: []LedgerRefSpec{
			{Kind: RefRelease, PayloadKey: "release_id"},
			{Kind: RefState, PayloadKey: "state_id"},
		},
		Why: "docs/13 §1 'published asset' in its strongest form — a release fixes an accepted " +
			"state's snapshot (docs/11 §1) — and the release-inclusion fact docs/13 §4 makes a " +
			"reputation dimension of its own, so released_context is set from the event itself " +
			"(internal/persistence/release_store.go) and never recomputed later.",
	},
	{
		EventType: "research_asset.version_published",
		Roles:     []ContributionRole{RoleAssetStewardship},
		Refs: []LedgerRefSpec{
			{Kind: RefAsset, PayloadKey: "asset_id"},
			{Kind: RefAssetVersion, PayloadKey: "asset_version_id"},
		},
		Why: "docs/13 §1 'published asset' → docs/04 §4 Asset Stewardship. The payload's asset_id " +
			"carries the asset's pid (the identity a citation resolves) and asset_version_id the " +
			"published version (internal/persistence/asset_publish_store.go).",
	},
	{
		EventType: "knowledge.version_published",
		Refs: []LedgerRefSpec{
			{Kind: RefKnowledgePublication, PayloadKey: "publication_id"},
			{Kind: RefObjectVersion, PayloadKey: "object_version_id"},
		},
		Why: "the same publication act on the knowledge surface: docs/13 §1's list is open (等), a " +
			"published knowledge object version is how a project's scientific work enters the " +
			"network (docs/03, T0805), and the event carries the publication and the version " +
			"(internal/persistence/knowledge_publish_store.go). No role is claimed — docs/04 §4's " +
			"Asset Stewardship is about released ASSETS, and a knowledge object is not an asset.",
	},
	{
		EventType:       "contribution.accepted",
		AcceptedContext: true,
		Why: "docs/13 §4's accepted-contribution dimension, and the one event in the vocabulary that " +
			"records it. No producer exists yet, so no refs are declared. The row's actor is the " +
			"event's actor: whether that is the contributor whose work was accepted or the " +
			"maintainer who accepted it is a property of the event as produced, and the ledger " +
			"does not re-attribute it — re-attribution would need a field the event does not define.",
	},
	{
		EventType: "credit.dispute_opened",
		Refs:      []LedgerRefSpec{{Kind: RefCreditDispute, PayloadKey: "dispute_id"}},
		Why: "docs/13 §3: the dispute is part of the record ('旧 attribution 和 dispute history 保留') " +
			"and docs/13 §2 makes corrections new events rather than edits. Recording the dispute " +
			"as an append-only fact is what keeps that history readable; the ledger makes no " +
			"reputation claim about it (docs/13 §3: a dispute does not enter the public reputation " +
			"until resolution), and no role or context flag is set. The producer is the credit " +
			"dispute command (internal/application/credit; recorded by " +
			"internal/persistence/credit_store.go when a dispute row is written), whose payload " +
			"carries dispute_id, the target ref and the ledger evidence the claim points at.",
	},
	{
		EventType: "credit.dispute_resolved",
		Refs:      []LedgerRefSpec{{Kind: RefCreditDispute, PayloadKey: "dispute_id"}},
		Why: "the resolution half of the same record (docs/13 §3): one row per dispute closed, " +
			"produced by the same command as the opening and by the same store, in the " +
			"transaction that closes the credit_disputes row. The row's actor is the maintainer " +
			"who decided, and the outcome (resolved or rejected) travels in the payload beside " +
			"the dispute's id, so the two halves of one dispute join on the same ref.",
	},
}

// LedgerMappings returns the projection's table, in declaration order.
func LedgerMappings() []LedgerMapping {
	out := make([]LedgerMapping, len(ledgerMappings))
	copy(out, ledgerMappings)
	return out
}

// LedgerMappingFor returns the mapping for an event type; ok is false when
// the event has no ledger row (the projection counts those, it does not
// drop them).
func LedgerMappingFor(eventType string) (LedgerMapping, bool) {
	for _, m := range ledgerMappings {
		if m.EventType == eventType {
			return m, true
		}
	}
	return LedgerMapping{}, false
}

// MappedEventTypes returns every event type the table maps, sorted. It is
// the parameter of the store's candidate scan, so the persisted and the
// declared vocabularies cannot drift into each other.
func MappedEventTypes() []string {
	out := make([]string, 0, len(ledgerMappings))
	for _, m := range ledgerMappings {
		out = append(out, m.EventType)
	}
	sort.Strings(out)
	return out
}

// LedgerSource is one domain event as the projection reads it: the
// research_events row, envelope columns included.
type LedgerSource struct {
	// EventID is research_events.id — the row's source_event_id.
	EventID string
	// EventType is the event name (specs/events/event-types.yaml).
	EventType string
	// ActorID is the person the event credits. A ledger row requires one
	// (contribution_events.actor_id is NOT NULL), so an event without an
	// actor cannot be projected; the projection reports those instead.
	ActorID string
	// ProjectID is the research boundary; empty means the event has no
	// project scope (the ledger column is nullable).
	ProjectID string
	// Via is the channel the event was carried by, from the event's own
	// envelope column. Empty means the source event carries none, and the
	// ledger row records none — the projection never reads a channel out of
	// a payload and never substitutes a default (00046: envelope columns are
	// copied, never re-derived).
	Via string
	// OccurredAt is the instant the act happened. It is the ledger row's
	// occurred_at, NOT the time of projection, and it is the instant the
	// affiliation is resolved at.
	OccurredAt time.Time
	// Payload is the event's raw payload object.
	Payload []byte
}

// LedgerRow is one row the projection writes.
type LedgerRow struct {
	// SourceEventID pins the event this row projects.
	SourceEventID string
	// ActorID is the credited person (the event's actor).
	ActorID string
	// ProjectID is the research boundary ("" = no project scope).
	ProjectID string
	// EventType is the event name, stored verbatim.
	EventType string
	// RoleCodes are the docs/04 §4 roles the event records.
	RoleCodes []ContributionRole
	// Refs are the entities the event names.
	Refs []LedgerRef
	// AcceptedContext / ReleasedContext are the docs/13 §4 facts, decided
	// from the event itself.
	AcceptedContext bool
	ReleasedContext bool
	// OccurredAt is the event's instant, not the projection's.
	OccurredAt time.Time
	// Via is the channel, copied from the source event's envelope column
	// ("" = the source carries none).
	Via string
}

// ErrNoLedgerMapping reports that an event's type has no row in the
// mapping table. It is a fact about the vocabulary, not a failure: the
// projection counts these events and reports them (LedgerBatch.Unmapped),
// which is why the exported ProjectEvent answers it with ok=false rather
// than with an error.
var ErrNoLedgerMapping = errors.New("contribution: event type has no ledger mapping")

// ProjectEvent renders one source event into the ledger row it produces.
// ok is false when the event's type has no mapping — the caller counts that
// and moves on; it is a fact about the vocabulary, not an error.
//
// A payload that is not a JSON object yields a row with no roles and no
// refs rather than an error: the act, its actor and its project are facts
// of the event's envelope, and a projection that refused the row would
// leave the event re-appearing as a candidate forever. What is lost is the
// tags, and the caller logs it.
func ProjectEvent(src LedgerSource) (LedgerRow, bool) {
	row, err := projectEvent(src)
	return row, err == nil
}

// projectEvent is ProjectEvent with the REASON it refused, which is what a
// caller inside this package reports: ErrNoLedgerMapping for a type the
// table does not cover, and the role vocabulary's own error (which names
// the offending code and the canonical set) for a table entry that spells
// a role docs/04 §4 does not define. The second one is a programming error
// in the table, and saying "no ledger mapping" about it would be false —
// the mapping is right there.
func projectEvent(src LedgerSource) (LedgerRow, error) {
	m, ok := LedgerMappingFor(src.EventType)
	if !ok {
		return LedgerRow{}, fmt.Errorf("%w: %s", ErrNoLedgerMapping, src.EventType)
	}
	payload := decodeLedgerPayload(src.Payload)
	row := LedgerRow{
		SourceEventID:   src.EventID,
		ActorID:         src.ActorID,
		ProjectID:       src.ProjectID,
		EventType:       src.EventType,
		RoleCodes:       m.RolesFor(payload),
		Refs:            m.RefsFor(payload),
		AcceptedContext: m.AcceptedContext,
		ReleasedContext: m.ReleasedContext,
		OccurredAt:      src.OccurredAt,
		Via:             src.Via,
	}
	// A mapping's own vocabulary is checked here, at the boundary between
	// the table and the row: a typo in an ObjectTypeRoles entry would
	// otherwise be written to the ledger as a role docs/04 never defined.
	if err := RoleVocabularyError(row.RoleCodes); err != nil {
		return LedgerRow{}, err
	}
	return row, nil
}

// decodeLedgerPayload reads the event payload as a JSON object; anything
// else (empty, null, malformed, a non-object) reads as an empty object.
func decodeLedgerPayload(raw []byte) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	return obj
}

// payloadString reads one payload member as a string ("" for absent, null
// or a non-string value — a number or object at that key is not an identity
// this projection will guess at).
func payloadString(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	s, _ := payload[key].(string)
	return s
}

// RefTexts renders refs as the text slice the jsonb column stores.
func RefTexts(refs []LedgerRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, string(r))
	}
	return out
}

// RoleCodes renders roles as the text slice the text[] column stores.
func RoleCodes(roles []ContributionRole) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, string(r))
	}
	return out
}

// describeSource renders an event identity for a log line: the event type
// and its row id, never the payload (an event payload is not an error
// message).
func describeSource(src LedgerSource) string {
	return fmt.Sprintf("%s %s", src.EventType, src.EventID)
}
