// Package prchecks is the application layer of the Pull Request integrity
// review (T0403): it assembles one PR's integrity snapshot from
// persistence and runs the deterministic engine (internal/rsg/integrity)
// over it.
//
// The service reads only. It resolves the PR's pinned states against the
// two branches' persisted chains (the PR row is the proposal's address;
// the chains are its content), gathers the base and proposed lineages'
// stored-form members via the manifest snapshot surface, resolves the
// policy universe the pins may reference, and hands the snapshot to the
// engine — which derives every check outcome and the verdict. The result
// is a machine result, never a human judgment: the blocking/warning
// severity per check is declared in the engine's spec table, and the
// human review decisions (docs/43: changes_requested/approved) are
// T0404's domain, informed by this report.
//
// Authorization is not this package's business: the consuming API task
// (cmd/api/pullrequestshttp) runs the project read gate before the
// service is reached, exactly like the validation endpoint.
package prchecks
