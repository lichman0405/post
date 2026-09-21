package dependencyimpact

import (
	"sort"
	"time"
)

// MaxHops bounds the dependency walk. It is a cycle guard as much as a
// depth budget: the walk is a recursive CTE over an append-only edge set,
// and a cycle (A depends on B, B on A — legal, and not even unusual
// between two protocols that cite each other) would make an unbounded
// UNION ALL recurse until the connection died. A bound makes the walk
// terminate, and the result SAYS when the bound was reached
// (Analysis.HitDepthCap) instead of quietly truncating: a truncated impact
// list that looked complete would be the silent hole this repository
// refuses everywhere else (the ledger's unmapped-event counts are the same
// rule).
//
// 16 is a depth budget, not a scientific constant: a dependency chain
// deeper than sixteen objects is not a research dependency, it is a data
// model that has gone wrong, and the flag is how an operator finds out.
const MaxHops = 16

// SubjectKind names what kind of thing an upstream change happened to —
// the two landing points of a dependency in this repository
// (internal/rsg/relationcatalog for object dependencies,
// asset_dependencies for the project-side ones).
type SubjectKind string

const (
	// SubjectObject is a scientific object (scientific_objects.id). The
	// trigger names a version; the walk is over the object (see the
	// package comment).
	SubjectObject SubjectKind = "object"
	// SubjectAssetVersion is a published research asset version
	// (research_asset_versions.id): the fixed version a project pins in
	// asset_dependencies.
	SubjectAssetVersion SubjectKind = "asset_version"
)

// Subject identifies the upstream thing that changed.
type Subject struct {
	Kind SubjectKind
	// ID is scientific_objects.id for SubjectObject and
	// research_asset_versions.id for SubjectAssetVersion. It is the raw
	// row id, never a display name or a pid: the walk is by identity, and
	// a name is not one.
	ID string
	// VersionNo is the object version the trigger named (0 when the
	// trigger named none, or for an asset subject). It is not part of the
	// walk — the walk is over the object — but it is part of what the
	// alert SAYS: "version 3 of this was aborted" is a different message
	// from "this changed".
	VersionNo int
}

// String renders the subject for logs and error messages.
func (s Subject) String() string {
	return string(s.Kind) + ":" + s.ID
}

// AffectedKind names what a downstream entity is.
type AffectedKind string

const (
	// AffectedObject is a scientific object that depends, directly or
	// transitively, on the changed upstream object.
	AffectedObject AffectedKind = "object"
	// AffectedProject is a project whose asset_dependencies row pins the
	// changed asset version. A project is the dependent on this landing
	// point because the table says so: asset_dependencies' key is
	// (project_id, asset_version_id, dependency_type) — the project is
	// the party that declared the dependency.
	AffectedProject AffectedKind = "project"
)

// Directness separates a one-hop impact from a transitive one. The two are
// different strengths of prompt and the page that renders them has to be
// able to tell them apart, so the distinction is a value in the data
// rather than something a renderer derives from a number it may not have.
type Directness string

const (
	// Direct is an entity that depends on the changed upstream directly:
	// one dependency hop away.
	Direct Directness = "direct"
	// Indirect is an entity reached through one or more further
	// dependents: two or more hops away.
	Indirect Directness = "indirect"
)

// DirectnessOf classifies a hop count. Hop counts start at 1 (the
// dependent of the changed thing); anything above is transitive. A hop
// count below 1 names no entity at all and classifies as Indirect rather
// than Direct, because the fail-safe answer to "how far away is this" is
// the weaker claim.
func DirectnessOf(hops int) Directness {
	if hops == 1 {
		return Direct
	}
	return Indirect
}

// Impact is one downstream entity the upstream change reaches — the
// answer's unit, and the thing one alert event is about.
//
// It carries the affected entity's identity in full (kind, id, the project
// that owns it, and for an object its object id and type), because the
// alert travels as an event and an event whose reader has to run a query to
// find out what it is about is a pointer, not a message (the payload rule
// events.Event documents: identity and reference, no bulk data).
type Impact struct {
	// Kind is what the affected entity is.
	Kind AffectedKind
	// ID is the affected entity's own id: scientific_objects.id for
	// AffectedObject, projects.id for AffectedProject.
	ID string
	// ProjectID is the project the affected entity belongs to. It is the
	// gate the read surface authorizes on, and the routing key the alert
	// is addressed to (the event envelope's project_id): the alert
	// belongs to the people who own the downstream work.
	ProjectID string
	// ObjectID and ObjectType are the affected object's identity. They are
	// empty for AffectedProject.
	ObjectID   string
	ObjectType string
	// Hops is the dependency distance: 1 for a direct dependent.
	Hops int
	// Directness is Hops classified. It is stored beside Hops rather than
	// derived at the render site so that a consumer cannot lose the
	// distinction (the acceptance requires direct and indirect to be
	// separable IN THE DATA).
	Directness Directness
}

