// Package diff computes the three-way Research State Diff of docs/07 §8:
// the semantic change a source branch proposes against a merge base, with
// the target branch's concurrent change reported alongside (T0401). The
// PR page (docs/06 §6) renders this list — created/updated/aborted
// scientific objects and changed relations — ahead of the raw Git diff,
// and the semantic merge/conflict tasks (T0405) consume the target-side
// field information.
//
// Design decisions (L1, recorded here because they are the contract the
// consumers build on):
//
//   - A change is a version change. Rows are append-only (migrations
//     00014/00015) and every semantic write lands a new version, so the
//     engine compares the head version of each object/relation in the
//     base lineage against the head version in the source lineage — a
//     different head version id is a change, whatever the content delta.
//     Two heads whose content is identical still differ (the write
//     happened); the changed-fields list may simply be empty.
//
//   - Nothing disappears (invariant 8): an object present in the base but
//     not in the source is NOT listed — removal is not representable, so
//     the change list enumerates source-side changes only.
//
//   - Kind classification for objects: created (absent in base), aborted
//     (head lifecycle flipped into 'aborted'), reopened (flipped out of
//     'aborted'), updated (everything else). Relations carry no lifecycle
//     column in V1, so relation kinds are created/updated only.
//
//   - The target side is reported, never judged: TargetMoved plus the
//     base→target changed fields feed the semantic conflict detector
//     (T0405). This engine deliberately does not classify conflicts or
//     decide winners — scientific conflicts are never auto-resolved
//     (docs/07 §8, CLAUDE.md §9).
//
//   - Scope: objects and relations. Evidence assertions, blob attachments
//     and schema-registry changes are not part of the V1 diff (recorded
//     follow-ups); the input snapshots may carry blob refs and this
//     package ignores them.
//
//   - Canonical serialization: the diff marshals to the same bytes for
//     the same inputs. Object changes sort by object id, relation changes
//     by relation id, new-version trails by version_no, changed fields in
//     the fixed canonical field order, file diff refs source-first, and
//     every payload is re-encoded through manifest.CanonicalJSON before
//     comparison and output. The golden files under testdata/ pin these
//     bytes.
package diff
