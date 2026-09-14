package events

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/domain"
)

// fakeDB is a DBTX stub that captures one Exec call's SQL and arguments —
// the Record contract (the exact insert shape) is asserted here without a
// database; the real database behaviour is covered by the integration
// suite (tests/integration/outbox_test.go).
type fakeDB struct {
	sql  string
	args []any
}

func (f *fakeDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.sql = sql
	f.args = args
	return pgconn.CommandTag{}, nil
}

func (f *fakeDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, nil
}

func (f *fakeDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return nil
}

func TestRecordValidation(t *testing.T) {
	base := func() Event {
		return Event{
			EventType:  "state.committed",
			Visibility: VisibilityPublic,
			Payload:    json.RawMessage(`{"project_id":"p"}`),
		}
	}
	tests := []struct {
		name    string
		mutate  func(*Event)
		wantSub string
	}{
		{"event type required", func(e *Event) { e.EventType = "" }, "event_type is required"},
		{"visibility required", func(e *Event) { e.Visibility = "" }, "visibility must be"},
		{"visibility vocabulary", func(e *Event) { e.Visibility = "internal" }, "visibility must be"},
		{"payload must be JSON", func(e *Event) { e.Payload = json.RawMessage(`{`) }, "not valid JSON"},
		{"payload must be object", func(e *Event) { e.Payload = json.RawMessage(`[1,2]`) }, "JSON object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := base()
			tt.mutate(&e)
			db := &fakeDB{}
			err := Record(context.Background(), db, e)
			if err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("Record() error = %v, want containing %q", err, tt.wantSub)
			}
			if db.sql != "" {
				t.Fatalf("Record() executed SQL on an invalid event: %q", db.sql)
			}
		})
	}
}

func TestRecordPayloadVersionInjected(t *testing.T) {
	db := &fakeDB{}
	err := Record(context.Background(), db, Event{
		EventType:  "state.committed",
		Visibility: VisibilityPrivate,
		Payload:    json.RawMessage(`{"project_id":"p"}`),
	})
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	// The payload arg sits at index 4 (event_type, actor, project,
	// visibility, payload, correlation).
	var payload map[string]any
	if err := json.Unmarshal(db.args[4].([]byte), &payload); err != nil {
		t.Fatalf("payload arg is not JSON: %v", err)
	}
	if payload["payload_version"] != DefaultPayloadVersion {
		t.Errorf("payload_version = %v, want %q", payload["payload_version"], DefaultPayloadVersion)
	}
	if payload["project_id"] != "p" {
		t.Errorf("producer payload fields lost: %v", payload)
	}
	// The producer's own payload_version wins, never overwritten.
	db = &fakeDB{}
	err = Record(context.Background(), db, Event{
		EventType:  "state.committed",
		Visibility: VisibilityPublic,
		Payload:    json.RawMessage(`{"payload_version":"7"}`),
	})
	if err != nil {
		t.Fatalf("Record() with explicit payload_version: %v", err)
	}
	var payload2 map[string]any
	if err := json.Unmarshal(db.args[4].([]byte), &payload2); err != nil {
		t.Fatalf("payload arg is not JSON: %v", err)
	}
	if payload2["payload_version"] != "7" {
		t.Errorf("payload_version = %v, want the producer's 7", payload2["payload_version"])
	}
}

func TestRecordCorrelationID(t *testing.T) {
	// The request correlation id wins over nothing; the event's own value
	// wins over the request's; and with neither present, one is generated
	// (a non-empty uuid-shaped string, never an empty column).
	ctx := domain.WithRequestInfo(context.Background(), domain.RequestInfo{CorrelationID: "req-corr"})
	db := &fakeDB{}
	if err := Record(ctx, db, Event{EventType: "state.committed", Visibility: VisibilityPublic, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if db.args[5] != "req-corr" {
		t.Errorf("correlation id = %v, want the request's %q", db.args[5], "req-corr")
	}

	db = &fakeDB{}
	if err := Record(ctx, db, Event{EventType: "state.committed", Visibility: VisibilityPublic, CorrelationID: "own-corr", Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if db.args[5] != "own-corr" {
		t.Errorf("correlation id = %v, want the event's own %q", db.args[5], "own-corr")
	}

	db = &fakeDB{}
	if err := Record(context.Background(), db, Event{EventType: "state.committed", Visibility: VisibilityPublic, Payload: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("Record() without any correlation source: %v", err)
	}
	corr, ok := db.args[5].(string)
	if !ok || len(corr) != 36 {
		t.Errorf("generated correlation id = %v, want a 36-char uuid", db.args[5])
	}
}

func TestRecordNullsEmptyIdentity(t *testing.T) {
	db := &fakeDB{}
	err := Record(context.Background(), db, Event{
		EventType:  "state.committed",
		Visibility: VisibilityPrivate,
		Payload:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	if db.args[1] != nil || db.args[2] != nil {
		t.Errorf("actor/project args = %v/%v, want nil/NULL for empty ids", db.args[1], db.args[2])
	}
	if !strings.Contains(db.sql, "INSERT INTO outbox_events") {
		t.Errorf("Record() SQL = %q, want the outbox insert", db.sql)
	}
}

func TestRecorderImplementsRecord(t *testing.T) {
	// The zero-value Recorder is the production implementation.
	var r Recorder
	db := &fakeDB{}
	if err := r.Record(context.Background(), db, Event{
		EventType:  "state.committed",
		Visibility: VisibilityPublic,
		Payload:    json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("Recorder.Record() error = %v", err)
	}
	if db.sql == "" {
		t.Fatal("Recorder.Record() executed nothing")
	}
}

func TestWithPayloadVersionPreservesLargeIntegers(t *testing.T) {
	// 2^53+1 does not fit in float64: a map[string]any round-trip silently
	// rounds it (9007199254740993 → 9007199254740992). The decoder must
	// use json.Number so the digits survive bit-exact — an event payload
	// must never be rewritten into different data (T1001 review).
	const big = "9007199254740993"
	out, err := withPayloadVersion(json.RawMessage(`{"n": ` + big + `}`))
	if err != nil {
		t.Fatalf("withPayloadVersion() error = %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	var got map[string]json.Number
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if got["n"].String() != big {
		t.Errorf("integer lost precision: got %s, want %s", got["n"], big)
	}
	if got["payload_version"].String() != DefaultPayloadVersion {
		t.Errorf("payload_version = %s, want %q", got["payload_version"], DefaultPayloadVersion)
	}
	// The literal digits survive in the raw output too.
	if !strings.Contains(string(out), big) {
		t.Errorf("raw output %s does not contain the literal %s", out, big)
	}
}
