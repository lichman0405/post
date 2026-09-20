// Package credit is the credit attribution and credit dispute use case
// (T0809): declaring who a high-level credit belongs to, raising a
// dispute against a credit, and having governance decide it.
//
// # What it implements, in the words of the specs
//
// docs/13 §2 is the attribution half: "Asset/Finding/Release 可声明
// creators/major contributors/method designer 等高层 credit。它可以被纠正，
// 但 correction 作为新 event，旧 attribution 和 dispute history 保留。"
// docs/13 §3 is the dispute half: "Contributor 可发起 dispute，附带 ledger
// evidence；Maintainer/Organization governance 处理。不得直接改原事件。
// Dispute 本身不参与公共 reputation 排名直到 resolution."
// Migration 00103 is the storage (credit_attribution_statements and
// credit_attribution_parties, append-only by the 00014/00015 guards, plus
// the state machine over 00011's credit_disputes row); this package is the
// command, and internal/persistence/credit_store.go is the transaction.
//
// # The four properties the commands are built around
//
//  1. A CORRECTION APPENDS, and the ledger's original events are not
//     touched. docs/13 §2's correction is a new declaration at the next
//     ordinal; the declaration it corrects stays readable, and the
//     append-only ledger rows the credit was derived from
//     (contribution_events) are exactly the rows they were — this package
//     never writes them, 00014's trigger refuses a rewrite from any path,
//     and the integration suite snapshots them around a correction to
//     prove it. So "correctable" and "append-only" are the same statement
//     about two different records, not a contradiction.
//
//  2. A DISPUTE'S STATE IS UPDATED IN PLACE AND ITS HISTORY IS AN EVENT.
//     closing a dispute updates 00011's row (state, resolution,
//     resolved_at) — the design, not a hole: that table is on
//     tests/integration/append_only_test.go's "mutable by design" list —
//     and the same transaction records credit.dispute_opened /
//     credit.dispute_resolved (specs/events/event-types.yaml) through the
//     outbox, which the Contribution Ledger projects into the append-only
//     contribution_events. The history a reader can consult is therefore
//     the ledger, and the row is only the current state. "不得直接改原
//     event" is exactly this split.
//
//  3. THE AUTHORIZATION REUSES AN EXISTING MATRIX ROW — submit_scientific_review.
//     The matrix has no dispute row (specs/policies/permissions-matrix.csv
//     lists fifteen actions and none of them is this), internal/authz is
//     out of this task's scope, and adding an action would change the
//     permission model (L3, the owner's decision). So the surface is
//     carried by the one row whose cells fit all three sentences this
//     package must satisfy, resolved per operation (see
//     Command.authorize):
//
//     declare — docs/13 §2 names no declarer, so the command admits the
//     classes the row allows UNCONDITIONALLY (maintainer, owner). That is
//     the same set the existing creator-writing path admits: the creators
//     of an asset version are written by the publish command
//     (internal/application/assetpublish), which is gated on
//     publish_private_to_public — maintainer or owner. Fail closed on a
//     sentence the specs do not write.
//
//     open — "Contributor 可发起 dispute": the row's contributor cell is
//     `conditional`, and this site resolves that condition as "the actor
//     holds Contributor or above in this project". The same resolution
//     refuses the viewer and the authenticated non-member, whose cells in
//     this row are `conditional` too, which is what makes the condition
//     per-resource rather than a second flat matrix.
//
//     close — "Maintainer/Organization governance 处理": maintainer and
//     owner are the row's unconditional `allow` cells, and the conditional
//     cell resolves to false here, so a contributor cannot decide the
//     dispute they raised. Which LEVEL of maintainer may decide is not
//     written anywhere (the matrix has one maintainer class, not several);
//     nothing here invents sub-levels, and the boundary is reported in the
//     task RESULT.
//
//  4. AGENTS ARE REFUSED, TWICE OVER. docs/60 §2 lists credit dispute
//     resolution among the actions that MUST be human-governed, and the
//     row's agent cell (proposal_or_scoped) is not a pass — this site
//     resolves it as refusal, because a scoped dispute resolution is a
//     concept nothing defines. The domain backstop refuses an agent before
//     the matrix is consulted at all, so an edit to the CSV cannot waive
//     the product rule.
//
// The second fail-closed boundary, from the same undecided list: a person
// who is credited in a declaration but holds NO membership in the project
// cannot open a dispute about it. The condition above is resolved against a
// membership role, and a non-member has none — docs/13 §3 gives the right
// to contributors, and no sentence extends it to a non-member who is merely
// named. That boundary is reported in the RESULT rather than decided here.
//
// # What this package deliberately does NOT do
//
//   - It computes no credit. Nothing here derives who contributed from the
//     ledger, from a count or from any other fact: what is stored is what a
//     person declared, and who declared it (docs/13 §4 forbids a single
//     reputation score, §6 refuses raw counts as quality proxies, CLAUDE.md
//     §9 invariant 13 forbids a Truth Score). A dispute does not feed a
//     ranking either — §3 keeps it out of the public reputation until it is
//     resolved, and this package has no ranking to feed.
//   - It does not let a dispute edit the original events. The only rows the
//     commands write are the declaration chain, the dispute's current-state
//     row, the outbox event, and the audit row.
//   - It does not widen the credit vocabulary. The two roles 00103's CHECK
//     admits are the two this command accepts; "method_designer" and the
//     open tail of docs/13 §2's list (等) are refused until a product
//     decision ratifies them.
//
// # The surface
//
// Delivered without an HTTP route, for the reason T0711's rights-holder
// command gives: specs/api/openapi.yaml is the API contract (docs/22,
// OpenAPI-first) and is not in this task's writable scope, so a route added
// here would be an endpoint the contract does not describe. The transport
// is a follow-up (see the task RESULT); what is complete and tested here is
// the command, its authorization, its transaction, its event and its audit,
// driven in the integration suite exactly as cmd/api would drive it, over
// the real PostgreSQL, the real permission matrix and the real ledger
// projection.
package credit
