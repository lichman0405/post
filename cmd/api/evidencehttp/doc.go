// Package evidencehttp is the transport of the evidence-graph read (T0506):
// GET .../objects/{objectId}/evidence and GET
// .../hypotheses/{objectId}/evidence.
//
// # What the handlers decide, and what they do not
//
// They decide the GATE ORDER and the wire shape's codes, and nothing else.
// The project read gate runs first on both routes (a denied reader and an
// unknown project answer the same existence-hiding 404 for every object id),
// the caller is resolved here the way every other project read resolves it,
// and a client-shaped version_no is refused before any read. Everything the
// answer contains — the grouping by pinned target version, the stance labels,
// the two hypothesis sections — is decided in internal/evidence and
// internal/application/evidencegraph; this package renders their own JSON
// shapes as-is, so a field cannot be dropped or invented in a layer that has
// no test for it (the same decision knowledgepublish.Preview and
// evidencenetwork.Section make).
//
// # Read only, and separately from provenance
//
// There is no write verb on this surface: an evidence assertion is written by
// the RSG evidence-assertion route inside a state commit. The routes carry no
// /provenance/ prefix and the packages behind them read no provenance_edges
// row (CLAUDE.md §9 invariant 10: Provenance Graph != Evidence Graph); the
// package's own test, separation_test.go, scans both halves of that claim
// rather than leaving it to this comment.
package evidencehttp
