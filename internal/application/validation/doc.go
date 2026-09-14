// Package validation is the application layer of the progressive
// validation gates (T0207): it assembles gate snapshots from persistence
// and re-runs the gate engine (internal/rsg/validation) over them.
//
// Two consumers, two directions:
//
//   - Service.ValidateBranch serves the :validate endpoint (docs/22 §7):
//     the caller asks "would this branch pass gate X now?" and receives the
//     full report. It modifies no state — it is a read.
//
//   - Guard.RequireCommitGate is the server-side re-validation of a state
//     commit (docs/22 §7: "command 再次 server validate"): it runs the
//     commit's gate INSIDE the commit transaction, after the semantic
//     writes, over the rows as persisted — not over anything the caller
//     claims to have checked. A caller having validated is never a reason
//     for the server to skip validating; a blocked gate rolls the whole
//     transition back.
//
// The package depends on the states domain only through its own ports
// (SnapshotRepository, TxProbe): the states service imports this package
// for the Guard, so this package must never import states back.
package validation
