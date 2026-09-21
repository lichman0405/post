// Package attestationhttp is the private-evidence / public-attestation
// surface (T0812): the publish pair (POST .../attestations:publish-preview
// and POST .../attestations:publish) and the public read the statement makes
// possible (GET /api/v1/attestations/{attestationId}).
//
// # Where the routes come from — an L1 decision, stated plainly
//
// The knowledge publication surface's three paths are the CONTRACT's
// (specs/api/openapi.yaml). This surface's are NOT: the specifications name
// the capability (docs/12 §2 "Private Project … 可显式 Publish
// Asset/Knowledge/Attestation"; docs/13 §5; docs/22 §28 lists create
// attestation among the high-risk commands) but fix no path for it, and
// specs/** is not this task's to write. The paths below therefore follow the
// shape the contract gave the publication pair, one noun over:
//
//	POST /api/v1/projects/{projectId}/attestations:publish-preview
//	POST /api/v1/projects/{projectId}/attestations:publish
//	GET  /api/v1/attestations/{attestationId}
//
// `{attestationId}` is the attestation's PID, exactly as `/knowledge/
// {knowledgeId}` is the publication's. The read is deliberately NOT nested
// under a project: the identity a client holds is the pid, and — more to the
// point here — nesting it under the ATTESTING project's segment would put
// that project's id in the URL of a public page about somebody else's work,
// which is the one thing this surface exists not to do.
//
// Recording this here and in the task's RESULT rather than in specs/ is
// deliberate: ADR-style, the Supervisor owns the contract, and a Worker that
// edited it would be fitting the spec to the implementation.
//
// # What the handlers decide, and what they do not
//
// Nothing about an attestation is decided here. The body is read, the
// principal is resolved, and the command runs; whether this actor may attest,
// whether the target is public, whether the internal review authorises the
// statement and what the row contains all belong to
// internal/application/attestations and commit or refuse inside that
// command's transaction. A transport that decided any of it would be the
// second, weaker copy of a security decision.
//
// The read route decides one thing, and it is the one thing that belongs to a
// transport: that a pid-shaped segment which resolves to nothing and a
// segment that is not a pid at all answer the SAME 404, so this route cannot
// be walked to learn which pids exist. It decides nothing about what may be
// shown: an attestation is written already public, and its audience is
// everyone (migration 00120 — the table has no visibility column, because a
// "private attestation" would be a second kind of row on a table whose whole
// point is that what is here is public).
//
// # The private side never reaches this layer
//
// The attesting project, the basis state and the internal review are the
// statement's private footing. This package has no field for any of them:
// attestations.PublicAttestation — the only value the read returns — has no
// such field, and the query behind it has no such column. The redaction is
// therefore not a rendering rule this file could get wrong; it happened
// before the row was read.
package attestationhttp
