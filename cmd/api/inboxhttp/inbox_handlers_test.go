package inboxhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/inbox"
	"github.com/lichman0405/post/internal/events"
)

// Task T1003 unit suite for the HTTP surface. Three things are pinned
// here, all of them reachable without a guard-attached principal (the
// authenticated paths are driven end to end by tests/integration, which
// composes the real guard):
//
//   - the surface fails closed: with no principal in the request, every
//     route answers 401 and the data layer is never touched. This is the
//     handler's own refusal, independent of the /api/v1 guard — if a route
//     were ever mounted outside the guard, it would still not serve an
//     anonymous caller.
//   - the error mapping: which sentinel becomes which wire code, and that
//     a dependency's error text never reaches the envelope.
//   - the entry payload shape: the fields a client reads, and that "read"
//     is derived from the counters rather than a second stored flag.

type fakeStore struct {
	entries []events.InboxEntry
	unread  int
	calls   int
}

func (s *fakeStore) InboxEntries(context.Context, string, string, int) ([]events.InboxEntry, int, error) {
	s.calls++
	return s.entries, s.unread, nil
}

func (s *fakeStore) MarkInboxRead(context.Context, string, []string) (int, error) {
	s.calls++
	return 1, nil
}

func (s *fakeStore) MarkInboxAllRead(context.Context, string) (int, error) {
	s.calls++
	return 1, nil
}

func newTestServer(store *fakeStore) *httptest.Server {
	// The routes as cmd/api/main.go mounts them (the two prefixes are what
	// makes the subtree reachable under /api/v1/inbox/...), without the
	// guard — so what answers below is the handler.
	mux := http.NewServeMux()
	api := New(Deps{Store: store})
	mux.Handle("/api/v1/inbox", api.Routes())
	mux.Handle("/api/v1/inbox/", api.Routes())
	return httptest.NewServer(mux)
}

// TestInboxRoutesRefuseAnAnonymousCaller: no route serves or changes
// anything without a principal, and none of them reaches the store to find
// that out.
func TestInboxRoutesRefuseAnAnonymousCaller(t *testing.T) {
	store := &fakeStore{}
	ts := newTestServer(store)
	defer ts.Close()

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/v1/inbox"},
		{http.MethodGet, "/api/v1/inbox?filter=all&limit=5"},
		{http.MethodPost, "/api/v1/inbox/read"},
		{http.MethodPost, "/api/v1/inbox/read-all"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader("{}"))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("%s %s: %v", tc.method, tc.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("%s %s = %d, want 401", tc.method, tc.path, resp.StatusCode)
			}
			var env struct {
				Code string `json:"code"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode the envelope: %v", err)
			}
			if env.Code != authn.CodeUnauthenticated {
				t.Errorf("code = %q, want %q", env.Code, authn.CodeUnauthenticated)
			}
		})
	}
	if store.calls != 0 {
		t.Errorf("the data layer was reached %d times by anonymous calls, want 0", store.calls)
	}
}

// TestInboxErrorMapping: the sentinels the service raises become the codes
// a client switches on, and the unexpected case answers a fixed string —
// the envelope is not a place for a dependency's error text.
func TestInboxErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantRetry  bool
	}{
		{"an unmarkable anchor is a 404", inbox.ErrNotFound, http.StatusNotFound, inbox.CodeNotFound, false},
		{"a wrapped sentinel is still that sentinel", fmt.Errorf("%w: at most 200 entries", inbox.ErrValidation), http.StatusBadRequest, inbox.CodeValidationFailed, false},
		{"malformed input is a 400", inbox.ErrValidation, http.StatusBadRequest, inbox.CodeValidationFailed, false},
		{"a failed store is a retryable 503", inbox.ErrStore, http.StatusServiceUnavailable, inbox.CodeServiceUnavailable, true},
		{"anything else is an opaque 500", errors.New("pgx: query failed: relation does not exist"),
			http.StatusInternalServerError, "INTERNAL_ERROR", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/inbox", nil)
			inboxError(rec, req, tc.err)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			var env struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				Retryable bool   `json:"retryable"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode the envelope %q: %v", rec.Body.String(), err)
			}
			if env.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Code, tc.wantCode)
			}
			if env.Retryable != tc.wantRetry {
				t.Errorf("retryable = %v, want %v", env.Retryable, tc.wantRetry)
			}
			if env.Message == "" {
				t.Error("the envelope carries no message")
			}
			for _, leak := range []string{"pgx", "relation does not exist", "connection"} {
				if strings.Contains(env.Message, leak) {
					t.Errorf("the message leaks the dependency's text (%q): %q", leak, env.Message)
				}
			}
		})
	}
}

// TestEntryPayloadShape: what a client reads off one entry. The fields are
// the contract; "read" being derived from the counters is what keeps the
// surface from needing a second source of truth for read state.
func TestEntryPayloadShape(t *testing.T) {
	window := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	entry := inbox.Entry{
		InboxEntry: events.InboxEntry{
			TargetType:       events.TargetTypeProject,
			TargetID:         "aeaa29f4-8ef5-4b25-96a9-812583b47aa1",
			TargetLabel:      "MOF Screening",
			EventType:        "state.committed",
			WindowStart:      window,
			Count:            40,
			Unread:           0,
			FirstAt:          window.Add(time.Minute),
			LastAt:           window.Add(40 * time.Minute),
			LatestEventID:    "0d95b076-1e3b-43db-9201-ed11365b72a9",
			LatestDeliveryID: "7c1b2d33-4e55-4a66-9b77-8c99d0e1f2a3",
		},
		URL: "/projects/aeaa29f4-8ef5-4b25-96a9-812583b47aa1/activity",
	}

	raw, err := json.Marshal(entryPayload(entry))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := map[string]any{
		"target_type":        "project",
		"target_id":          "aeaa29f4-8ef5-4b25-96a9-812583b47aa1",
		"target_label":       "MOF Screening",
		"event_type":         "state.committed",
		"count":              float64(40),
		"unread":             float64(0),
		"read":               true,
		"latest_event_id":    "0d95b076-1e3b-43db-9201-ed11365b72a9",
		"latest_delivery_id": "7c1b2d33-4e55-4a66-9b77-8c99d0e1f2a3",
		"url":                "/projects/aeaa29f4-8ef5-4b25-96a9-812583b47aa1/activity",
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Errorf("%s = %v, want %v", key, got[key], wantValue)
		}
	}
	for _, key := range []string{"window_start", "first_at", "last_at"} {
		if _, ok := got[key].(string); !ok {
			t.Errorf("%s = %v, want an RFC3339 timestamp string", key, got[key])
		}
	}
	if len(got) != len(want)+3 {
		t.Errorf("the payload has %d fields, want %d — a field added or dropped silently is a "+
			"contract change: %v", len(got), len(want)+3, raw)
	}

	// One unread delivery flips the whole entry's read flag: "read" is the
	// entry's, not the newest delivery's.
	entry.Unread = 1
	raw, err = json.Marshal(entryPayload(entry))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = map[string]any{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["read"] != false || got["unread"] != float64(1) {
		t.Errorf("with one unread delivery the entry reads as %v/%v, want read=false, unread=1",
			got["read"], got["unread"])
	}
}
