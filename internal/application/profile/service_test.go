package profile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/domain"
)

// fakeStore is a scripted ProfileStore: fixed profiles plus recorded
// updates. Broken behavior is injected per test via failWith.
type fakeStore struct {
	byID     map[string]domain.Profile
	byHandle map[string]string
	updates  []struct {
		userID string
		upd    Update
	}
	failWith error
}

func newFakeStore() *fakeStore {
	alice := domain.Profile{User: domain.User{
		ID: "user-alice", Handle: "alice", Email: "alice@example.com",
		DisplayName: "Alice", CreatedAt: time.Now().UTC(),
	}}
	bob := domain.Profile{User: domain.User{
		ID: "user-bob", Handle: "bob", Email: "bob@example.com",
		DisplayName: "Bob", CreatedAt: time.Now().UTC(),
	}}
	return &fakeStore{
		byID:     map[string]domain.Profile{"user-alice": alice, "user-bob": bob},
		byHandle: map[string]string{"alice": "user-alice", "bob": "user-bob"},
	}
}

func (f *fakeStore) GetByUserID(_ context.Context, userID string) (domain.Profile, error) {
	if f.failWith != nil {
		return domain.Profile{}, f.failWith
	}
	p, ok := f.byID[userID]
	if !ok {
		return domain.Profile{}, ErrNotFound
	}
	return p, nil
}

func (f *fakeStore) GetByHandle(_ context.Context, handle string) (domain.Profile, error) {
	if f.failWith != nil {
		return domain.Profile{}, f.failWith
	}
	id, ok := f.byHandle[handle]
	if !ok {
		return domain.Profile{}, ErrNotFound
	}
	return f.byID[id], nil
}

func (f *fakeStore) Update(_ context.Context, userID string, upd Update) (domain.Profile, error) {
	f.updates = append(f.updates, struct {
		userID string
		upd    Update
	}{userID, upd})
	if f.failWith != nil {
		return domain.Profile{}, f.failWith
	}
	p, ok := f.byID[userID]
	if !ok {
		return domain.Profile{}, ErrNotFound
	}
	if upd.Handle != nil {
		delete(f.byHandle, p.User.Handle)
		p.User.Handle = *upd.Handle
		f.byHandle[p.User.Handle] = userID
	}
	if upd.DisplayName != nil {
		p.User.DisplayName = *upd.DisplayName
	}
	if upd.Bio != nil {
		p.Bio = *upd.Bio
	}
	f.byID[userID] = p
	return p, nil
}

func strPtr(s string) *string { return &s }

func TestServiceGet(t *testing.T) {
	svc := NewService(newFakeStore())

	p, err := svc.Get(context.Background(), "user-alice")
	if err != nil || p.User.Handle != "alice" {
		t.Fatalf("Get = %+v, %v", p, err)
	}
	if _, err := svc.Get(context.Background(), "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(unknown) = %v, want ErrNotFound", err)
	}
}

func TestServiceGetByHandleNormalizes(t *testing.T) {
	svc := NewService(newFakeStore())

	// The store only knows the normalized form; the service must normalize
	// "  ALICE " before the lookup.
	p, err := svc.GetByHandle(context.Background(), "  ALICE ")
	if err != nil || p.User.Handle != "alice" {
		t.Fatalf("GetByHandle = %+v, %v", p, err)
	}
	if _, err := svc.GetByHandle(context.Background(), "carol"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByHandle(unknown) = %v, want ErrNotFound", err)
	}
}

func TestServiceUpdateOwnerOnly(t *testing.T) {
	svc := NewService(newFakeStore())
	upd := Update{DisplayName: strPtr("Alice Two")}

	// The owner may update their own profile.
	if _, err := svc.Update(context.Background(), "user-alice", "user-alice", upd); err != nil {
		t.Fatalf("owner update: %v", err)
	}
	// Anyone else may not — not another signed-in user, not an empty actor
	// (a call path that somehow bypassed the guard).
	if _, err := svc.Update(context.Background(), "user-bob", "user-alice", upd); !errors.Is(err, ErrForbidden) {
		t.Errorf("foreign update = %v, want ErrForbidden", err)
	}
	if _, err := svc.Update(context.Background(), "", "user-alice", upd); !errors.Is(err, ErrForbidden) {
		t.Errorf("empty-actor update = %v, want ErrForbidden", err)
	}
}

func TestServiceUpdateValidation(t *testing.T) {
	svc := NewService(newFakeStore())
	cases := []struct {
		name string
		upd  Update
	}{
		{"handle with spaces", Update{Handle: strPtr("has space")}},
		{"handle too long", Update{Handle: strPtr(strings.Repeat("h", 65))}},
		{"handle leading dash", Update{Handle: strPtr("-alice")}},
		{"empty display name", Update{DisplayName: strPtr("")}},
		{"whitespace display name", Update{DisplayName: strPtr("   ")}},
		{"display name too long", Update{DisplayName: strPtr(strings.Repeat("n", 201))}},
		{"bio too long", Update{Bio: strPtr(strings.Repeat("b", domain.MaxBioLen+1))}},
	}
	for _, tc := range cases {
		if _, err := svc.Update(context.Background(), "user-alice", "user-alice", tc.upd); !errors.Is(err, ErrValidation) {
			t.Errorf("%s = %v, want ErrValidation", tc.name, err)
		}
	}
}

func TestServiceUpdateNormalizesHandleAndClearsBio(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)

	// The handle arrives with case/space noise; the service normalizes
	// before storing. An explicit empty bio clears it.
	p, err := svc.Update(context.Background(), "user-alice", "user-alice", Update{
		Handle: strPtr("  Alice-2 "),
		Bio:    strPtr(""),
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if p.User.Handle != "alice-2" {
		t.Errorf("handle = %q, want normalized alice-2", p.User.Handle)
	}
	if p.Bio != "" {
		t.Errorf("bio = %q, want cleared", p.Bio)
	}
	// The store received the normalized handle (transport of normalization
	// is the service's job, not the adapter's).
	if got := store.updates[0].upd.Handle; got == nil || *got != "alice-2" {
		t.Errorf("store received handle %v, want alice-2", got)
	}
}

func TestServiceUpdateStoreErrors(t *testing.T) {
	// A taken handle passes through as-is; an adapter failure becomes
	// ErrStore (cause kept), unknown target passes through as ErrNotFound.
	store := newFakeStore()
	svc := NewService(store)
	if _, err := svc.Update(context.Background(), "user-alice", "user-alice",
		Update{Handle: strPtr("bob")}); err != nil {
		t.Fatalf("no injected failure yet: %v", err)
	}

	store.failWith = ErrHandleTaken
	if _, err := svc.Update(context.Background(), "user-alice", "user-alice",
		Update{Handle: strPtr("bob")}); !errors.Is(err, ErrHandleTaken) {
		t.Errorf("taken handle = %v, want ErrHandleTaken", err)
	}

	store.failWith = errors.New("connection refused")
	_, err := svc.Update(context.Background(), "user-alice", "user-alice", Update{})
	if !errors.Is(err, ErrStore) {
		t.Errorf("adapter failure = %v, want ErrStore", err)
	}
	store.failWith = nil

	if _, err := svc.Update(context.Background(), "nobody", "nobody", Update{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown target = %v, want ErrNotFound", err)
	}
	// The adapter failure also maps on reads.
	store.failWith = errors.New("connection refused")
	if _, err := svc.Get(context.Background(), "user-alice"); !errors.Is(err, ErrStore) {
		t.Errorf("read adapter failure = %v, want ErrStore", err)
	}
}
