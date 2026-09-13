// Package states is the Project State 与 State Commit application layer
// (task T0204): the transaction boundary that turns semantic writes into
// traceable RSG state transitions.
//
// The model (docs/07 §5, docs/09 §2): every semantic write — a scientific
// object version, a relation version, an evidence assertion, a blob
// attachment, a policy/schema reference — happens inside exactly one
// Commit. A commit records actor, via channel, message and the ordered
// operation summary, and produces a new ProjectState whose ParentStateID
// names the state it was built on; the branch's head pointer
// (branches.base_state_id) advances to the new state in the same
// transaction. The whole transition is one database transaction: either
// the state, the commit row and every member row the operations wrote are
// all visible together, or none of them are (acceptance: a failed
// transaction leaves no half state).
//
// Concurrency: the head advance is a compare-and-swap on the expected base
// state (CommitStateParams.BaseStateID). A commit built on a branch head
// that has moved underneath it fails with *StateConflictError
// (BRANCH_STATE_CONFLICT) and writes nothing; the caller re-reads the head
// and retries — the same expected_version discipline T0202/T0203 apply to
// version logs (docs/45).
//
// The package validates and orchestrates; it does not authorize — actors
// and membership checks belong to the consuming API task, which passes
// resolved identities in. The persistence adapter lives in
// internal/persistence (StateStore).
package states
