// Package contract is the T1205 instrument set: it enumerates the HTTP routes
// the tree actually registers, reads the API contract (specs/api/openapi.yaml)
// and the exemption list (specs/api/openapi-exemptions.yaml), and decides
// mechanically whether every mounted /api/v1 route is either declared in the
// contract or exempted with a citation that resolves to real text.
//
// THE SHAPE OF THIS TASK, because it is easy to get backwards:
//
//   - The INSTRUMENT is green. Everything in this package is exercised by
//     tests/contract/*_test.go against synthetic fixtures under testdata/, so
//     `go test ./...` — which is CI's `go` job and every task's G2 — stays
//     green.
//   - The READING of the real tree is red, on purpose, and lives in a separate
//     command (tests/cmd/contractgate) that NO go test package calls. At the
//     time T1205 merged, the exemption list was empty and the contract did not
//     cover every registered route, so the comparator reports undocumented
//     routes and exits non-zero. That is the true state of the tree, not a
//     bug: closing it (writing the contract, filling the exemption list) is
//     the Supervisor's action, per specs/api/openapi-exemptions.yaml's header
//     and CLAUDE.md §8.1 (specs/** is Supervisor-only).
//
// Why the split is not "hiding a red": a _test.go asserting "zero undocumented
// routes" over the real tree would run in every task's G2 (gates.json wires G2
// to CI's exact `go` job steps), turning one known-open, Supervisor-owned gap
// into a red gate on four unrelated in-flight workers and blocking T1205's own
// merge — the driver only merges on a green CI. The red must be visible to the
// one reader who can close it, and to nobody else's gate.
package contract
