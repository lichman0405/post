package notifications

import (
	"context"
	"fmt"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// Service owns the account's email cadence (T1005). All policy lives here
// (docs/52: the application layer is the only layer that may decide): which
// cadences exist (events.ValidateCadence — the one vocabulary), that an
// account may only read and write ITS OWN setting, and that an unknown
// cadence is refused rather than stored.
//
// The complementary half — what the cadence is USED for — is not here. The
// sender reads the same row through the same store port and applies
// events.DigestDue; there is no second copy of "what daily means".
type Service struct {
	store Store
}

// NewService wires the service on the store port.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// Preferences returns the actor's email cadence. An account that has never
// set one gets the zero preferences, whose EffectiveCadence is
// events.DefaultCadence — the response shape is the same either way, so a
// client never has to treat "never set" as a special case.
func (s *Service) Preferences(ctx context.Context, actor domain.User) (events.NotificationPreferences, error) {
	prefs, err := s.store.GetPreferences(ctx, actor.ID)
	if err != nil {
		return events.NotificationPreferences{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return prefs, nil
}

// SetCadence records the actor's email cadence. The value is required and
// must be one of the three: an empty cadence is refused rather than
// defaulted, because "unset" and "set me back to the default" are the same
// outcome only by accident — a caller that meant one and wrote the other
// would never find out.
func (s *Service) SetCadence(ctx context.Context, actor domain.User, cadence string) (events.NotificationPreferences, error) {
	if err := events.ValidateCadence(cadence); err != nil {
		return events.NotificationPreferences{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	prefs, err := s.store.SetCadence(ctx, actor.ID, cadence)
	if err != nil {
		return events.NotificationPreferences{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return prefs, nil
}
