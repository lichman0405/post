package gitprovider

import (
	"context"
	"fmt"
)

// The platform layer's enforcement loop (T0302, docs/16 §3): the Gitea
// branch-protection rule is the first layer, and this sweep is the second.
// It re-verifies the canonical rule on every provisioned repository on a
// schedule (wired in cmd/api): a rule an operator removed or drifted in
// the provider UI is re-applied by the platform on the next pass, so the
// protected-main invariant is continuously enforced, not merely configured
// once at provisioning.
//
// Failures are per-repository and never fatal to the sweep: one wedged
// repository must not stop the healing of all the others, and a provider
// outage simply means the next pass retries (the same bounded-retry shape
// as the provisioning backlog).

// MainProtectionEnsurer is the provider port the sweeper needs (a slice
// of GitPort: the sweep only ever ensures the rule).
type MainProtectionEnsurer interface {
	EnsureMainProtection(ctx context.Context, repo Repository, spec MainProtectionSpec) (MainProtection, error)
}

// ProtectionSweeper re-verifies main protection on provisioned
// repositories.
type ProtectionSweeper struct {
	port  MainProtectionEnsurer
	store ProtectionStore
}

// ProtectionStore is the canonical-store port the sweeper needs.
type ProtectionStore interface {
	// ProvisionedRepositories lists every provisioned repository.
	ProvisionedRepositories(ctx context.Context) ([]Repository, error)
}

// NewProtectionSweeper wires the sweeper.
func NewProtectionSweeper(port MainProtectionEnsurer, store ProtectionStore) *ProtectionSweeper {
	return &ProtectionSweeper{port: port, store: store}
}

// Sweep runs one pass over the provisioned repositories. Every repository
// is ensured (converge is idempotent: a rule already canonical costs one
// read). Failures are returned per repository with the owner/name embedded
// and no secret-bearing text (the adapter errors are built redacted by
// construction); a store-level failure aborts the pass and is returned as
// err.
func (s *ProtectionSweeper) Sweep(ctx context.Context) (ensured int, failures []error, err error) {
	repos, err := s.store.ProvisionedRepositories(ctx)
	if err != nil {
		return 0, nil, err
	}
	for _, repo := range repos {
		if _, err := s.port.EnsureMainProtection(ctx, repo, MainProtectionSpec{}); err != nil {
			failures = append(failures, fmt.Errorf("%s/%s: %w", repo.Owner, repo.Name, err))
			continue
		}
		ensured++
	}
	return ensured, failures, nil
}
