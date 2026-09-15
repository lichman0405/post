// Package prdiff is the pull-request-facing Research State Diff read
// (T0408): resolve one PR's three pinned states — the PR's fixed base,
// its proposed head and the target branch's current head — and compute
// the three-way semantic diff of internal/rsg/diff over them.
//
// It is a read assembly, nothing more: the change list, the summary
// categories and the recorded file-level git refs are the diff engine's
// (the canonical document of docs/06 §6's PR first screen); this package
// owns the resolution of WHICH three states a PR's diff compares and the
// mapping of every failure to a stable outcome. It performs no
// authorization — the consuming API route runs the project read gate
// before calling it, exactly as the PR list and checks routes do (docs/45:
// a denied read answers the same existence-hiding 404).
//
// The base is deliberately the PR's OWN pinned BaseStateID, never the
// target branch's current head: the base does not drift as the target
// advances (docs/09 §4, the acceptance the PR domain is built on). The
// target's current head rides along as the THREE-WAY third state, so the
// proposal is shown against concurrent target movement without the base
// moving under it — re-evaluating the proposal against the target's head
// is the merge flow's job (T0406), this read only reports.
package prdiff
