// Package rsg is the consuming API service for the research state graph
// (T0208): the commands that create research branches, scientific object
// versions and typed relations, and read them back. It is the "consuming
// API task" the sciobjects/relations ports assigned authorization to
// (internal/application/relations/ports.go: "authz belongs to the
// consuming API task").
//
// Every write command resolves the caller's membership/role in the target
// project first, then evaluates the action through internal/authz — branch
// creation asks ActionCreateBranch, scientific-state writes (object
// create/version, relation create) ask ActionWriteScientificState — with
// the same require shape as internal/application/projects (fail closed:
// nil engine, engine error and non-permitting verdicts all refuse the
// write). The project read gate runs as part of the membership resolution,
// so a denied caller is refused before any object or relation is looked
// up: the denial looks identical whether the object exists or not
// (existence hiding, docs/45).
//
// Scientific-state writes land as state commits (states.Service.Commit,
// gate draft, via api, manifest "v1"): object and relation ids are
// pre-generated so the commit's operation summary names the real entities
// (the pr gate's commit_linkage check), and the object/relation rows are
// written inside the commit transaction, so a version and its state
// transition are one atomic outcome.
package rsg
