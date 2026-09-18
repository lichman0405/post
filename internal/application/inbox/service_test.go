package inbox

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// Task T1003 unit suite for the application layer. It pins the ORDER the
// service does things in and the translation of the store's sentinels —
// the two things the database-backed suite cannot observe, because there
// everything succeeds and every failure is already a wire code.
//
// The fake store counts its calls: a service that validated AFTER calling
// the store, or that sent an anchor it should have refused, would behave
// identically against a happy database and differently here.

const (
	testUser   = "3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80"
	testAnchor = "7c1b2d33-4e55-4a66-9b77-8c99d0e1f2a3"
	testTarget = "aeaa29f4-8ef5-4b25-96a9-812583b47aa1"
)

type fakeStore struct {
	entries []events.InboxEntry
	unread  int
	readN   int
	allN    int
	// err is returned by whichever method is called when the specific
	// error below is not set.
	err     error
	listErr error
	readErr error
	allErr  error

	listCalls, readCalls, allCalls int
	lastUserID, lastFilter         string
	lastLimit                      int
	lastAnchors                    []string
}

func (s *fakeStore) InboxEntries(_ context.Context, userID, filter string, limit int) ([]events.InboxEntry, int, error) {
	s.listCalls++
	s.lastUserID, s.lastFilter, s.lastLimit = userID, filter, limit
	if s.listErr != nil {
		return nil, 0, s.listErr
	}
	if s.err != nil {
		return nil, 0, s.err
	}
	return s.entries, s.unread, nil
}

func (s *fakeStore) MarkInboxRead(_ context.Context, userID string, deliveryIDs []string) (int, error) {
	s.readCalls++
	s.lastUserID, s.lastAnchors = userID, deliveryIDs
	if s.readErr != nil {
		return 0, s.readErr
	}
	if s.err != nil {
		return 0, s.err
	}
	return s.readN, nil
}

func (s *fakeStore) MarkInboxAllRead(_ context.Context, userID string) (int, error) {
	s.allCalls++
	s.lastUserID = userID
	if s.allErr != nil {
		return 0, s.allErr
	}
	if s.err != nil {
		return 0, s.err
	}
	return s.allN, nil
}

func testActor() domain.User { return domain.User{ID: testUser, Handle: "alice"} }

func inboxEntry(targetType, targetID, eventType string) events.InboxEntry {
	return events.InboxEntry{
		TargetType:       targetType,
		TargetID:         targetID,
		TargetLabel:      "MOF Screening",
		EventType:        eventType,
		WindowStart:      time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC),
		Count:            3,
		Unread:           3,
		FirstAt:          time.Date(2026, 9, 16, 10, 0, 1, 0, time.UTC),
		LastAt:           time.Date(2026, 9, 16, 10, 0, 3, 0, time.UTC),
		LatestEventID:    "0d95b076-1e3b-43db-9201-ed11365b72a9",
		LatestDeliveryID: testAnchor,
	}
}

// TestListDefaultsTheViewAndClampsThePage: the two decisions the service
// makes about its arguments before the store sees them.
func TestListDefaultsTheViewAndClampsThePage(t *testing.T) {
	cases := []struct {
		name       string
		filter     string
		limit      int
		wantFilter string
		wantLimit  int
	}{
		{"no view named is the unread view", "", 0, events.InboxFilterUnread, events.DefaultInboxLimit},
		{"the all view is passed through", events.InboxFilterAll, 7, events.InboxFilterAll, 7},
		{"an over-large page is capped", events.InboxFilterUnread, 1 << 20, events.InboxFilterUnread, events.MaxInboxLimit},
		{"a negative page is no page named", events.InboxFilterAll, -1, events.InboxFilterAll, events.DefaultInboxLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			svc := NewService(store)
			view, err := svc.List(context.Background(), testActor(), tc.filter, tc.limit)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if store.listCalls != 1 {
				t.Fatalf("the store was called %d times, want 1", store.listCalls)
			}
			if store.lastUserID != testUser {
				t.Errorf("the store was asked for user %q, want the actor %q", store.lastUserID, testUser)
			}
			if store.lastFilter != tc.wantFilter {
				t.Errorf("the store was asked for view %q, want %q", store.lastFilter, tc.wantFilter)
			}
			if store.lastLimit != tc.wantLimit {
				t.Errorf("the store was asked for a page of %d, want %d", store.lastLimit, tc.wantLimit)
			}
			// The view echoes what was SERVED, not what was asked: a client
			// cannot mistake a defaulted page for the one it named.
			if view.Filter != tc.wantFilter {
				t.Errorf("the view echoes %q, want %q", view.Filter, tc.wantFilter)
			}
		})
	}
}

