// Package attestations is the private-evidence / public-attestation
// surface (T0812): the governance write that lets a project publish a
// minimal, non-disclosing statement about a PUBLIC object or asset version
// it did not author, and the ONE projection that decides what a public
// reader is shown of it.
//
// # What an attestation is, and what it is not
//
// docs/12 §2 逐字: "Private Project：project/RSG/private blobs 默认不可见；
// 可显式 Publish Asset/Knowledge/Attestation" — publishing an attestation
// is the one way a private project's judgment reaches the network without
// the private work coming with it. docs/13 §5 states the same property for
// the organization-issued form ("不泄露具体 private research"), and
// docs/22 §28 lists create attestation among the high-risk commands that
// need server-side authorization plus policy validation.
//
// An attestation is NOT evidence, and the difference is structural rather
// than editorial:
//
//   - An evidence assertion (internal/domain.EvidenceAssertion,
//     evidence_assertions) pins TWO versions — the target and the evidence
//     cited — and carries the substance: relation, directness,
//     inference_nature, scope, reasoning_note. That is what makes it
//     evidence: it says WHY, and it names what it is citing.
//   - An attestation pins ONE version and carries no substance at all: a
//     validation type from a closed vocabulary, a three-valued result, and
//     whether the attesting organization is named. It cites no evidence
//     version, holds no note, and has no column a private detail could be
//     written into (migration 00120).
//
// So "attestation != public evidence" is not a caption this package prints
// on a page — it is the shape of the row. A reader of an attestation learns
// that somebody validated the target and how it went; they learn nothing
// about what the validation rested on, and the projections below have no
// field in which they could.
//
// # 发布不等于公开 still holds, and here it is the whole point
//
// The publish ruling (L3-20260916-1) is that publishing records a STATE and
// widens no visibility. This surface takes the same posture from the other
// end: an attestation is written already public, and what it publishes is
// ONLY itself. It does not touch the attesting project's visibility, the
// basis state's visibility, or the visibility of anything in it — the
// private side stays exactly as visible as it was, and the attestation is a
// new row that names it by id to nobody.
//
// # The private side: cited, never read out
//
// The attesting project (attesting_project_id), the state its evidence
// lives in (basis_state_id) and the review that authorised the statement
// (internal_review_id) are all recorded on the row — an attestation that
// rested on nothing would be a bare assertion, and the point of requiring
// them is that there IS something underneath. But the public read does not
// read them: ResolvePublicAttestation (internal/persistence/queries/
// attestations.sql) has no column for any of the three, so the projection
// cannot drop what it was never handed.
//
// # What the projection decides
//
// Present is the ONE place that turns a row into what a reader may see, and
// it has exactly one judgement in it: whether the attesting organization is
// named. The rule is a CONJUNCTION of two independent gates —
//
//   - the attestation's recorded org_visibility, which is the promise made
//     when the statement was issued, and
//   - the organization's CURRENT standing setting
//     (organizations.attestation_attribution, migration 00120),
//
// and the narrower of the two wins. An organization that flips to
// anonymous stops being named on everything it ever issued; flipping back
// does not re-name the attestations issued while it was anonymous. Both
// directions are the same rule, and both failure modes are ones a reader of
// this package would expect a privacy surface to have.
//
// # L3 boundary
//
// The organization-issued AGGREGATE attestation docs/13 §5 and docs/04 §7
// describe — a confidential verified contribution summary on a person's
// profile, carrying a "披露级别" the specifications never define — is NOT
// this package and is not built here. The recorded gap is
// internal/application/researchprofile/doc.go: an aggregate needs three
// things no specification fixes (who may issue one, the disclosure-level
// vocabulary, and the rule for when a count stops being a leak), and
// docs/23 §5 adds a fourth constraint in the same direction ("私有对象计数
// 也不能通过 public API 泄漏"). This package renders one attestation, by
// its own pid, with no count of anything anywhere in it — which is the part
// the specifications do fix.
package attestations
