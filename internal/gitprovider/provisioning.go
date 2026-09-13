package gitprovider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The provisioning application service (T0301): one project → one provider
// repository → one push webhook with a fresh HMAC secret. All policy lives
// here; the provider calls go through GitPort and every canonical-store
// fact through ProvisionStore.

// ProvisionJobType is the Redis job type the API enqueues on project
// creation (consumed by the loop wired in cmd/api/main.go).
const ProvisionJobType = "project-provision"

// ProvisionJobPayload is the payload of a project-provision job. It
// carries identity only (docs/52 §17): the project id — the repository
// name, owner and secret are all derived server-side.
type ProvisionJobPayload struct {
	ProjectID string `json:"project_id"`
}

// repoNamePrefix marks platform-provisioned repositories in the service
// account's namespace, so a human browsing the (internal) provider UI can
// tell them apart from anything else under that account.
const repoNamePrefix = "p-"

// projectIDRe is the canonical project id shape (lowercase uuid). The id
// becomes the provider repository name, so it must be charset-validated
// before it leaves the platform: the canonical store produces only uuids,
// this check makes that assumption fail loudly instead of smuggling an
// invalid name into the provider API.
var projectIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// webhookSecretBytes is the HMAC secret length (256 bits of entropy).
const webhookSecretBytes = 32

// ProvisionStore is the canonical-store port the provisioner needs. The
// concrete adapter is *PGProvisionStore in this package.
type ProvisionStore interface {
	// ProvisioningBacklog lists every project awaiting provisioning: the
	// work list derived from provision_status IN ('pending', 'failed') —
	// failed rows stay in the backlog, so a later sweep or a redelivered
	// job re-attempts them (the bounded retry policy).
	ProvisioningBacklog(ctx context.Context) ([]PendingProject, error)
	// Provision serializes one provisioning attempt per project: the row
	// is locked, already-provisioned rows are skipped, fn does the
	// provider work, and its outcome is committed atomically with the
	// status transition (see *PGProvisionStore.Provision).
	Provision(ctx context.Context, projectID string, fn func(PendingProject) (*ProvisionRecord, error)) (provisioned, skipped bool, err error)
}

// Provisioner provisions repositories for new projects.
type Provisioner struct {
	port       GitPort
	store      ProvisionStore
	webhookURL string
}

// NewProvisioner wires the provisioner. webhookURL is the delivery target
// registered on every push webhook (validated config,
// POST_GITEA_WEBHOOK_URL).
func NewProvisioner(port GitPort, store ProvisionStore, webhookURL string) *Provisioner {
	return &Provisioner{port: port, store: store, webhookURL: webhookURL}
}

// Provision provisions the repository and push webhook of one project.
// Idempotent: an already-provisioned project is skipped, and every step on
// the provider side is itself idempotent (EnsureRepository / EnsureWebhook),
// so job redelivery never creates duplicates. A failed project stays in
// the store's backlog — the boot sweep (or a redelivered job) re-attempts
// it.
func (p *Provisioner) Provision(ctx context.Context, projectID string) error {
	_, _, err := p.store.Provision(ctx, projectID, func(proj PendingProject) (*ProvisionRecord, error) {
		return p.provision(ctx, proj)
	})
	return err
}

// provision performs the provider work for one project while the store
// holds the project-row lock. It returns the record to commit; an error
// aborts and the store records the project as failed.
func (p *Provisioner) provision(ctx context.Context, proj PendingProject) (*ProvisionRecord, error) {
	if !projectIDRe.MatchString(proj.ID) {
		return nil, fmt.Errorf("%w: project id %q cannot become a repository name",
			ErrConflict, proj.ID)
	}
	repo, err := p.port.EnsureRepository(ctx, RepositorySpec{
		Name:        RepositoryName(proj.ID),
		Description: repoDescription(proj),
		Private:     true,
	})
	if err != nil {
		return nil, err
	}
	secret, err := NewWebhookSecret()
	if err != nil {
		return nil, err
	}
	hook, err := p.port.EnsureWebhook(ctx, WebhookSpec{
		Repository: repo,
		URL:        p.webhookURL,
		Secret:     secret,
		Events:     []string{"push"},
	})
	if err != nil {
		return nil, err
	}
	return &ProvisionRecord{
		ProjectID:     proj.ID,
		Owner:         repo.Owner,
		Name:          repo.Name,
		GiteaRepoID:   repo.ID,
		WebhookID:     hook.ID,
		WebhookSecret: secret,
	}, nil
}

// RepositoryName derives the provider-side repository name from the
// project id: "p-" + uuid. Deterministic (the same project always maps to
// the same repository), globally unique (uuids are), and length-safe —
// which is the project→repo 1:1 mapping made mechanical. The slug cannot
// serve: it is only unique inside an organization, and two organizations
// may both have a "battery-lab".
func RepositoryName(projectID string) string { return repoNamePrefix + projectID }

// repoDescription is the operator-facing description on the provider side
// (the provider UI is not a product surface, ADR-019; this exists for
// debugging). Bounded to the provider's description length.
func repoDescription(proj PendingProject) string {
	return trimmedDescription(fmt.Sprintf("POST project %s (%s)", proj.Slug, proj.Name), 500)
}

// NewWebhookSecret generates a fresh webhook HMAC secret: 32 random bytes,
// hex-encoded. Every provisioned repository gets its own, so one
// compromised secret never signs for another repository (T0305 verifies
// deliveries against the per-project stored value).
func NewWebhookSecret() (string, error) {
	var b [webhookSecretBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("gitprovider: entropy source failed")
	}
	return hex.EncodeToString(b[:]), nil
}

// trimmedDescription truncates on rune boundaries (defensive: Go strings
// are bytes; slicing mid-rune would store invalid UTF-8).
func trimmedDescription(s string, max int) string {
	if len(s) <= max {
		return s
	}
	r := []rune(s)
	if len(r) > max {
		s = string(r[:max])
	}
	return strings.TrimSpace(s)
}
