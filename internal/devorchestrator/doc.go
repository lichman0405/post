// Package devorchestrator implements the deterministic execution core of the
// POST development orchestrator `rddev`: the task DAG reader, the task state
// machine, and atomic state-file writes.
//
// It makes no product judgements. The task state machine enforces the
// transition table recorded in specs/orchestrator/task-state-machine.yaml
// (L1 decision, T0009): an illegal transition is rejected with a clear error
// and the state file is left unchanged — never coerced. Every state change
// carries a run_id and is appended to the task's history (append-only audit
// trail); writes are serialized by an exclusive lock and atomically renamed
// into place so concurrent writers cannot silently overwrite each other.
//
// The doctor preflight engine lives in the doctor subpackage.
package devorchestrator