// TestListRefusesAnUnknownViewBeforeTheStore: an unknown view is refused at
// the boundary. Serving a different list than the one asked for would be
// worse than an error, and reaching the store first would mean a query per
// bad request.
func TestListRefusesAnUnknownViewBeforeTheStore(t *testing.T) {
	store := &fakeStore{}
	_, err := NewService(store).List(context.Background(), testActor(), "everything", 0)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("List with an unknown view = %v, want ErrValidation", err)
	}
	if store.listCalls != 0 {
		t.Errorf("the store was called %d times for an unknown view, want 0", store.listCalls)
	}
}

func TestListLinksEachEntryAndMapsFailures(t *testing.T) {
	t.Run("each entry carries its subject's route", func(t *testing.T) {
		store := &fakeStore{
			entries: []events.InboxEntry{
				inboxEntry(events.TargetTypeProject, testTarget, "state.committed"),
				inboxEntry(events.TargetTypeAsset, "01j9z6k3m4n5p6q7r8s9t0v1w2", "research_asset.version_published"),
				inboxEntry(events.TargetTypeOrganization, testTarget, "project.created"),
			},
			unread: 3,
		}
		view, err := NewService(store).List(context.Background(), testActor(), "", 0)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if view.UnreadEntries != 3 {
			t.Errorf("the badge = %d, want the store's 3", view.UnreadEntries)
		}
		if len(view.Entries) != 3 {
			t.Fatalf("entries = %d, want 3", len(view.Entries))
		}
		// Order is the store's (newest first) — the service does not
		// re-sort a page it was handed.
		wantURLs := []string{
			"/projects/" + testTarget + "/activity",
			"/assets/01j9z6k3m4n5p6q7r8s9t0v1w2",
			"",
		}
		for i, want := range wantURLs {
			if view.Entries[i].URL != want {
				t.Errorf("entry %d links to %q, want %q", i, view.Entries[i].URL, want)
			}
		}
	})

	t.Run("a failed read is a store failure, not an empty inbox", func(t *testing.T) {
		store := &fakeStore{listErr: errors.New("connection reset")}
		_, err := NewService(store).List(context.Background(), testActor(), "", 0)
		if !errors.Is(err, ErrStore) {
			t.Fatalf("List over a failing store = %v, want ErrStore", err)
		}
		// The store's text stays in the error value (it is what a log
		// line is for); what a CLIENT reads is the handler's fixed
		// message, pinned in cmd/api/inboxhttp's mapping test.
	})
}

// TestMarkReadValidatesAnchorsBeforeTheStore: shape and count are decided
// here, so a malformed anchor never reaches a query that would cast it and
// an oversized batch never reaches the database at all.
func TestMarkReadValidatesAnchorsBeforeTheStore(t *testing.T) {
	t.Run("an empty list is a no-op, not a call", func(t *testing.T) {
		store := &fakeStore{}
		n, err := NewService(store).MarkRead(context.Background(), testActor(), nil)
		if err != nil || n != 0 {
			t.Fatalf("MarkRead(nil) = %d, %v, want 0, nil", n, err)
		}
		if store.readCalls != 0 {
			t.Errorf("the store was called %d times for an empty list, want 0", store.readCalls)
		}
	})

	t.Run("a malformed anchor is refused", func(t *testing.T) {
		store := &fakeStore{}
		_, err := NewService(store).MarkRead(context.Background(), testActor(), []string{"not-a-uuid"})
		if !errors.Is(err, ErrValidation) {
			t.Fatalf("MarkRead with a malformed anchor = %v, want ErrValidation", err)
		}
		if store.readCalls != 0 {
			t.Errorf("the store was called %d times, want 0 — the id shape is a boundary check",
				store.readCalls)
		}
	})

	t.Run("a batch over the maximum is refused, not truncated", func(t *testing.T) {
		store := &fakeStore{}
		ids := make([]string, MaxReadAnchors+1)
		for i := range ids {
			ids[i] = fmt.Sprintf("%08d-2222-3333-4444-555555555555", i)
		}
		if _, err := NewService(store).MarkRead(context.Background(), testActor(), ids); !errors.Is(err, ErrValidation) {
			t.Fatalf("MarkRead with %d anchors = %v, want ErrValidation", len(ids), err)
		}
		if store.readCalls != 0 {
			t.Errorf("the store was called %d times, want 0 — silently marking the first %d of %d "+
				"would be a lie about what was read", store.readCalls, MaxReadAnchors, len(ids))
		}
	})

	t.Run("a full page of anchors is accepted", func(t *testing.T) {
		store := &fakeStore{readN: MaxReadAnchors}
		ids := make([]string, MaxReadAnchors)
		for i := range ids {
			ids[i] = fmt.Sprintf("%08d-2222-3333-4444-555555555555", i)
		}
		n, err := NewService(store).MarkRead(context.Background(), testActor(), ids)
		if err != nil {
			t.Fatalf("MarkRead with %d anchors: %v", len(ids), err)
		}
		if n != MaxReadAnchors || store.readCalls != 1 {
			t.Errorf("MarkRead = %d marked / %d store calls, want %d / 1", n, store.readCalls, MaxReadAnchors)
		}
		if store.lastUserID != testUser {
			t.Errorf("the store was asked to mark for %q, want the actor %q", store.lastUserID, testUser)
		}
		if len(store.lastAnchors) != MaxReadAnchors {
			t.Errorf("the store received %d anchors, want %d — every anchor the caller named must be "+
				"named to it", len(store.lastAnchors), MaxReadAnchors)
		}
	})
}

