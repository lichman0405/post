// Package rights owns the rights layer per ADR-009 and
// docs/12_PERMISSIONS_RIGHTS_POLICY.md: visibility, data access and usage
// rights are separate axes (T0002 scaffold, populated by T0703).
//
// # The model
//
// A Document is the machine-readable rights declaration of one published
// research asset version or knowledge publication: a standard license
// id, a custom agreement reference, the six usage declarations docs/12 §4
// names (commercial use, derivatives, redistribution, model training,
// attribution, patent grant), and the two access axes the rights layer
// keeps apart (metadata visibility vs blob data access).
//
// Documents are stored as the rights_json jsonb column of
// research_asset_versions and knowledge_publications (migrations 00010,
// 00066). The JSON shape is not an internal detail that a transport layer
// re-invents: it is the shape specs/policies/rights-template.yaml fixes,
// and the stored, API-served and rendered document are the same
// document, so the field names and the closed vocabularies live here with
// JSON tags.
//
// # What this package is not
//
// It is not a legal opinion and it is not a license registry (ADR-009:
// 不发明新的法律许可证 — do not invent new legal licenses; docs/38 §1: the
// platform records the declaration and does not judge it). Nothing here
// decides whether a license id names a real license, whether an agreement
// grants anything, or whether a use is lawful; that is why the UI stays
// on the recording side of the boundary (docs/38 §3).
//
// # How it is enforced
//
// Shape rules — a version this package understands, values from the fixed
// vocabularies, a well-formed license id, a bounded agreement ref — are
// enforced by Validate, and by Parse for stored bytes. The database
// enforces only that the two rights_json columns hold a JSON object
// (migration 00066): the vocabulary has one definition, here, and a CHECK
// spelling it again in SQL would be a second one to drift from.
package rights
