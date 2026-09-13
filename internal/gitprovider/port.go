package gitprovider

import (
	"context"
	"errors"
)

// The GitPort contract (T0301): the port over the internal Git transport
// (ADR-003, ADR-019). Product domain objects never use provider-side
// identity as primary identity — the canonical mapping lives in
// projects.git_repository_external_id and git_repository_provisions, and
// the domain layer never touches the provider's own database (acceptance
// criterion: domain 不引用 Gitea DB).

// RepositorySpec describes the repository to provision (or find) on the
// provider side. The caller derives Name deterministically from the project
// (RepositoryName), which is what makes the project→repo mapping 1:1:
// re-provisioning the same project always resolves to the same repository.
type RepositorySpec struct {
	// Name is the provider-side repository name. The repository is created
	// in the adapter's own service-account namespace — the adapter resolves
	// the owner from its token, callers never invent one.
	Name string
	// Description is free-text metadata for operator debugging (the
	// provider UI is never a product surface, ADR-019).
	Description string
	// Private is always true in V1: repositories are private at the Git
	// layer regardless of project visibility. Project visibility is a
	// platform concept; Git-layer read access is granted through per-user
	// tokens (T0304), never through Git-layer publicity.
	Private bool
}

// Repository is the provider-side identity of one repository.
type Repository struct {
	// Owner is the provider namespace (the service account login).
	Owner string
	Name  string
	// ID is the provider's numeric repository id (the webhook payload's
	// repository.id — T0305 maps deliveries through it).
	ID int64
	// CloneURL is the provider-side clone URL (may embed credentials; the
	// product surfaces it via its own scoped-token URLs, T0304).
	CloneURL string
	// Private is the provider-side visibility at the moment the repository
	// was read. EnsureRepository repairs it on adopt: a pre-existing
	// repository must never stay public.
	Private bool
}

// WebhookSpec describes the push webhook to ensure on a repository.
type WebhookSpec struct {
	Repository Repository
	// URL is the delivery target — the platform API endpoint the provider
	// must be able to reach (operator-configured, POST_GITEA_WEBHOOK_URL).
	URL string
	// Secret is the HMAC secret the provider signs deliveries with. The
	// adapter sends it to the provider; it is write-only there and never
	// read back.
	Secret string
	// Events is the delivery event set (V1: ["push"]).
	Events []string
}

// Webhook is the provider-side identity of one registered webhook.
type Webhook struct {
	ID int64
	// Active reports the delivery state the adapter left it in.
	Active bool
}

// GitPort is the port over the internal Git transport: repository
// provisioning and webhook registration. Future Git-layer tasks extend it
// (branch protection T0302, ref creation T0303, user tokens T0304) — it is
// the ADR-003 exit hatch, so nothing outside internal/gitprovider talks to
// the provider directly.
type GitPort interface {
	// EnsureRepository provisions the repository described by spec, or
	// returns the existing one. Idempotent: provisioning the same spec
	// twice is not an error, and the caller's deterministic naming makes
	// "the same spec" mean "the same repository" — that is the 1:1
	// project→repo invariant. It fails with ErrUnauthorized when the
	// adapter's credentials are rejected, ErrConflict when the spec cannot
	// be satisfied (name invalid), and ErrUnavailable when the provider
	// cannot answer.
	EnsureRepository(ctx context.Context, spec RepositorySpec) (Repository, error)
	// GetRepository reads one repository. ErrNotFound when it does not
	// exist.
	GetRepository(ctx context.Context, owner, name string) (Repository, error)
	// EnsureWebhook makes sure the repository has one active webhook
	// delivering spec.Events to spec.URL signed with spec.Secret: an
	// existing webhook for the same URL is updated in place (secret
	// included — re-provisioning with a fresh secret rotates the secret
	// instead of piling up duplicate hooks), otherwise a new one is
	// created. Fails with ErrNotFound when the repository does not exist.
	EnsureWebhook(ctx context.Context, spec WebhookSpec) (Webhook, error)
}

// Sentinel errors every GitPort implementation maps provider failures onto.
// Callers decide per error: retriable (ErrUnavailable), fixable by the
// operator (ErrUnauthorized), or permanent (ErrNotFound, ErrConflict).
var (
	// ErrNotFound: the repository does not exist.
	ErrNotFound = errors.New("gitprovider: not found")
	// ErrConflict: the request cannot be satisfied as specified (e.g. an
	// invalid name, or a repository owned by someone else).
	ErrConflict = errors.New("gitprovider: conflict")
	// ErrUnauthorized: the adapter's credentials were rejected.
	ErrUnauthorized = errors.New("gitprovider: unauthorized")
	// ErrUnavailable: the provider could not answer (network, 5xx).
	ErrUnavailable = errors.New("gitprovider: provider unavailable")
)
