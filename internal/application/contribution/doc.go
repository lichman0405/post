// Package contribution is the Contribution Opportunity use case (T0803):
// a maintainer marks an issue or a research need as open for contribution
// with difficulty/capability metadata, and publicizes it onto the open
// network through an explicit, audited action.
//
// Three requirements shape the surface:
//
//   - "maintainer 将 issue/research need 标 open for contribution":
//     MarkOpen creates an open opportunity on an issue or a
//     research_question target; the store snapshots the title from the
//     target inside the creation transaction.
//   - "difficulty/capability metadata": every opportunity carries a
//     difficulty level (beginner/intermediate/advanced) and bounded
//     capability tags, editable while the row is internal.
//   - "agent suggestions require approval": an agent's only path into
//     the model is Suggest, which creates a suggested row — it becomes a
//     real opportunity only through a human maintainer's Approve. Mark,
//     Approve and Publicize refuse agent actors outright
//     (*AgentNotPermittedError), so an agent can neither mark directly
//     nor approve its own suggestion.
//
// The acceptance criterion is structural, not just a check: "Agent 不能
// 自动 publicize opportunity" holds at three independent layers — the
// service refuses agent actors on Publicize; the store is the only path
// that sets the session flag migration 00062's guard requires, so no
// other write path can make a row public; and the publicize is always
// audited (docs/12 §3: visibility widening requires an authorized
// person's explicit confirmation and produces an audit record, never
// automatic).
//
// Like the other application services, this service owns input
// validation and the state machine (the port trusts, the service
// verifies); it does NOT authorize project membership/roles — those
// checks belong to the consuming API task, which passes resolved
// identities in (including the actor's agent flag, resolved from
// verified session facts like authz.ClassOf, never from client
// strings). The consuming API task must also map Publicize onto
// authz.ActionPublishPrivateToPublic (agent column: deny) when the
// opportunity HTTP/MCP surface lands — the agent refusal here is the
// domain-level backstop, not a replacement for the permission matrix.
package contribution
