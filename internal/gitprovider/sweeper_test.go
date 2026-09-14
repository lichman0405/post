package gitprovider_test

import (
	"context"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/gitprovider"
)

// fakeProtectionStore scripts the sweeper's canonical-store port.
type fakeProtectionStore struct {
	repos []gitprovider.Repository
	err   error
}

func (f *fakeProtectionStore) ProvisionedRepositories(context.Context) ([]gitprovider.Repository, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.repos, nil
}

// fakeProtectionPort scripts the sweeper's provider port.
type fakeProtectionPort struct {
	err      error
	ensured  []gitprovider.Repository
	failRepo gitprovider.Repository // when set, this repository fails
}

func (f *fakeProtectionPort) EnsureMainProtection(_ context.Context, repo gitprovider.Repository, _ gitprovider.MainProtectionSpec) (gitprovider.MainProtection, error) {
	f.ensured = append(f.ensured, repo)
	if f.failRepo.Owner == repo.Owner && f.failRepo.Name == repo.Name {
		return gitprovider.MainProtection{}, f.err
	}
	return gitprovider.MainProtection{}, nil
}

func (f *fakeProtectionPort) GetMainProtection(context.Context, gitprovider.Repository) (gitprovider.MainProtection, error) {
	return gitprovider.MainProtection{}, gitprovider.ErrNotFound
}

// TestProtectionSweeperEnsuresEveryRepository: one pass visits every
// provisioned repository exactly once.
func TestProtectionSweeperEnsuresEveryRepository(t *testing.T) {
	repos := []gitprovider.Repository{
		{Owner: "svc", Name: "p-a"},
		{Owner: "svc", Name: "p-b"},
	}
	port := &fakeProtectionPort{}
	sweeper := gitprovider.NewProtectionSweeper(port, &fakeProtectionStore{repos: repos})

	ensured, failures, err := sweeper.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if ensured != 2 || len(failures) != 0 {
		t.Errorf("Sweep = (%d ensured, %d failures), want (2, 0)", ensured, len(failures))
	}
	if len(port.ensured) != 2 {
		t.Errorf("ensured repos = %v, want both", port.ensured)
	}
}

// TestProtectionSweeperFailureIsPerRepository: one wedged repository is
// reported and the rest are still ensured.
func TestProtectionSweeperFailureIsPerRepository(t *testing.T) {
	repos := []gitprovider.Repository{
		{Owner: "svc", Name: "p-a"},
		{Owner: "svc", Name: "p-b"},
	}
	port := &fakeProtectionPort{
		err:      errors.New("provider down"),
		failRepo: gitprovider.Repository{Owner: "svc", Name: "p-a"},
	}
	sweeper := gitprovider.NewProtectionSweeper(port, &fakeProtectionStore{repos: repos})

	ensured, failures, err := sweeper.Sweep(t.Context())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if ensured != 1 {
		t.Errorf("ensured = %d, want 1 (the healthy repository)", ensured)
	}
	if len(failures) != 1 {
		t.Fatalf("failures = %d, want 1", len(failures))
	}
	if got := failures[0].Error(); got != "svc/p-a: provider down" {
		t.Errorf("failure = %q, want the repository named", got)
	}
}

// TestProtectionSweeperStoreFailureAborts: a store-level failure aborts
// the pass and is returned as err (the next tick retries).
func TestProtectionSweeperStoreFailureAborts(t *testing.T) {
	storeErr := errors.New("store unreachable")
	sweeper := gitprovider.NewProtectionSweeper(&fakeProtectionPort{}, &fakeProtectionStore{err: storeErr})

	_, _, err := sweeper.Sweep(t.Context())
	if !errors.Is(err, storeErr) {
		t.Errorf("Sweep error = %v, want the store error", err)
	}
}
