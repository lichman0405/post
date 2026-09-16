package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// Task T1002 unit suite for the application layer. It pins the ORDER the
// service does things in and the translation of the store's sentinels —
// the two things the database-backed suite cannot observe, because there
// everything succeeds and every error is already a wire code.
//
// The fake store counts its calls: a service that validated AFTER calling
// the store, or authorized AFTER creating the row, would behave the same
// against a happy database and differently here.

const (
	testUser = "3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80"
	testSub  = "7c1b2d33-4e55-4a66-9b77-8c99d0e1f2a3"
)

type fakeStore struct {
	level events.AudienceLevel
	// err, when set, is returned by the ownership and write methods.
	err error
	// audienceErr is deliberately a separate field: the audience lookup has
	// its own failure mode (the database could not answer), and routing a
	// create-path sentinel through it would exercise a state the real store
	// cannot produce.
	audienceErr error

	createCalls, listCalls, getCalls, updateCalls, deleteCalls, audienceCalls int
	lastUserID                                                                string
	lastFilters, lastChannels                                                 []string
	lastTarget                                                                events.Target
}

func (s *fakeStore) CreateSubscription(_ context.Context, userID string, target events.Target, filters, channels []string) (events.Subscription, error) {
	s.createCalls++
	s.lastUserID, s.lastTarget, s.lastFilters, s.lastChannels = userID, target, filters, channels
	if s.err != nil {
		return events.Subscription{}, s.err
	}
	return events.Subscription{ID: testSub, UserID: userID, TargetType: target.Type, TargetID: target.ID,
		EventFilters: filters, Channels: channels}, nil
}

func (s *fakeStore) ListSubscriptions(_ context.Context, userID, targetType, targetID string) ([]events.Subscription, error) {
	s.listCalls++
	s.lastUserID, s.lastTarget = userID, events.Target{Type: targetType, ID: targetID}
	if s.err != nil {
		return nil, s.err
	}
	return []events.Subscription{{ID: testSub, UserID: userID}}, nil
}

func (s *fakeStore) GetSubscription(_ context.Context, userID, id string) (events.Subscription, error) {
	s.getCalls++
	s.lastUserID = userID
	if s.err != nil {
		return events.Subscription{}, s.err
	}
	return events.Subscription{ID: id, UserID: userID, TargetType: events.TargetTypeProject, TargetID: testUser}, nil
}

func (s *fakeStore) UpdateSubscription(_ context.Context, userID, id string, filters, channels []string) (events.Subscription, error) {
	s.updateCalls++
	s.lastUserID, s.lastFilters, s.lastChannels = userID, filters, channels
	if s.err != nil {
		return events.Subscription{}, s.err
	}
	return events.Subscription{ID: id, UserID: userID, EventFilters: filters, Channels: channels}, nil
}

func (s *fakeStore) DeleteSubscription(_ context.Context, userID, id string) error {
	s.deleteCalls++
	s.lastUserID = userID
	return s.err
}

func (s *fakeStore) TargetAudienceFor(_ context.Context, target events.Target, userID string) (events.AudienceLevel, error) {
	s.audienceCalls++
	s.lastUserID, s.lastTarget = userID, target
	if s.audienceErr != nil {
		return events.AudienceNone, s.audienceErr
	}
	return s.level, nil
}

func actor() domain.User { return domain.User{ID: testUser, Handle: "alice"} }

func okTarget() events.Target {
	return events.Target{Type: events.TargetTypeProject, ID: testSub}
}

