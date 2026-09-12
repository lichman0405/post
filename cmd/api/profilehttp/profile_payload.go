package profilehttp

import (
	"time"

	"github.com/lichman0405/post/internal/domain"
)

// publicProfilePayload is the client-visible public profile shape
// (T0102): exactly the public fields — handle, display name, bio — plus
// the stable id and created_at. The private fields (email, disabled
// state, credentials) are structurally absent: this type has no email
// field, so no future handler edit can leak one by accident. The owner's
// private view is the auth session payload (/api/v1/auth/session), which
// carries email deliberately.
type publicProfilePayload struct {
	ID          string    `json:"id"`
	Handle      string    `json:"handle"`
	DisplayName string    `json:"display_name"`
	Bio         string    `json:"bio"`
	CreatedAt   time.Time `json:"created_at"`
}

func publicProfileFromDomain(p domain.Profile) publicProfilePayload {
	return publicProfilePayload{
		ID:          p.User.ID,
		Handle:      p.User.Handle,
		DisplayName: p.User.DisplayName,
		Bio:         p.Bio,
		CreatedAt:   p.User.CreatedAt,
	}
}
