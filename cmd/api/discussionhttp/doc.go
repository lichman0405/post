// Package discussionhttp is the discussion API (T0811): the conversation a
// project, a published knowledge object or a research pull request carries,
// and the promotion of one comment into a proposed research object (an
// Issue, a Hypothesis or an external-evidence proposal).
//
// # Routes
//
//	POST   /api/v1/projects/{projectId}/discussions
//	GET    /api/v1/projects/{projectId}/discussions?target_type=&target_id=
//	GET    /api/v1/projects/{projectId}/discussions/{threadId}
//	POST   /api/v1/projects/{projectId}/discussions/{threadId}/comments
//	DELETE /api/v1/projects/{projectId}/discussions/{threadId}/comments/{commentId}
//	POST   /api/v1/projects/{projectId}/discussions/{threadId}/comments/{commentId}/promotions
//	GET    /api/v1/projects/{projectId}/discussions/promotions?ref=
//	GET    /api/v1/projects/{projectId}/discussions/promotions/{promotionId}
//
// The routes are registered in the repository's existing style (full-path
// patterns on the v1 mux, the technique releasehttp and milestonehttp use)
// and are named here for the contract: specs/api/openapi.yaml is the
// Supervisor's to maintain and this task does not edit it, so the shapes
// these handlers read and write are reported in the task result for the
// contract to adopt.
//
// # What the surface serves, and what it withholds
//
// A thread carries its target in the target's own addressing scheme
// (project uuid, publication pid, pull request number) — the same identity
// its own surface displays. A comment is served with its author and its
// time; a withdrawn comment is served WITHOUT its body and with its
// tombstone (deleted: true plus who and when), because the row is never
// removed and the conversation's history is what the surface shows
// (CLAUDE.md §9.8).
//
// # Authorization
//
// Reads run the project visibility gate (the command does: a discussion is
// exactly as visible as the project carrying it). Writes need an
// authenticated actor — the guard refuses an unauthenticated request before
// routing, and the handlers keep a 401 backstop — and a promotion
// additionally evaluates the matrix cell of write_scientific_state, exactly
// as the RSG write path does. No new action is added to the matrix.
package discussionhttp
