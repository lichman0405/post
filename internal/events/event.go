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
		e.Visibility, payload, correlationID); err != nil {
		return fmt.Errorf("events: record %s: %w", e.EventType, err)
	}
	return nil
}

const insertOutboxEvent = `
INSERT INTO outbox_events (event_type, actor_id, project_id, visibility, payload, correlation_id)
VALUES ($1, $2, $3, $4, $5, $6)`

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
