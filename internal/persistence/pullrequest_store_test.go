// T0814: the pull_requests INSERT runs into two database gates that raise
// the SAME SQLSTATE.
//
// infra/migrations/00042 adds pull_request_semantic_gate (the source branch's
// recorded semantic state is unstructured_changes: no formal PR may be opened
// from it, 409 to the caller) and 00086 adds pull_request_fork_gate (a
// cross-project source that is not the opener's own fork of the project:
// a permission outcome, answered as the existence-hiding not-found). Both
// RAISE ... USING ERRCODE = 'P0001', so the code alone cannot say which rule
// refused the row, and the two outcomes are not interchangeable: one tells
// the caller to fix their content, the other must not confirm that a foreign
// project's branch exists.
//
// mapPullRequestWriteError tells them apart by the RAISE text's opening
// sentence. That function is unexported and its decision is about driver
// error values, which is why it is pinned here rather than through the
// integration suite: driving it from the database would need a real
// PostgreSQL per case and still could not produce the third case below (a
// P0001 from a rule this code has never heard of).
//
// What the cases below settle:
//
//   - the semantic gate's refusal becomes the caller's outcome AND keeps the
//     driver error in the chain (errors.As still finds the *pgconn.PgError,
//     whose Message names the branch and the semantic state). Two %w verbs,
//     not one: dropping the cause would answer "unstructured changes" to a
//     caller with no way to see which rule said so.
//   - the fork gate's refusal becomes the not-found outcome.
//   - an UNPLACEABLE P0001 — a third trigger, or a rewording that leaves the
//     two prefixes stale — stays a store failure. That is the fail-closed
//     direction the transport turns into 503 (SERVICE_UNAVAILABLE): an
//     unplaceable refusal is never reported as a rule the caller can act on.
//     Without this case the two above would pass on a mapping that guessed.
package persistence

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/application/pullrequests"
)

// pgRaise is the driver error PostgreSQL hands back for one RAISE: the
// SQLSTATE in Code and the RAISE statement's text in Message, exactly as
// pgx builds it.
func pgRaise(code, message string) error {
	return &pgconn.PgError{Code: code, Message: message, Severity: "ERROR"}
}

// The two migrations' RAISE texts, as the database reports them (the text
// after "ERROR: "). Their opening sentences are the discriminators; the rest
// is the row-specific detail the mapping must NOT parse.
const (
	semanticRaiseText = "pull request cannot be opened from branch 6a1c5f1e-0000-4000-8000-000000000001: " +
		"its semantic state is unstructured_changes — the pushed content carries changes the platform " +
		"cannot parse, and docs/16 §4 forbids a formal PR until the required scientific semantics are filled"
	forkRaiseText = "pull request on project 6a1c5f1e-0000-4000-8000-000000000002 cannot take its " +
		"source branch from project 6a1c5f1e-0000-4000-8000-000000000003: a cross-project pull request " +
		"must come from the opener's own fork of this project (docs/04 §2, open_pr = allow_from_fork)"
)

// TestMapPullRequestWriteErrorTellsTheTwoGatesApart is the discrimination
// itself, both directions, plus the fail-closed third case.
func TestMapPullRequestWriteErrorTellsTheTwoGatesApart(t *testing.T) {
	t.Run("the semantic gate becomes the unstructured-changes outcome", func(t *testing.T) {
		err := mapPullRequestWriteError(pgRaise("P0001", semanticRaiseText))
		if !errors.Is(err, pullrequests.ErrBranchUnstructuredChanges) {
			t.Fatalf("mapped to %v, want ErrBranchUnstructuredChanges", err)
		}
		if errors.Is(err, pullrequests.ErrBranchNotFound) {
			t.Fatalf("the semantic gate's refusal was also reported as a missing branch: %v", err)
		}
		// The driver error survives in the chain: it names the branch and the
		// semantic state the trigger refused on.
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Message != semanticRaiseText {
			t.Fatalf("mapped error = %v, want the database's own refusal still in the chain", err)
		}
	})

	t.Run("the fork gate becomes the not-found outcome", func(t *testing.T) {
		err := mapPullRequestWriteError(pgRaise("P0001", forkRaiseText))
		if !errors.Is(err, pullrequests.ErrBranchNotFound) {
			t.Fatalf("mapped to %v, want ErrBranchNotFound (a foreign source is never confirmed to exist)", err)
		}
		if errors.Is(err, pullrequests.ErrBranchUnstructuredChanges) {
			t.Fatalf("the fork gate's refusal was reported as the caller's own content problem: %v", err)
		}
	})

	t.Run("an unplaceable P0001 stays a store failure", func(t *testing.T) {
		// A third trigger, or either migration reworded: the mapping must not
		// guess. Neither outcome may be claimed, so the caller is answered
		// "the service is unavailable" rather than a rule it could act on.
		for _, message := range []string{
			"some later trigger refused this row",
			// The fork gate's rule without its opening sentence: the mapping
			// keys on the text, so a reworded trigger falls here rather than
			// being folded into the not-found outcome.
			"a cross-project pull request must come from the opener's own fork of this project",
			"",
		} {
			err := mapPullRequestWriteError(pgRaise("P0001", message))
			if errors.Is(err, pullrequests.ErrBranchUnstructuredChanges) {
				t.Errorf("message %q was placed at the semantic gate, which it does not name", message)
			}
			if errors.Is(err, pullrequests.ErrBranchNotFound) {
				t.Errorf("message %q was placed at the fork gate, which it does not name", message)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) {
				t.Errorf("message %q: the store failure lost the database's refusal: %v", message, err)
			}
		}
	})

	t.Run("other SQLSTATEs are untouched", func(t *testing.T) {
		// The mapping must not turn every database error into a domain
		// outcome: a serialization failure is still an adapter failure.
		err := mapPullRequestWriteError(fmt.Errorf("persistence: insert: %w",
			pgRaise("40001", "could not serialize access due to concurrent update")))
		if errors.Is(err, pullrequests.ErrBranchUnstructuredChanges) || errors.Is(err, pullrequests.ErrBranchNotFound) {
			t.Fatalf("a 40001 became a domain outcome: %v", err)
		}
	})
}
