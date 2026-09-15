package webhooks

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// The service contract over a fake store: the once-only secret, the
// owner scoping, the validation, and the error mapping. The real store
// behaviour is covered by the integration suite
// (tests/integration/webhook_test.go).

type fakeStore struct {
	createErr    error
	getErr       error
	updateErr    error
	deleteErr    error
	redeliverErr error
	created      events.WebhookEndpoint
	updated      events.WebhookEndpoint
	got          events.WebhookEndpoint
	list         []events.WebhookEndpoint
	deliveries   []events.Delivery
}

func (f *fakeStore) CreateEndpoint(ctx context.Context, userID, url, secret string, filters []string) (events.WebhookEndpoint, error) {
	if f.createErr != nil {
		return events.WebhookEndpoint{}, f.createErr
	}
	return f.created, nil
}

func (f *fakeStore) ListEndpoints(ctx context.Context, userID string) ([]events.WebhookEndpoint, error) {
	return f.list, nil
}

func (f *fakeStore) GetEndpoint(ctx context.Context, userID, id string) (events.WebhookEndpoint, error) {
	if f.getErr != nil {
		return events.WebhookEndpoint{}, f.getErr
	}
	return f.got, nil
}

func (f *fakeStore) UpdateEndpoint(ctx context.Context, userID, id string, url *string, filters []string, enabled *bool) (events.WebhookEndpoint, error) {
	if f.updateErr != nil {
		return events.WebhookEndpoint{}, f.updateErr
	}
	return f.updated, nil
}

func (f *fakeStore) RegenerateSecret(ctx context.Context, userID, id, secret string) (events.WebhookEndpoint, error) {
	return f.updated, nil
}

func (f *fakeStore) DeleteEndpoint(ctx context.Context, userID, id string) error {
	return f.deleteErr
}

func (f *fakeStore) ListDeliveries(ctx context.Context, userID, endpointID string, beforeTS *time.Time, beforeID string, limit int) ([]events.Delivery, error) {
	return f.deliveries, nil
}

func (f *fakeStore) Redeliver(ctx context.Context, userID, endpointID, deliveryID string) error {
	return f.redeliverErr
}

func testActor() domain.User {
	return domain.User{ID: "user-1", Handle: "alice"}
}

func TestCreateReturnsSecretOnceAndValidates(t *testing.T) {
	ctx := context.Background()
	f := &fakeStore{created: events.WebhookEndpoint{ID: "e1", UserID: "user-1", URL: "https://example.com/hook"}}
	svc := NewService(f)
	svc.secretSource = func() (string, error) { return "fixed-secret", nil }

	created, err := svc.Create(ctx, testActor(), "https://example.com/hook", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Secret != "fixed-secret" {
		t.Fatalf("Create secret = %q, want the one-time secret", created.Secret)
	}
	if created.WebhookEndpoint.Secret != "" {
		t.Fatal("the embedded endpoint leaks the secret; it must travel in CreatedEndpoint.Secret only")
	}

	// Validation failures answer ErrValidation before the store runs.
	for _, bad := range []string{"", "ftp://x", "https://user:pass@h"} {
		if _, err := svc.Create(ctx, testActor(), bad, nil); !errors.Is(err, ErrValidation) {
			t.Errorf("Create(%q) = %v, want ErrValidation", bad, err)
		}
	}

	// Store sentinels map to the wire sentinels.
	f.createErr = events.ErrWebhookLimit
	if _, err := svc.Create(ctx, testActor(), "https://example.com/hook", nil); !errors.Is(err, ErrLimit) {
		t.Errorf("Create limit = %v, want ErrLimit", err)
	}
}

func TestUpdateValidatesAndMapsNotFound(t *testing.T) {
	ctx := context.Background()
	f := &fakeStore{updated: events.WebhookEndpoint{ID: "e1"}}
	svc := NewService(f)

	badURL := "ftp://x"
	if _, err := svc.Update(ctx, testActor(), "e1", &badURL, nil, nil); !errors.Is(err, ErrValidation) {
		t.Errorf("Update(bad url) = %v, want ErrValidation", err)
	}
	if _, err := svc.Update(ctx, testActor(), "e1", nil, []string{"BAD TYPE!"}, nil); !errors.Is(err, ErrValidation) {
		t.Errorf("Update(bad filters) = %v, want ErrValidation", err)
	}

	f.updateErr = events.ErrWebhookNotFound
	if _, err := svc.Update(ctx, testActor(), "e1", nil, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("Update(missing) = %v, want ErrNotFound", err)
	}
}

func TestRedeliverMapsNotRedeliverable(t *testing.T) {
	ctx := context.Background()
	f := &fakeStore{redeliverErr: events.ErrWebhookNotRedeliverable}
	svc := NewService(f)
	if err := svc.Redeliver(ctx, testActor(), "e1", "d1"); !errors.Is(err, ErrNotRedeliverable) {
		t.Errorf("Redeliver = %v, want ErrNotRedeliverable", err)
	}
}

func TestListDeliveriesValidatesBeforeIDAtTheBoundary(t *testing.T) {
	ctx := context.Background()
	svc := NewService(&fakeStore{})

	// A non-UUID pagination key answers ErrValidation (the handler's 400)
	// before the store ever runs — never a store-side cast error (503).
	for _, bad := range []string{"not-a-uuid", "99999999-9999-4999-8999-", "{}", "1"} {
		if _, err := svc.ListDeliveries(ctx, testActor(), "e1", nil, bad); !errors.Is(err, ErrValidation) {
			t.Errorf("ListDeliveries(before=%q) = %v, want ErrValidation", bad, err)
		}
	}
	// A well-formed UUID passes through to the store untouched.
	uuid := "99999999-9999-4999-8999-999999999999"
	if _, err := svc.ListDeliveries(ctx, testActor(), "e1", nil, uuid); err != nil {
		t.Errorf("ListDeliveries(before=%q) = %v, want nil", uuid, err)
	}
}