// Analysis is the whole downstream set of one upstream subject, as the
// analysis sees it with no reader in the way.
//
// Both landing points arrive in the same list: object dependents are
// Impacts of Kind AffectedObject, asset dependents (which are projects) of
// Kind AffectedProject. There is deliberately no second list — a subject
// is one kind of thing, so one walk produces one answer, and two
// collections would be two things to keep in step for a caller that has to
// render them together anyway.
type Analysis struct {
	Subject Subject
	// Impacts are the affected entities, sorted by (kind, project, id) so
	// two analyses of the same log produce byte-identical results.
	Impacts []Impact
	// HitDepthCap reports whether the walk reached MaxHops, i.e. whether
	// the result may be truncated. It is a standing fact about the graph
	// as well as about this walk.
	HitDepthCap bool
}

// Empty reports whether the analysis found nothing downstream.
func (a Analysis) Empty() bool { return len(a.Impacts) == 0 }

// Subjects is the list of subjects a read was asked about. It is a type
// rather than a bare slice so that "the caller asked about nothing" is
// distinguishable at the call site from "the answer is empty".
type Subjects []Subject

// SubjectImpacts groups one subject's visible impacts. A group is present
// for every subject the caller asked about and may see, with an empty
// Impacts slice when nothing downstream is visible: a client renders a
// stated absence rather than guessing at a missing field (the same rule
// assets.ProjectDependencies states).
type SubjectImpacts struct {
	Subject Subject
	Impacts []Impact
}

// Report is the authorized answer to "what does this change reach" — the
// shape the PR first screen and the asset-side read both render.
//
// It carries NO count of whatever was withheld. That is the whole point of
// the type: docs/23 §5 forbids a reader inferring the existence of a
// private dependency, and "3 impacts (1 hidden)" leaks exactly that. The
// only numbers here are properties of rows the reader may read.
type Report struct {
	Groups []SubjectImpacts
}

// AssetDependent is one project that pins an asset version, with the
// declaration's own visibility. Both fields belong to the read's
// authorization: the project decides whether the row may be seen at all,
// and visibility_of_usage (public/private) decides whether a reader who
// may read a PUBLIC project is entitled to the declaration inside it
// (assets.ProjectDependencyViewer's second rule, docs/23 §5 — a private
// usage is the project's own business).
type AssetDependent struct {
	ProjectID string
	// VisibilityOfUsage is the stored asset_dependencies.visibility_of_usage
	// verbatim.
	VisibilityOfUsage string
}

// Trigger is one recorded upstream change the analysis has to work
// through: a research_events row whose type this package's trigger table
// covers, with its subject already resolved.
type Trigger struct {
	// EventID is research_events.id — the analysis's unit of work and the
	// thing its alert is keyed to (the idempotency index of migration
	// 00111 is keyed on it).
	EventID   string
	EventType string
	// OccurredAt orders the backlog: the analysis works through the log in
	// the order the changes happened.
	OccurredAt time.Time
	// ActorID and ProjectID are the trigger event's own envelope values,
	// carried so the alert can be correlated with its cause (ProjectID is
	// the project the change was made IN — the upstream side, never the
	// alert's own address).
	ActorID   string
	ProjectID string
	// Visibility is the trigger event's visibility, and the alert is never
	// more visible than it: the alert names the trigger, so it cannot be
	// published to readers who may not read the trigger (docs/12 §3).
	Visibility    string
	CorrelationID string
	// Subject is what changed.
	Subject Subject
}

// Batch is one analysis pass's outcome. The counts are the projection's
// own coverage statement, in the shape the Contribution Ledger projection
// established (internal/contribution.LedgerBatch): Candidates is what this
// pass picked up, Emitted how many alerts landed, Duplicates how many were
// already there (a replay — not an error), and TriggerCoverage counts the
// log's trigger-type events that name no subject this package can resolve,
// so a trigger the analysis can never act on is a number someone can read
// rather than a hole.
type Batch struct {
	Candidates  int
	Emitted     int
	Duplicates  int
	Pending     int64
	HitDepthCap int
	// Unresolvable counts, per event type, trigger-type events whose
	// payload names no subject (a version id that no longer resolves, a
	// payload that is not the shape its producer writes). They are counted
	// rather than skipped for the reason the ledger counts its unmapped
	// types: a type the analysis claims to cover and silently drops is
	// worse than one it does not claim.
	Unresolvable []EventCount
	// NoDependents counts, per event type, trigger-type events that DO name
	// a resolvable subject and have nothing downstream of it. They produce
	// no alert because there is no one to alert, which is why the scan
	// leaves them out of the batch window — and this count is where that
	// shows: without it, "the analysis never looked at this" and "the
	// analysis looked and found nobody" would be the same silence, and the
	// one number an operator needs in order to trust the backlog (how much
	// of the log the analysis acted on) would be missing.
	NoDependents []EventCount
}

// EventCount is one event type's count in a report.
type EventCount struct {
	EventType string
	Count     int64
}

// sortImpacts orders impacts deterministically: kind, then project, then
// id, then hops. Two walks over the same log therefore produce the same
// slice, which is what lets a test compare sets by value and a replay
// compare byte for byte.
func sortImpacts(impacts []Impact) {
	sort.Slice(impacts, func(i, j int) bool {
		a, b := impacts[i], impacts[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.ProjectID != b.ProjectID {
			return a.ProjectID < b.ProjectID
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Hops < b.Hops
	})
}
