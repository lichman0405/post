package events

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/domain"
)

// Visibility is the event visibility vocabulary (docs/12 §3: an event is
// never more visible than its subject; the canonical values are the ones
// the projects/branches surfaces already use).
const (
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"
)

// DefaultPayloadVersion is the payload envelope version injected when the
// producer's payload does not carry one (specs/events/event-types.yaml:
// payload_version is a required envelope field).
const DefaultPayloadVersion = "1"

// DBTX is the database surface Record writes through: the caller's
// in-flight transaction (a states.Transaction callback, a pgx.Tx, or a
// pool — sqlc's generated queries accept the same shape). The outbox row
// commits or rolls back with whatever else the caller writes, which is the
// whole point of the transactional outbox.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Event is one domain event heading for the transactional outbox. The
// worker publishes it into research_events later; the envelope columns
// (actor, project, visibility) travel as outbox columns so the publisher
// copies them verbatim instead of re-deriving them.
type Event struct {
	// EventType is the canonical event name
	// (specs/events/event-types.yaml), e.g. "state.committed".
	EventType string
	// ActorID / ProjectID identify who acted and where; "" means "no
	// actor" / "no project" (the columns are nullable).
	ActorID   string
	ProjectID string
	// Visibility is required and explicit: public or private. An event
	// without one is refused, never guessed — a silent default could
	// publish a private event publicly.
	Visibility string
	// Via is the channel the write that produced this event arrived
	// through (docs/13 §1's seventh field, "via agent/client"): the
	// state_commits.via vocabulary of docs/15 §5 — web, api, mcp,
	// claude_code, git_compat, system (domain.StateVia). It is an envelope
	// column, not payload data: it travels outbox → research event →
	// Contribution Ledger by being copied verbatim at each hop, and it is
	// never re-derived from the payload (00046). Empty means the write
	// path did not record a channel and the column stays NULL — never a
	// guessed default, since a fabricated 'api' would read exactly like a
	// recorded fact (00087). A value OUTSIDE the vocabulary is refused:
	// fail closed, like the state commit's own via check.
	Via domain.StateVia
	// CorrelationID traces the event across API → outbox → published
	// event (docs/26 §2). Empty means "fill from the request context, and
	// generate one when there is none".
	CorrelationID string
	// Payload is the event-specific JSON object (identity/reference only —
	// docs/52「Workers」关于 job payload 的口径，事件沿用同一原则: no bulk
	// sensitive data in events). Empty or null is stored as {}.
	Payload json.RawMessage
}

// Recorder is the outbox write surface application services compose (the
// rsg service takes the same shape as a port). The zero value is ready to
// use.
type Recorder struct{}

// Record implements the outbox write surface.
func (Recorder) Record(ctx context.Context, db DBTX, e Event) error {
	return Record(ctx, db, e)
}

// Record validates e and inserts it into outbox_events inside db's
// transaction. Fail-closed, like the audit write: an invalid event or a
// failed insert fails the caller's whole transaction, because an action
// whose event was silently dropped would have lost the event — the one
// thing the outbox exists to prevent.
func Record(ctx context.Context, db DBTX, e Event) error {
	if err := e.validate(); err != nil {
		return err
	}
	payload, err := withPayloadVersion(e.Payload)
	if err != nil {
		return err
	}
	correlationID := e.CorrelationID
	if correlationID == "" {
		if info, ok := domain.RequestInfoFrom(ctx); ok && info.CorrelationID != "" {
			correlationID = info.CorrelationID
		}
	}
	if correlationID == "" {
		correlationID, err = newUUID()
		if err != nil {
			return fmt.Errorf("events: correlation id: %w", err)
		}
	}
	if _, err := db.Exec(ctx, insertOutboxEvent,
		e.EventType, nullableText(e.ActorID), nullableText(e.ProjectID),
		e.Visibility, payload, correlationID, nullableVia(e.Via)); err != nil {
		return fmt.Errorf("events: record %s: %w", e.EventType, err)
	}
	return nil
}

// The via column is appended LAST so the argument positions of the
// columns that were here before it do not move (the unit tests pin them by
// index, and so does anyone reading a pgx trace).
const insertOutboxEvent = `
INSERT INTO outbox_events (event_type, actor_id, project_id, visibility, payload, correlation_id, via)
VALUES ($1, $2, $3, $4, $5, $6, $7)`

// validate checks the event's shape before it reaches the database: the
// producer-facing contract, kept strict so a programming error fails the
// action loudly at the source instead of surfacing as a stuck outbox row.
func (e Event) validate() error {
	if e.EventType == "" {
		return fmt.Errorf("events: event_type is required")
	}
	switch e.Visibility {
	case VisibilityPublic, VisibilityPrivate:
	default:
		return fmt.Errorf("events: visibility must be %q or %q, got %q",
			VisibilityPublic, VisibilityPrivate, e.Visibility)
	}
	// The channel is optional (a path that does not know one records
	// none), but a value outside the vocabulary is refused before any
	// storage — fail closed, exactly as the state commit's own via check
	// does (internal/application/states validateCommitParams). The
	// vocabulary is domain.StateVia's, never re-spelled here.
	if e.Via != "" && !domain.ValidStateVia(e.Via) {
		return fmt.Errorf("events: via %q is not a canonical channel (web, api, mcp, claude_code, git_compat, system)",
			e.Via)
	}
	if len(e.Payload) > 0 && string(e.Payload) != "null" {
		var v any
		if err := json.Unmarshal(e.Payload, &v); err != nil {
			return fmt.Errorf("events: payload is not valid JSON: %w", err)
		}
		if _, ok := v.(map[string]any); !ok {
			return fmt.Errorf("events: payload must be a JSON object")
		}
	}
	return nil
}

// withPayloadVersion guarantees the canonical envelope's payload_version
// field (specs/events/event-types.yaml) inside the payload object. The
// producer's own value wins; only an absent one is injected — one place
// owns the envelope invariant, producers never hand-roll it. Numbers are
// decoded as json.Number (UseNumber) and re-encoded from their literal
// text: a map[string]any round-trip would decode integers through float64
// and silently round anything above 2^53 (T1001 review).
func withPayloadVersion(payload json.RawMessage) ([]byte, error) {
	obj := map[string]any{}
	if len(payload) > 0 && string(payload) != "null" {
		dec := json.NewDecoder(bytes.NewReader(payload))
		dec.UseNumber()
		if err := dec.Decode(&obj); err != nil {
			return nil, fmt.Errorf("events: payload is not valid JSON: %w", err)
		}
		// The decoder reads one value; a second decode must hit EOF or the
		// input carries trailing data (the same strictness json.Unmarshal
		// applies).
		var trailing any
		if err := dec.Decode(&trailing); err != io.EOF {
			return nil, fmt.Errorf("events: payload is not valid JSON: trailing data")
		}
	}
	if _, ok := obj["payload_version"]; !ok {
		obj["payload_version"] = DefaultPayloadVersion
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("events: re-encoding payload: %w", err)
	}
	return out, nil
}

// nullableText renders "" as NULL (actor/project may be absent).
func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullableVia renders an unset channel as NULL. The empty StateVia is not
// a member of the vocabulary and is not a default: it is the absence of a
// declaration, and NULL is what says so in the column (00087).
func nullableVia(v domain.StateVia) any {
	if v == "" {
		return nil
	}
	return string(v)
}

// newUUID generates a UUID v4 with crypto/rand (the same shape as the rsg
// service's id helper — no external dependency for one random id).
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
