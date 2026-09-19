// Package aborts is the abort-proposal use case (T0602): the command
// behind
//
//	POST /api/v1/projects/{projectId}/objects/{objectId}:abort-proposal
//
// (specs/api/openapi.yaml:345-355, summary "Propose abort for object; main
// objects require PR flow"), and the MCP tool
// object.abort_proposal(project_id, object_version_ref, reason_code,
// explanation) of specs/mcp/tools.json:23.
//
// # What the specification fixes, verbatim
//
// docs/46_ABORT_RETENTION.md is the governing document and it fixes three
// things this package obeys rather than invents:
//
//   - what an abort records (:7): "Abort 必须记录 actor、time、reason code、
//     human explanation、replacement/superseding ref(optional)、
//     review/approval if main object." The five data fields are the
//     domain.AbortRecord the version row carries; the review/approval half
//     is structural rather than a field — a main-line abort reaches the
//     accepted state only through a Research PR merge, and the PR's own
//     review records are that approval.
//   - how a main object is aborted (:9): "main 中对象 Abort 必须 branch →
//     PR → merge；不能在详情页直接一键修改 current main." This command
//     therefore NEVER writes to main. It forks a proposal branch off
//     main's head, appends the aborted version there, and opens a Research
//     PR; the abort becomes effective when a human merges that PR through
//     the T0409 merge flow, which is the only thing that may advance main.
//   - that nothing is deleted (:3, :11): the abort APPENDS a new version.
//     The aborted version stays byte-for-byte what it was; the new row
//     carries the same payload and a moved lifecycle_state.
//
// # Authorization (the matrix, not a re-derivation)
//
// The row already exists in both spellings of the permission matrix and
// agrees with itself, so this package evaluates it rather than restating
// it: internal/authz/matrix.go:121-129 and
// specs/policies/permissions-matrix.csv:14 both say
// `abort_main_object,deny,deny,deny,deny,via_pr,via_pr,proposal_only`.
//
//   - anonymous / non-member / viewer / contributor: deny. Refused.
//   - maintainer / owner: via_pr. A conditional verdict is resolved at the
//     enforcement site, and this command IS the resolution: the route
//     creates the proposal and the PR, and it is the merge — a separate,
//     separately authorized step this command never takes — that makes the
//     abort effective. `ViaPR` is therefore satisfied by this route, and
//     it is satisfied only so far: nothing here writes main.
//   - agent: proposal_only. Resolved as a refusal. Two independent lines
//     say so. First, this command evaluates the matrix and treats every
//     verdict except `allow` and `via_pr` as a refusal, so
//     `proposal_only` is refused by the authorization step itself, not by
//     a later guard. Second, a domain backstop refuses every agent before
//     the engine is consulted (the
//     internal/application/contribution/service.go:173-175 shape), so an
//     agent cannot reach the proposal path even if a future matrix edit
//     widened the cell. The two lines agree; the backstop exists so that
//     agreement is not the only thing holding.
//
// The refusal is resolved BEFORE any object or project lookup, so it never
// discloses whether the target exists: an unknown object and a real object
// the caller may not touch answer exactly the same. The refusal is also
// recognizably an authorization outcome rather than a generic forbidden
// (ErrForbidden's own code, and the agent backstop's dedicated one).
//
// # Idempotency without a ledger
//
// The state itself is the idempotency record, as the task requires: no new
// table. The Idempotency-Key is stored on the version row the request
// appended (migration 00100's abort_request_key, unique per object), so a
// repeated request reads the row the first one wrote and returns it
// instead of appending a second. The version append is a compare-and-swap
// on the object's version counter (sciobjects.VersionParams), so of two
// concurrent requests with one key exactly one wins; the loser re-reads
// the key and returns the winner's result. Only the winner's transaction
// writes the audit row and the event, because both are written inside it
// (docs/53: the audit of a high-risk action is part of the action).
//
// # What this package does not do
//
// Reopen (docs/46:11) is T0610 and is not implemented here; the lifecycle
// states this command can produce are exactly one, `aborted`. Nothing in
// this package changes specs/, docs/ or the permission matrix.
package aborts
