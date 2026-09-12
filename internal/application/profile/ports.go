package profile

import (
	"context"

	"github.com/lichman0405/post/internal/domain"
)

// ProfileStore is the profile port. Implementations read the identity +
// profile rows (users + profiles in PostgreSQL; one in-memory map in
// memstore) — the application never sees which (docs/52: application
// orchestrates against ports, adapters live in internal/persistence).
type ProfileStore interface {
	// GetByUserID returns the profile for the identity id, or ErrNotFound.
	GetByUserID(ctx context.Context, userID string) (domain.Profile, error)
	// GetByHandle returns the profile for a handle (callers pass the
	// normalized form), or ErrNotFound.
	GetByHandle(ctx context.Context, handle string) (domain.Profile, error)
	// Update applies the non-nil fields of upd and returns the fresh
	// profile. It fails with ErrNotFound for an unknown id and
	// ErrHandleTaken when the new handle is another account's.
	Update(ctx context.Context, userID string, upd Update) (domain.Profile, error)
}

// Update is the editable-field patch. A nil pointer means "leave
// unchanged"; a non-nil empty Bio clears it. Email is deliberately absent:
// it is an identity fact, not an editable profile field in V1 — changing
// it needs verification flows, which belong to a later identity task, not
// to the profile surface.
type Update struct {
	Handle      *string
	DisplayName *string
	Bio         *string
}