// TestSubscribeAuthorizesBeforeWriting pins the order of the two decisions
// a subscribe makes: the shape rules, then the audience, and only then the
// row. A service that wrote first (or authorized after) would leave a
// subscription behind for a target the actor may not see.
func TestSubscribeAuthorizesBeforeWriting(t *testing.T) {
	ctx := context.Background()

	t.Run("a refused audience never reaches the store's write", func(t *testing.T) {
		store := &fakeStore{level: events.AudienceNone}
		_, err := NewService(store).Subscribe(ctx, actor(), okTarget(), nil, []string{events.ChannelWeb})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Subscribe on an unseen target = %v, want ErrNotFound", err)
		}
		if store.createCalls != 0 {
			t.Errorf("CreateSubscription called %d times for an unseen target, want 0", store.createCalls)
		}
		if store.audienceCalls != 1 {
			t.Errorf("TargetAudienceFor called %d times, want 1", store.audienceCalls)
		}
	})

	t.Run("a malformed target never reaches the store at all", func(t *testing.T) {
		store := &fakeStore{level: events.AudienceMember}
		svc := NewService(store)
		bad := []struct {
			name     string
			target   events.Target
			filters  []string
			channels []string
		}{
			{"unknown type", events.Target{Type: "planet", ID: testSub}, nil, []string{events.ChannelWeb}},
			{"unshaped id", events.Target{Type: events.TargetTypeProject, ID: "nope"}, nil, []string{events.ChannelWeb}},
			{"bad filter", okTarget(), []string{"not a filter"}, []string{events.ChannelWeb}},
			{"no channel", okTarget(), nil, nil},
			{"unknown channel", okTarget(), nil, []string{"sms"}},
		}
		for _, tc := range bad {
			if _, err := svc.Subscribe(ctx, actor(), tc.target, tc.filters, tc.channels); !errors.Is(err, ErrValidation) {
				t.Errorf("%s: Subscribe = %v, want ErrValidation", tc.name, err)
			}
		}
		if store.audienceCalls != 0 || store.createCalls != 0 {
			t.Errorf("the store was reached %d/%d times by malformed input, want 0/0",
				store.audienceCalls, store.createCalls)
		}
	})

	t.Run("a permitted subscribe reaches both, in that order", func(t *testing.T) {
		store := &fakeStore{level: events.AudienceMember}
		sub, err := NewService(store).Subscribe(ctx, actor(), okTarget(), []string{"state.committed"},
			[]string{events.ChannelWeb, events.ChannelEmail})
		if err != nil {
			t.Fatalf("Subscribe = %v, want nil", err)
		}
		if store.audienceCalls != 1 || store.createCalls != 1 {
			t.Fatalf("audience/create calls = %d/%d, want 1/1", store.audienceCalls, store.createCalls)
		}
		if sub.ID != testSub || sub.UserID != testUser {
			t.Errorf("Subscribe returned %+v", sub)
		}
		if store.lastTarget != okTarget() {
			t.Errorf("the store was handed %+v, want %+v", store.lastTarget, okTarget())
		}
		if len(store.lastChannels) != 2 || len(store.lastFilters) != 1 {
			t.Errorf("filters/channels handed to the store = %v/%v", store.lastFilters, store.lastChannels)
		}
	})
}

// TestUpdateRechecksAudience: changing what a subscription lets through is
// a write on a target, so it re-resolves the actor's access — a lost
// permission must not be editable around.
func TestUpdateRechecksAudience(t *testing.T) {
	ctx := context.Background()

	store := &fakeStore{level: events.AudienceNone}
	_, err := NewService(store).Update(ctx, actor(), testSub, nil, []string{events.ChannelWeb})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update on an unseen target = %v, want ErrNotFound", err)
	}
	if store.updateCalls != 0 {
		t.Errorf("UpdateSubscription called %d times, want 0", store.updateCalls)
	}

	store = &fakeStore{level: events.AudiencePublic}
	if _, err := NewService(store).Update(ctx, actor(), testSub, nil, []string{events.ChannelEmail}); err != nil {
		t.Fatalf("Update = %v, want nil", err)
	}
	if store.getCalls != 1 || store.audienceCalls != 1 || store.updateCalls != 1 {
		t.Errorf("get/audience/update calls = %d/%d/%d, want 1/1/1",
			store.getCalls, store.audienceCalls, store.updateCalls)
	}
}

// TestUnsubscribeDoesNotRecheckAudience pins a deliberate asymmetry: an
// unsubscribe must work exactly when access has been LOST, otherwise a
// user removed from a private project could never clean up the dead
// subscription. Owner scoping is the store's, and it is what makes the
// foreign id answer ErrNotFound.
func TestUnsubscribeDoesNotRecheckAudience(t *testing.T) {
	ctx := context.Background()

	store := &fakeStore{level: events.AudienceNone}
	if err := NewService(store).Unsubscribe(ctx, actor(), testSub); err != nil {
		t.Fatalf("Unsubscribe = %v, want nil even when the target is no longer visible", err)
	}
	if store.audienceCalls != 0 {
		t.Errorf("Unsubscribe resolved the audience %d times, want 0", store.audienceCalls)
	}
	if store.deleteCalls != 1 {
		t.Errorf("DeleteSubscription called %d times, want 1", store.deleteCalls)
	}
}

// TestServiceErrorMapping pins the store sentinels to this package's —
// the seam a transport reads.
func TestServiceErrorMapping(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name        string
		from        error
		audienceErr error
		want        error
		call        func(*Service) error
	}{
		{"not found", events.ErrSubscriptionNotFound, nil, ErrNotFound,
			func(s *Service) error { _, err := s.Get(ctx, actor(), testSub); return err }},
		{"exists", events.ErrSubscriptionExists, nil, ErrExists,
			func(s *Service) error {
				_, err := s.Subscribe(ctx, actor(), okTarget(), nil, []string{events.ChannelWeb})
				return err
			}},
		{"limit", events.ErrSubscriptionLimit, nil, ErrLimit,
			func(s *Service) error {
				_, err := s.Subscribe(ctx, actor(), okTarget(), nil, []string{events.ChannelWeb})
				return err
			}},
		{"anything else is a store failure", errors.New("connection refused"), nil, ErrStore,
			func(s *Service) error { _, err := s.List(ctx, actor(), "", ""); return err }},
		{"a transport error is not a client error", fmt.Errorf("wrapped: %w", errors.New("boom")), nil, ErrStore,
			func(s *Service) error { return s.Unsubscribe(ctx, actor(), testSub) }},
		// The one that matters for the second acceptance criterion: an
		// audience lookup that FAILED is an outage, not a revocation. It
		// must not be answered as "you cannot see this" (404), and it must
		// not slip through as a permitted subscribe either.
		{"an audience lookup that failed is not a refusal", nil, errors.New("could not reach the database"), ErrStore,
			func(s *Service) error {
				_, err := s.Subscribe(ctx, actor(), okTarget(), nil, []string{events.ChannelWeb})
				return err
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{level: events.AudienceMember, err: tc.from, audienceErr: tc.audienceErr}
			err := tc.call(NewService(store))
			if !errors.Is(err, tc.want) {
				t.Fatalf("= %v, want %v", err, tc.want)
			}
			// The cause must survive for the log, and must never be a
			// sentinel of the OTHER kind (an ErrNotFound that was really a
			// store failure would answer 404 for an outage).
			if errors.Is(err, ErrNotFound) && tc.want != ErrNotFound {
				t.Errorf("a %v surfaced as ErrNotFound", tc.from)
			}
		})
	}
}