// TestMarkReadTranslatesTheStoreSentinels: the store's "not yours / does
// not exist" is the service's ErrNotFound, and anything else is a store
// failure. A caller must be able to tell them apart: one is a 404, the
// other a 503 that is worth retrying.
func TestMarkReadTranslatesTheStoreSentinels(t *testing.T) {
	cases := []struct {
		name    string
		store   *fakeStore
		wantErr error
	}{
		{"an anchor the caller cannot mark is not found", &fakeStore{readErr: events.ErrInboxDeliveryNotFound}, ErrNotFound},
		{"a wrapped not-found is still not found", &fakeStore{readErr: fmt.Errorf("events: mark: %w", events.ErrInboxDeliveryNotFound)}, ErrNotFound},
		{"any other failure is the store's", &fakeStore{readErr: errors.New("deadlock detected")}, ErrStore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewService(tc.store).MarkRead(context.Background(), testActor(), []string{testAnchor})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("MarkRead = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestMarkAllRead(t *testing.T) {
	t.Run("a successful call reports what it marked", func(t *testing.T) {
		store := &fakeStore{allN: 5}
		n, err := NewService(store).MarkAllRead(context.Background(), testActor())
		if err != nil || n != 5 {
			t.Fatalf("MarkAllRead = %d, %v, want 5, nil", n, err)
		}
		if store.lastUserID != testUser {
			t.Errorf("the store was asked for %q, want the actor %q", store.lastUserID, testUser)
		}
	})

	t.Run("a failure is the store's, never a silent success", func(t *testing.T) {
		store := &fakeStore{allErr: errors.New("connection reset")}
		_, err := NewService(store).MarkAllRead(context.Background(), testActor())
		if !errors.Is(err, ErrStore) {
			t.Fatalf("MarkAllRead over a failing store = %v, want ErrStore", err)
		}
	})
}

// TestEntryURL: the deep link rule. A route that exists is a link; a
// target with no surface yet is "" — an href to a page that does not exist
// would be a 404 dressed as a notification.
func TestEntryURL(t *testing.T) {
	cases := []struct {
		targetType string
		id         string
		want       string
	}{
		{events.TargetTypeProject, testTarget, "/projects/" + testTarget + "/activity"},
		{events.TargetTypeAsset, "01j9z6k3m4n5p6q7r8s9t0v1w2", "/assets/01j9z6k3m4n5p6q7r8s9t0v1w2"},
		{events.TargetTypeUser, testUser, "/users/" + testUser},
		// No route exists for these yet: "" is the link, not a guess
		// (knowledge is T0805's surface, organizations T0808's).
		{events.TargetTypeOrganization, testTarget, ""},
		{events.TargetTypeKnowledge, testTarget, ""},
		{"something_else", testTarget, ""},
	}
	for _, tc := range cases {
		t.Run(tc.targetType, func(t *testing.T) {
			if got := EntryURL(tc.targetType, tc.id); got != tc.want {
				t.Errorf("EntryURL(%q, %q) = %q, want %q", tc.targetType, tc.id, got, tc.want)
			}
		})
	}
}
