package domain

import (
	"encoding/json"
	"time"
)

// ScientificObject is the container row of a research object: identity,
// project and type. Scientific content lives exclusively in the append-only
// ScientificObjectVersion log — an object row carries no content of its own
// (docs/21 §4, CLAUDE.md §9.8: nothing disappears; state only evolves).
// Canonical columns: infra/migrations/00005_scientific_objects.sql; 00024
// adds the current_version_no head pointer.
type ScientificObject struct {
	// ID is the uuid v4 text form (matches the scientific_objects.id uuid
	// column).
	ID string
	// ProjectID is the research boundary the object belongs to.
	ProjectID string
	// ObjectType names the object's scientific type (e.g. "experiment",
	// "hypothesis"). The schema registry (internal/rsg/schemareg, T0201)
	// validates payloads per type; the repository stores the string as
	// given, so unknown future types never break persistence.
	ObjectType string
	// CurrentVersionNo is the materialized head of the version log
	// (docs/21 §5). It doubles as the compare-and-swap cell for
	// expected_version creation; readers that need the log's own truth
	// read the version rows.
	CurrentVersionNo int
	CreatedBy        string
	CreatedAt        time.Time
}

// ScientificObjectVersion is one immutable row of the append-only version
// log (scientific_object_versions). Rows are never updated or deleted — the
// database itself rejects it (migrations 00014/00015) and the repository
// surface offers no update path; every change is a new version row
// (docs/21 §4, docs/46).
type ScientificObjectVersion struct {
	// ID is the uuid v4 text form of the version row.
	ID string
	// ObjectID names the container object.
	ObjectID string
	// VersionNo is the 1-based position in the object's version log,
	// unique per object.
	VersionNo int
	// StateID is the project state this version was created in (docs/07:
	// every change belongs to a state transition).
	StateID string
	// BranchID is the research branch the version was created on; nil when
	// the version predates branch resolution.
	BranchID *string
	// SchemaID is the $id of the JSON Schema that governs Payload
	// (the schemareg.Ref.ID half).
	SchemaID string
	// SchemaVersion is the schema registry version used for Payload
	// (the schemareg.Ref.Version half). Old schema versions stay valid:
	// schema migration never rewrites history (docs/21 §8).
	SchemaVersion string
	Title         string
	// LifecycleState is the version's lifecycle position.
	LifecycleState LifecycleState
	// Payload is the versioned scientific content, stored as exact bytes
	// (jsonb round-trips through the same textual JSON).
	Payload json.RawMessage
	// VisibilityPolicyID pins the rights policy of this version; nil
	// inherits the project default.
	VisibilityPolicyID *string
	// IntegrityHash is the sha256 hex digest of the stored payload's
	// canonical jsonb text form, computed server-side at insert
	// (docs/23 §3: never caller-supplied). jsonb normalizes key order and
	// whitespace, so the hash pins the bytes the row actually holds: a
	// read payload always re-hashes to its stored hash. The canonical-JSON
	// hash algorithm of docs/21 §10 arrives with a later task and never
	// rewrites old rows.
	IntegrityHash string
	CreatedBy     string
	CreatedAt     time.Time
	// Abort is the abort record this version carries, or nil when it is not
	// an abort (and for every version written before migration 00100). It
	// is governance data about the version, never part of Payload: the
	// payload of an aborted version is the aborted version's content,
	// byte-for-byte, so an abort moves the lifecycle without changing what
	// the version says (docs/46 — a correction appends, it never rewrites).
	Abort *AbortRecord
}

// AbortRecord is the record docs/46:7 requires of every abort, verbatim:
// "Abort 必须记录 actor、time、reason code、human explanation、
// replacement/superseding ref(optional)、review/approval if main object."
// The review/approval half is structural rather than a field — a main-line
// abort reaches the accepted state only through a Research PR merge, whose
// reviews are the PR's own records — so the five fields here are the record
// itself.
//
// It is a domain value, not a wire DTO: ReasonCode and Explanation are
// required, ReplacementRef is optional, and DecidedAt is server-derived
// (docs/23 §3 — timestamps are never accepted from an untrusted caller).
type AbortRecord struct {
	// ReasonCode is the caller-supplied reason token (docs/46:7's "reason
	// code", the `reason_code` argument of specs/mcp/tools.json's
	// object.abort_proposal). It is an OPEN string in V1: no specification
	// enumerates the values, so none is invented here — the code checks the
	// token's SHAPE (lowercase [a-z0-9_], 1..64) and stores the value as
	// given. The cost of the open set is named in the T0602 task result:
	// aborts cannot be broken down by reason until a vocabulary is decided,
	// and narrowing later is a normal, forward-only change.
	ReasonCode string
	// Explanation is the human explanation — the part of an abort no machine
	// can reconstruct, and the reason it is required rather than optional.
	Explanation string
	// ReplacementRef names the object version that supersedes the aborted
	// one (docs/46:7's "replacement/superseding ref(optional)"). Empty
	// means none was given; it is stored as NULL, never as an empty string,
	// so "no replacement" and "a replacement that is nothing" stay distinct.
	ReplacementRef string
	// DecidedBy is the actor who decided the abort (docs/46:7's "actor").
	// It is NOT the version row's CreatedBy: when a Research PR merge
	// materializes the abort onto main, the new row's CreatedBy is the
	// merging actor, while this stays the aborting one.
	DecidedBy string
	// DecidedAt is the server-derived time of the abort decision
	// (docs/46:7's "time"). It travels with the record onto main.
	DecidedAt time.Time
}

// LifecycleState is the scientific_object_versions.lifecycle_state CHECK:
// active, aborted, reopened, superseded. Corrections never mutate a row —
// aborting an object appends a new version in state 'aborted' (docs/46).
type LifecycleState string

const (
	LifecycleActive     LifecycleState = "active"
	LifecycleAborted    LifecycleState = "aborted"
	LifecycleReopened   LifecycleState = "reopened"
	LifecycleSuperseded LifecycleState = "superseded"
)

// ValidLifecycleState reports whether s is one of the four canonical
// lifecycle states.
func ValidLifecycleState(s string) bool {
	switch s {
	case "active", "aborted", "reopened", "superseded":
		return true
	}
	return false
}
