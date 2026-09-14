package prchecks

import (
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/application/pullrequests"
)

// Sentinel errors the service maps for the transport layer (docs/45: the
// wire carries codes, never dependency detail). The not-found outcome
// mirrors the pullrequests package's sentinel — one outcome, one error
// identity, whichever package reports it (the handler answers the same
// wire code the pull requests endpoints use).
var (
	// ErrPullRequestNotFound: no PR row exists for the (project, number)
	// pair — an unknown number, or a PR of another project (same outcome,
	// no foreign existence leak, docs/45).
	ErrPullRequestNotFound = errors.New("prchecks: pull request not found")
	// ErrValidation: an input fails the domain shape rules (empty
	// project id, non-positive number).
	ErrValidation = errors.New("prchecks: validation failed")
	// ErrStore: the persistence adapter failed (cause kept for the log).
	ErrStore = errors.New("prchecks: store failure")
)

// WrapStoreError keeps the expected domain outcomes and turns everything
// else into ErrStore with the cause kept for the log.
func WrapStoreError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pullrequests.ErrPullRequestNotFound) {
		return ErrPullRequestNotFound
	}
	if errors.Is(err, ErrValidation) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
