// Package reopens is the reopen-proposal use case (T0610): the command that
// proposes reopening a main-line scientific object whose current version
// carries an abort, i.e. the reverse edge of T0602's abort command
// (internal/application/aborts).
//
// # What the specification fixes, verbatim
//
// Two sentences are the whole behavioural specification, and this package
// obeys them rather than inventing around them:
//
//   - docs/43_STATE_MACHINES.md:10: "active → aborted → reopened → active；
//     可 superseded（关系/状态事件）但不物理删除。" The reopened state is a
//     position in a cycle, and the edge INTO it starts at 'aborted'.
//   - docs/46_ABORT_RETENTION.md:11: "Reopen 创建新 transition，保留历史
//     abort。" A reopen APPENDS a transition; the abort it reverses stays in
//     the version log exactly as it was written. Nothing is deleted, and no
//     row is rewritten — the same append-only rule migration
//     00014_append_only_enforcement.sql:9 states and enforces.
//
// Two more rules are not reopen-specific but bind this command all the same:
//
//   - docs/09_VERSION_CONTROL.md:9-10: "即便 Owner 也只能经 PR merge" — main's
//     scientific state is advanced by a Research PR merge and by nothing else.
//     This command therefore NEVER writes to main: it forks a proposal branch
//     off main's head, appends the reopened version there, and opens a Research
//     PR. The reopen becomes effective when a human merges that PR through the
//     T0409 merge flow (internal/application/merge), which reads the reopen
//     record off the source version and carries it onto the accepted row.
//   - docs/26_OBSERVABILITY.md:25 lists abort/reopen together among the
//     highest-risk actions: the audit row is written in the same transaction as
//     the version row (docs/53), and the domain event
//     `scientific_object.reopened` — registered by
//     specs/events/event-types.yaml:23, a name this package spells but does not
//     coin — commits with them or not at all.
//
// # Authorization: the ruling's row, evaluated rather than restated
//
// The permission row did not exist when this task was written: the task book
// names it as "the only thing missing" and forbids the Worker from choosing a
// shape. The owner's 2026-09-21 ruling (recorded in this task's package as
// the supervisor ruling of that date) chose SHAPE A: a new row, cell for cell
// identical to abort_main_object —
//
//	reopen_main_object,deny,deny,deny,deny,via_pr,via_pr,proposal_only
//
// (specs/policies/permissions-matrix.csv:15, and internal/authz/action.go with
// internal/authz/matrix.go for the code's own copy). Shape B — reusing
// write_scientific_state — was rejected because it would let a contributor
// undo a maintainer's abort while being unable to abort at all: a loosening of
// the permission model, in the wrong direction.
//
// This package evaluates that row (authz.ActionReopenMainObject) rather than
// restating it, and resolves its conditional verdicts:
//
//   - anonymous / non-member / viewer / contributor: deny. Refused.
//   - maintainer / owner: via_pr. Satisfied by THIS route, which creates the
//     proposal and the PR and never writes main — docs/09:9-10's rule spelled
//     as a verdict, and the reason the cell is via_pr rather than allow.
//   - agent: proposal_only. Resolved as a refusal, on two independent lines.
//     This command treats every verdict except allow and via_pr as a refusal,
//     so the authorization step itself stops it; and a domain backstop refuses
//     every agent before the engine is consulted, so an agent stays out even if
//     a future matrix edit widened the cell. An agent therefore produces no
//     reopened version at all — not on main, and not on a proposal branch.
//
// The refusal is resolved BEFORE any object or project lookup, so it never
// discloses whether the target exists: an unknown object and a real object the
// caller may not touch answer exactly the same.
//
// # Idempotency: the state is the record
//
// A repeated request is answered by the version the first one appended, not by
// a second transition and not by a ledger table. The Idempotency-Key is stored
// on that version row (migration 00123's reopen_request_key, unique per
// object), so the replay read finds the recorded row and re-renders the first
// answer. The append is a compare-and-swap on the object's version counter, so
// of two concurrent requests carrying one key exactly one wins; the loser
// re-reads the key and replays the winner's result, writing no audit row and no
// event, because both are written inside the winner's transaction (docs/53).
//
// # The one place this package had to make a call, and its cost
//
// No sentence in docs/ or specs/ says what a reopen RECORDS. The governing
// sentences say only that it appends a transition and keeps the abort history.
// The task book's instruction for this gap is explicit — carry the abort's
// metadata shape ("沿用它的决定，不要另立一套") — so the reopen record is
// abortRecord's field set minus the replacement ref, which docs/46:7 gives to
// ABORT alone ("replacement/superseding ref(optional)") and never to a reopen:
// reason_code, explanation, decided_by, decided_at, stored in the version row's
// own columns (migration 00123) and never inside payload.
//
// What is invented here is therefore a REQUEST shape, not a rule: the command
// requires a reason code and a human explanation, because docs/26 puts reopen
// in the same highest-risk class as abort and an unexplained lifecycle
// restoration of somebody else's aborted work is not a decision anyone can
// review. The cost is named rather than hidden: a caller who has not read this
// doc must supply two fields the specification does not list, and the reason
// code is an open token in V1, so no report can break reopens down by reason.
//
// # What this package does not do
//
// It does not implement abort (T0602 owns it; this package reads the shape it
// established). It does not declare an HTTP route: the contract
// (specs/api/openapi.yaml) has exactly one object-level proposal path,
// `/projects/{projectId}/objects/{objectId}:abort-proposal`, and no reopen
// path at all, and this task's scope note forbids adding one to the contract
// while telling the Worker to report back rather than invent a path. This
// command is therefore mounted on no route, and — for the same reason — it is
// not constructed in cmd/api/main.go either: a command wired there with no
// transport reaching it would be a claim of reachability that is not true.
//
// What IS wired into cmd/api's production graph is the reader the merge needs
// (merge.Deps.Reopens): landing the transition on main without the record of
// who decided it would put a 'reopened' version on main that nobody can
// account for, and the merge's own stamp on that row is the merging actor's.
// The full command graph (members, authz, objects, branches, pull requests,
// commits, outbox recorder — all over the real pool) is what
// tests/integration/reopen_e2e_test.go assembles, so the command is exercised
// over production stores even though no binary mounts it yet.
//
// Nothing in this package changes specs/, docs/ or the permission matrix — the
// ruling's one CSV row is the single contract-facing write this task makes.
package reopens