// TestServiceIsOwnerScoped: every operation hands the store the ACTOR's id,
// never an id from the request. That is the whole of the isolation the
// service can enforce itself — the store then filters by it, and a foreign
// subscription id answers ErrNotFound.
func TestServiceIsOwnerScoped(t *testing.T) {
	ctx := context.Background()
	other := domain.User{ID: "00000000-0000-4000-8000-000000000000", Handle: "bob"}
	cases := []struct {
		name string
		call func(*Service, domain.User) error
	}{
		{"List", func(s *Service, u domain.User) error { _, err := s.List(ctx, u, "", ""); return err }},
		{"Get", func(s *Service, u domain.User) error { _, err := s.Get(ctx, u, testSub); return err }},
		{"Update", func(s *Service, u domain.User) error {
			_, err := s.Update(ctx, u, testSub, nil, []string{events.ChannelWeb})
			return err
		}},
		{"Unsubscribe", func(s *Service, u domain.User) error { return s.Unsubscribe(ctx, u, testSub) }},
		{"Subscribe", func(s *Service, u domain.User) error {
			_, err := s.Subscribe(ctx, u, okTarget(), nil, []string{events.ChannelWeb})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{level: events.AudienceMember}
			svc := NewService(store)
			if err := tc.call(svc, other); err != nil {
				t.Fatalf("%s = %v, want nil", tc.name, err)
			}
			if store.lastUserID != other.ID {
				t.Errorf("%s handed the store user %q, want the actor %q", tc.name, store.lastUserID, other.ID)
			}
		})
	}
}

// TestListRequiresTargetTypeWithTargetID: the target id's shape is per
// type, so an id without its type cannot be validated — and guessing would
// silently list the wrong target's rows.
func TestListRequiresTargetTypeWithTargetID(t *testing.T) {
	ctx := context.Background()
	store := &fakeStore{}
	svc := NewService(store)

	if _, err := svc.List(ctx, actor(), "", testSub); !errors.Is(err, ErrValidation) {
		t.Errorf("List(id without type) = %v, want ErrValidation", err)
	}
	if _, err := svc.List(ctx, actor(), "planet", ""); !errors.Is(err, ErrValidation) {
		t.Errorf("List(unknown type) = %v, want ErrValidation", err)
	}
	if _, err := svc.List(ctx, actor(), events.TargetTypeProject, "not-a-uuid"); !errors.Is(err, ErrValidation) {
		t.Errorf("List(unshaped id) = %v, want ErrValidation", err)
	}
	if store.listCalls != 0 {
		t.Fatalf("the store was reached %d times by malformed reads, want 0", store.listCalls)
	}

	// The control: the well-formed forms do reach the store, so "never
	// called" above is about the validation and not about a broken fixture.
	if _, err := svc.List(ctx, actor(), "", ""); err != nil {
		t.Fatalf("List(all) = %v, want nil", err)
	}
	if _, err := svc.List(ctx, actor(), events.TargetTypeProject, testSub); err != nil {
		t.Fatalf("List(target) = %v, want nil", err)
	}
	if store.listCalls != 2 {
		t.Errorf("ListSubscriptions called %d times, want 2", store.listCalls)
	}
}

// TestSubscribeKeepsTheCallersChannelOrder: the channel list is the
// subscription's data, not the service's to normalize. Reordering it would
// make a PATCH that changed nothing look like a change.
func TestSubscribeKeepsTheCallersChannelOrder(t *testing.T) {
	store := &fakeStore{level: events.AudienceMember}
	channels := []string{events.ChannelEmail, events.ChannelWeb}
	if _, err := NewService(store).Subscribe(context.Background(), actor(), okTarget(), nil, channels); err != nil {
		t.Fatalf("Subscribe = %v", err)
	}
	if len(store.lastChannels) != 2 ||
		store.lastChannels[0] != events.ChannelEmail || store.lastChannels[1] != events.ChannelWeb {
		t.Fatalf("channels reaching the store = %v, want the caller's order", store.lastChannels)
	}
}
