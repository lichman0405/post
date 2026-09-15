// Package milestonehttp is the project milestone API (T0609): the
// research-timeline surface of one project — GET the timeline, POST one
// milestone record (kind + custom label + occurred_at date + optional
// release link), GET one milestone. Milestones are deliberately a
// separate object from releases: no release is required to record one,
// and recording one never touches the project lifecycle (no "completed"
// terminal state is forced — docs/43 §1).
//
// The reads run the project visibility gate (a milestone is exactly as
// visible as the project carrying it); the create runs the command's own
// authorization (ActionCreateRelease — see the milestones package doc).
// The create carries the caller's Idempotency-Key (docs/22): a retry
// replays the first create instead of duplicating a timeline row.
package milestonehttp
