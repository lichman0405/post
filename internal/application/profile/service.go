package profile

import (
	"context"
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/domain"
)

// Service orchestrates the profile use cases. The public field set is a
// transport concern (cmd/api/profilehttp renders it), but WHO may change
// WHICH fields is application policy (docs/52): only the account owner may
// update their own handle, display name and bio — never anyone else's, and
// never the private identity fields (email etc., which Update does not
// carry).
type Service struct {
	store ProfileStore
}

// NewService wires the service.
func NewService(store ProfileStore) *Service {
	return &Service{store: store}
}

// Get returns the profile for userID. Reading is public by design
// (acceptance "未登录可读公开 profile"): no authorization here — the
// transport renders exactly the public payload shape for every caller,
// signed in or not.
func (s *Service) Get(ctx context.Context, userID string) (domain.Profile, error) {
	p, err := s.store.GetByUserID(ctx, userID)
	if err != nil {
		return domain.Profile{}, wrapStoreError(err)
	}
	return p, nil
}

// GetByHandle resolves a profile by its current handle (normalized here —
// handles are case-insensitive identity strings, domain.NormalizeHandle).
// An unknown handle is ErrNotFound; the caller renders 404.
func (s *Service) GetByHandle(ctx context.Context, handle string) (domain.Profile, error) {
	p, err := s.store.GetByHandle(ctx, domain.NormalizeHandle(handle))
	if err != nil {
		return domain.Profile{}, wrapStoreError(err)
	}
	return p, nil
}

// Update applies the owner's patch to the editable fields. actorID must
// equal targetID — the transport resolves actorID from the authenticated
// session, and the service enforces the rule itself as defense in depth:
// even a future call path that forgot the guard cannot edit someone
// else's profile.
func (s *Service) Update(ctx context.Context, actorID, targetID string, upd Update) (domain.Profile, error) {
	if actorID == "" || actorID != targetID {
		return domain.Profile{}, ErrForbidden
	}
	clean, err := validateUpdate(upd)
	if err != nil {
		return domain.Profile{}, err
	}
	p, err := s.store.Update(ctx, targetID, clean)
	if err != nil {
		return domain.Profile{}, wrapStoreError(err)
	}
	return p, nil
}

// validateUpdate normalizes and checks the editable fields. Messages are
// client-safe fixed strings (docs/22 §5: no dependency detail on the
// wire); the handler renders them verbatim for VALIDATION_FAILED.
func validateUpdate(upd Update) (Update, error) {
	if upd.Handle != nil {
		h := domain.NormalizeHandle(*upd.Handle)
		if !domain.ValidHandle(h) {
			return Update{}, fmt.Errorf("%w: handle may only contain letters, digits, - and _ (max 64 characters)", ErrValidation)
		}
		upd.Handle = &h
	}
	if upd.DisplayName != nil && !domain.ValidDisplayName(*upd.DisplayName) {
		return Update{}, fmt.Errorf("%w: display name must be 1-200 characters and contain visible text", ErrValidation)
	}
	if upd.Bio != nil && !domain.ValidBio(*upd.Bio) {
		return Update{}, fmt.Errorf("%w: bio must be at most %d bytes", ErrValidation, domain.MaxBioLen)
	}
	return upd, nil
}

// wrapStoreError keeps the expected domain outcomes (unknown profile,
// taken handle) and turns everything else — including a persistence
// adapter that cannot run (e.g. the profiles migration not yet applied) —
// into ErrStore for the handler, with the cause kept for the log.
func wrapStoreError(err error) error {
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrHandleTaken) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
