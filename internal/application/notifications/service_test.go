package notifications

import (
	"context"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// T1005 unit suite for the cadence service: the ORDER (validate before
// write), the translation of store sentinels, and the owner scoping — the
// three things a database-backed run cannot observe, because there
// everything succeeds and the actor is always the caller.

type fakeStore struct {
	prefs    events.NotificationPreferences
	getErr   error
	setErr   error
	getCalls int
	setCalls int
	lastID   string
	lastCad  string
}

func (s *fakeStore) GetPreferences(_ context.Context, userID string) (events.NotificationPreferences, error) {
	s.getCalls++
	s.lastID = userID
	if s.getErr != nil {
		return events.NotificationPreferences{}, s.getErr
	}
	return s.prefs, nil
}

func (s *fakeStore) SetCadence(_ context.Context, userID, cadence string) (events.NotificationPreferences, error) {
	s.setCalls++
	s.lastID, s.lastCad = userID, cadence
	if s.setErr != nil {
		return events.NotificationPreferences{}, s.setErr
	}
	return events.NotificationPreferences{UserID: userID, Cadence: cadence, Stored: true}, nil
}

func testActor() domain.User {
	return domain.User{ID: "3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80", Handle: "alice"}
}

func TestServiceSetCadenceValidatesBeforeWriting(t *testing.T) {
	// An unknown cadence must never reach the store: the column's CHECK
	// would refuse it too, but as a constraint violation — an error the
	// caller cannot act on — and the row must not be created at all.
	store := &fakeStore{}
	svc := NewService(store)

	for _, bad := range []string{"", "hourly", "DAILY", "immediate ", "null"} {
		_, err := svc.SetCadence(context.Background(), testActor(), bad)
		if !errors.Is(err, ErrValidation) {
			t.Errorf("SetCadence(%q) = %v, want ErrValidation", bad, err)
		}
	}
	if store.setCalls != 0 {
		t.Errorf("the store was written %d times for invalid cadences, want 0", store.setCalls)
	}

	prefs, err := svc.SetCadence(context.Background(), testActor(), events.CadenceWeekly)
	if err != nil {
		t.Fatalf("SetCadence(weekly): %v", err)
	}
	if prefs.Cadence != events.CadenceWeekly || !prefs.Stored {
		t.Errorf("SetCadence returned %+v, want the stored weekly preference", prefs)
	}
	if store.lastID != testActor().ID {
		t.Errorf("the store was called with user %q, want the actor's own id", store.lastID)
	}
}

func TestServiceScopesEveryCallToTheActor(t *testing.T) {
	// There is no parameter naming another account: the request's subject is
	// the only id this service can pass down, which is what makes "read
	// somebody else's setting" unrepresentable rather than merely unhandled.
	store := &fakeStore{prefs: events.NotificationPreferences{Cadence: events.CadenceImmediate, Stored: true}}
	svc := NewService(store)

	if _, err := svc.Preferences(context.Background(), testActor()); err != nil {
		t.Fatalf("Preferences: %v", err)
	}
	if _, err := svc.SetCadence(context.Background(), testActor(), events.CadenceDaily); err != nil {
		t.Fatalf("SetCadence: %v", err)
	}
	if store.lastID != testActor().ID || store.getCalls != 1 || store.setCalls != 1 {
		t.Errorf("store calls = get %d / set %d for %q, want one each for the actor", store.getCalls, store.setCalls, store.lastID)
	}
}

func TestServicePreferencesDefaults(t *testing.T) {
	// An account that never set a cadence reads as the default, and Stored
	// says so: the client renders "daily (default)" rather than pretending
	// the account chose it.
	store := &fakeStore{prefs: events.NotificationPreferences{}}
	svc := NewService(store)
	prefs, err := svc.Preferences(context.Background(), testActor())
	if err != nil {
		t.Fatalf("Preferences: %v", err)
	}
	if got := prefs.EffectiveCadence(); got != events.DefaultCadence {
		t.Errorf("EffectiveCadence = %q, want the default %q", got, events.DefaultCadence)
	}
	if prefs.Stored {
		t.Error("Stored = true for an account with no row")
	}
}

func TestServiceMapsStoreFailures(t *testing.T) {
	// A dependency outage is SERVICE_UNAVAILABLE, never a 500 and never a
	// silent success: the caller must be able to tell "not stored" from
	// "stored".
	store := &fakeStore{getErr: errors.New("connection reset"), setErr: errors.New("connection reset")}
	svc := NewService(store)
	if _, err := svc.Preferences(context.Background(), testActor()); !errors.Is(err, ErrStore) {
		t.Errorf("Preferences error = %v, want ErrStore", err)
	}
	if _, err := svc.SetCadence(context.Background(), testActor(), events.CadenceDaily); !errors.Is(err, ErrStore) {
		t.Errorf("SetCadence error = %v, want ErrStore", err)
	}
}

func TestServiceCadenceRoundTrip(t *testing.T) {
	// The three cadences the vocabulary declares are exactly the three the
	// service accepts and stores: the setting a user can express is the set
	// the pipeline can act on.
	for _, cadence := range []string{events.CadenceImmediate, events.CadenceDaily, events.CadenceWeekly} {
		store := &fakeStore{}
		svc := NewService(store)
		prefs, err := svc.SetCadence(context.Background(), testActor(), cadence)
		if err != nil {
			t.Errorf("SetCadence(%q): %v", cadence, err)
			continue
		}
		if prefs.Cadence != cadence {
			t.Errorf("SetCadence(%q) stored %q", cadence, prefs.Cadence)
		}
		if !events.ValidCadence(prefs.Cadence) {
			t.Errorf("the service stored %q, which the digest pipeline cannot act on", prefs.Cadence)
		}
	}
}

// (The companion rule — that saving a cadence does not disturb the digest
// anchor last_digest_at — is not observable here: the fake store decides
// what SetCadence returns. It is pinned against the real row in
// tests/integration/email_digest_test.go, TestCadenceChangeKeepsTheAnchor.)
