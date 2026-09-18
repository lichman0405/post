// Package knowledgepublish is the knowledge publication command (T0805):
// the governance write that publishes one scientific object version to the
// network, and the pure model both the write and the read path are decided
// by.
//
// # What the task was, and what was already there
//
// knowledge_publications has existed since migration 00010 with no writer
// at all: the table, the event name (knowledge.version_published,
// specs/events/event-types.yaml) and the public read route
// (GET /knowledge/{knowledgeId}, specs/api/openapi.yaml) were all in
// place, and nothing had ever inserted a row — the generated query
// PublishKnowledgePublication sat unused. This package is the missing
// path. It follows internal/application/assetpublish, the shipped
// precedent for the same operation on the asset surface, step for step:
// shape, agent backstop, authorization, idempotency replay, and then one
// transaction in the store that re-runs every decision over the state it
// actually sees before it writes.
//
// # 发布不等于公开 — publishing does not publish
//
// docs/12 §2: a Private Project's project/RSG/private blobs are invisible
// by default, and an Asset, Knowledge object or Attestation may still be
// explicitly published. Owner ruling L3-20260916-1 #1 (tasks/decisions.md)
// states the same thing for knowledge: publishing records a STATE, and
// visibility is a DIFFERENT axis — the one that already existed,
// scientific_object_versions.visibility_policy_id (migration 00005), which
// is part of the RSG manifest, is server-authoritative, and is treated as
// a rights field by RSG conflict resolution.
//
// So this command never widens anything, and deliberately has no
// visibility argument to widen with:
//
//   - It publishes a version whose visibility policy is pinned exactly as
//     happily as one that inherits the project's — the assets precedent's
//     rule ("a private publication widens nothing; the rule is about
//     public assets and is not read",
//     internal/application/assetpublish/command.go).
//   - It adds no public/private column. The axis exists; naming a second
//     one here would be the parallel axis the ruling forbids.
//   - The read path (AudienceFor, below) is what decides who may see the
//     result, and it reads that existing axis — not the project's
//     visibility alone.
//
// # Lifecycle: private candidate → publication_review → published
//
// docs/43 §Publication is the only written publication state machine:
// private candidate → publication_review → published, failure returns to
// private candidate, and "there is no automatic published". The middle
// cell is realised as the EXISTING review channel rather than a new
// column: a version may be published only when the research PRs that
// accepted its state into main carry an approved scientific review and an
// approved integrity review — the same record and the same predicate the
// release gate reads (releases.ReleaseService.ReleaseFacts →
// approvedKinds(records, "scientific", "integrity"),
// internal/application/releases/service.go). The chain is: a version
// reaches main only through a Research PR, a Research PR is accepted only
// with its recorded reviews, and this command re-reads that record inside
// its own transaction. There is therefore no path to `published` that did
// not go through review, and no place a reviewer's decision has to be
// re-entered.
//
// # The second publication of a version is refused
//
// Owner ruling L3-20260916-1 #3: one version is published to the network
// exactly once; a second publication is refused, not an overwrite and not
// a new row. The database does NOT say this — UNIQUE(object_version_id,
// public_version) (00010) only forbids reusing the same NAME, and
// republishing the same version under a different public_version would
// satisfy it. The rule is enforced here (Judge, ReasonAlreadyPublished)
// and pinned by a test.
//
// # public_version is the publisher's name, stored verbatim
//
// Owner ruling L3-20260916-1 #2: public_version is the name the
// publication is shown under, supplied by the publisher in the request and
// stored exactly as sent. This package refuses a missing or blank name and
// a name beyond a bound, and does nothing else to it: no trimming, no
// case folding, no slugification, no derivation from the version number.
package knowledgepublish
