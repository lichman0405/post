// Package researchcontext owns the flow docs/14:25 specifies in one
// sentence: "Start Research Project 创建 Draft Research Context：Research
// Question、referenced knowledge、dependencies、candidate materials、known
// uncertainties、agent-suggested hypotheses。用户确认后才形成 initial
// state。"
//
// Two commands, two halves of one idea, and the split between them is the
// domain rule this package exists to hold:
//
//   - Start turns ONE answered search into a Draft Research Context. It
//     creates a planning project and the draft row, and it writes NO research
//     state: no branch, no state, no object, no commit. docs/03: DRAFT is not
//     yet an RSG, and docs/07:5 makes the RSG the project's full state graph
//     AT a state version — a project with a draft and no confirmation has no
//     version to be at.
//   - Confirm is the only call in the platform that forms that initial state,
//     and it does it through the ordinary RSG write path (the same
//     rsg.Service.CreateBranch + CreateObject every other write uses), so the
//     transition is named by a state commit like any other (docs/09 §2).
//
// # Why "an agent answer never writes main" is a storage fact here
//
// The platform answers searches with a model. If starting a project from an
// answer wrote research state, the model's output would reach a project's
// canonical state without a human in the loop. The split above is what makes
// that impossible rather than discouraged: between Start and Confirm the
// project has branches = 0, project_states = 0, state_commits = 0 and
// scientific_object_versions = 0, and the e2e test asserts exactly those
// counts against real PostgreSQL
// (tests/integration/search_start_project_test.go). It is checkable because
// the answer's own record (search_records) and the draft
// (research_context_drafts) are rows in OTHER tables — the draft is the
// candidate, and a candidate is not a state.
//
// # The refs: chosen from the search, never added to it
//
// A draft's referenced/dependency/candidate refs come from the search
// record's selected_refs — the set the answer was allowed to cite (docs/54
// ranks a fabricated or unauthorized citation as the top-severity failure).
// The caller may KEEP, DROP or RECLASSIFY within that set; a ref the search
// never returned is refused. This package refuses it before the project is
// created (so a caller's mistake costs no project), and migration 00134's
// research_context_draft_refs_guard refuses it again in the database, for any
// writer that never read this paragraph — the same guarding pair
// internal/search/answer keeps between its in-Go guard and 00121's CHECK.
//
// # Where the draft's free text lives
//
// `uncertainties` and `hypotheses` are stored on the draft row and nowhere
// else. V1 has no object type and no schema field for either, docs/14:25
// names them as parts of the DRAFT, and inventing a schema for them would be
// inventing the product's vocabulary (CLAUDE.md §5: L3). At confirmation they
// are not dropped — the draft row stays, and it is the record of what the
// candidate said when it was confirmed — but they do not become scientific
// objects. That is stated in the package's result type, in the migration's
// comments, and in the task result, so nobody has to infer it from silence.
//
// # Idempotency
//
// Both routes are writes and both carry the contract's Idempotency-Key
// (specs/api/openapi.yaml, components.parameters.IdempotencyKey: required,
// minLength 8). The rules are the platform's usual ones (00070, 00072,
// 00083): a key that names what the caller asked for already is answered with
// that result, and a key that names something else is refused
// (IDEMPOTENCY_CONFLICT) rather than silently doing the second thing. See
// migration 00134 for why the start route ALSO has one draft per search
// regardless of key: clicking "Start Research Project" twice with two
// different keys must not open two projects for one answer.
package researchcontext
