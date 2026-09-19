// Package embedding owns the vector half of the search projection: the port
// an embedding provider is reached through, and the batch job that fills
// search_documents.embedding from it (T0902).
//
// The port exists because the architecture requires it, not for symmetry:
// docs/20 §5 — "LLM only does query planning/answer interpretation, never
// source of truth; embedding/LLM providers go through a port abstraction" —
// and docs/52: Gitea, S3, Redis, email, LLM and the scientific adapter are
// all reached through ports, and no external provider's ID becomes a domain
// primary identity. A document is addressed by its entity_ref; the embedder
// is recorded on the row as an attribute (00092's embedding_provider /
// embedding_model / embedding_version), never as an identifier.
//
// # What is here, and what is deliberately not
//
// There is exactly ONE implementation, and it is wired into the worker as
// the production one (cmd/worker: `embedding.NewDeterministic`). It is also
// NOT a semantic embedding, and this package does not pretend otherwise:
// Deterministic computes a bag-of-terms vector in-process, offline and
// reproducibly (see deterministic.go), so it captures lexical overlap, not
// meaning. It is a PLACEHOLDER standing where the real provider will stand —
// the row is real, the column is real, the provenance is real, and the
// numbers are not a semantic representation of the text. Nothing may rank or
// recommend off it as if it were one. A row produced by it is identifiable
// in the data by `embedding_provider = 'post-local'` (embedding_model =
// 'sha256-bag'), so a real provider's rows, once it lands, are distinguishable
// from the placeholder's by that column alone instead of by a migration or an
// assumption about when the change was deployed. Replacing the placeholder is
// a deployment change plus a recompute — the provenance columns are what make
// the two populations separable, and the recompute is what makes the
// replacement cheap.
//
// Nothing in specs/ or docs/ says whether platform content may
// be sent to a third-party embedding service — docs/55 forbids SECRETs in
// embeddings, docs/59 forbids third-party analytics, docs/23 forbids
// unauthorized visibility, and none of them answers "may a private document
// leave the platform to be embedded?" — and that question is simultaneously
// a privacy boundary and a purchased-service decision. So this task ships
// the port, the in-process implementation and the batch job, and the real
// provider arrives as its own task once that question is answered; the port
// is the seam it lands in. There is no outbound call anywhere in this
// package, and no configuration that could add one: offline_test.go's
// TestPackageImportsNothingThatCanCallOut (with TestImportCheckCanFail as
// its own control) and the `go list` commands recorded in the task result
// both measure that, rather than it being asserted in prose. Neither says
// anything about the transitive closure — `go list -deps` there does reach
// `net`, through pgx, because the batch job writes to PostgreSQL.
//
// # The job
//
// Worker (batch.go) is the projection's second writer: it selects the
// documents whose vector is not the current model's, embeds them in batches,
// and writes the vector with the provenance that produced it. It is
// idempotent in the strong sense — a second run over an up-to-date row
// selects nothing, so it writes nothing — and it is rebuildable: the
// embedding is derived state, so throwing it away and recomputing is the
// repair, exactly as it is for the documents themselves
// (internal/search/rebuild.go).
package embedding
