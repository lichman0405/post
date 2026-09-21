package attestationhttp

import (
	"context"
	"net/http"

	"github.com/lichman0405/post/internal/application/attestations"
)

// The attestation surface's wiring: the command and the public read go in;
// three routes come out (the session/CSRF guard is owned by authhttp and
// composed in cmd/api/main.go, like every other product route).

// AttestCommand is the attestation use case this surface drives (the
// production value is *attestations.Command).
type AttestCommand interface {
	// Preview answers what an attestation would publish and what it would
	// keep private, writing nothing.
	Preview(ctx context.Context, actor attestations.Actor, in attestations.PublishParams) (attestations.Preview, error)
	// Publish performs the governance write.
	Publish(ctx context.Context, actor attestations.Actor, in attestations.PublishParams) (attestations.Attested, error)
}

// Read resolves one attestation by its pid. The production value is
// *persistence.AttestationStore, and it returns the value ALREADY PROJECTED
// by attestations.Present — the transport applies no rule to it, because
// there is none left to apply: what a reader may be shown was decided where
// the row was read, and the row it was read from has no private column.
type Read interface {
	GetPublicAttestation(ctx context.Context, pid string) (attestations.PublicAttestation, bool, error)
}

// Deps carries the adapters the surface needs.
type Deps struct {
	// Attest is the attestation command (T0812).
	Attest AttestCommand
	// Read is the public read of one attestation.
	Read Read
}

// New wires the handlers.
func New(deps Deps) *API {
	return &API{handlers: &handlers{attest: deps.Attest, read: deps.Read}}
}

// API is the mounted attestation surface.
type API struct {
	handlers *handlers
}

type handlers struct {
	attest AttestCommand
	read   Read
}

// Register mounts the three routes on the v1 mux.
//
// The two project-nested paths are more specific than the /api/v1/projects/
// subtree, so they coexist with it (the same technique assetshttp,
// releasehttp and knowledgehttp use). They are registered verbatim as the
// shape the contract gave the publication pair — see doc.go for why that
// shape is an L1 decision here rather than a copied contract path.
//
// The read is NOT project-nested and carries no audience gate: an attestation
// is public by construction, so an anonymous caller reaching it is the
// intended case (`security: []` in every other surface's spelling).
func (a *API) Register(v1 *http.ServeMux) {
	v1.HandleFunc("POST /api/v1/projects/{projectId}/attestations:publish-preview", a.handlers.handlePreview)
	v1.HandleFunc("POST /api/v1/projects/{projectId}/attestations:publish", a.handlers.handlePublish)
	v1.HandleFunc("GET /api/v1/attestations/{attestationId}", a.handlers.handleAttestation)
}
