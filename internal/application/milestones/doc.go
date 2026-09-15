// Package milestones is the project milestone use case (T0609): the
// research-timeline markers of one project — candidate selected, paper
// submitted, patent filed, external validation, or a custom-labeled
// marker — deliberately a separate object from releases. A release is an
// immutable snapshot of accepted state; a milestone is a recorded
// research event positioned on the project's timeline by its occurred_at
// date. A milestone MAY name the release it documents; it never requires
// one, and it never touches the project lifecycle (recording milestones
// does not advance projects.activity_status — the project state machine
// keeps no forced "completed" terminal state, docs/43 §1).
//
// The command owns input validation (the ports trust, the command
// verifies) and authorizes the create against the matrix
// (ActionCreateRelease — the governance action whose actor classes match
// the release surface; the milestone vocabulary has no action of its own
// yet and the authz package is outside this task's scope, see the task
// result). The reads (List/Get) do NOT authorize here: the transport
// resolves the project's read visibility first (the same gate every
// project read runs — a milestone is exactly as visible as the project
// carrying it), then calls them. They do enforce the project boundary: a
// milestone id of another project is "not found", and a release link that
// does not name a release of the same project is "not found" too
// (existence hiding, docs/45).
//
// The timeline is the List order: occurred_at ascending — the events'
// dates, the canonical kinds' natural progression — with creation order
// (created_at, id) breaking same-date ties. Insertion order never
// influences the timeline, so milestones recorded out of chronological
// order still render in research order (acceptance: milestone timeline
// correct).
package milestones
