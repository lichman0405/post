// Package diffs is the Research State Diff use case (T0401): the read
// that renders the three-way semantic diff of docs/07 §8 — base, source
// and target state — as the change list a Research PR proposes
// (internal/rsg/diff, docs/06 §6, docs/09 §4).
//
// Diff is a system-facing read, not an API command: there is no HTTP
// route in this task and the service performs no authorization of its
// own. The consumers that authorize are T0402 (the PR domain, which pins
// base/proposed states), T0408 (the PR page) and the future merge flow,
// each of which resolves visibility before calling it.
//
// The service reads exactly what the engine needs: the three state rows
// (identity, project, recorded git ref) and the three lineage snapshots.
// Nothing mutable is read — a diff is a pure function of the three
// states' recorded content — and no project row is consulted beyond the
// project-membership check of the three states themselves.
package diffs
