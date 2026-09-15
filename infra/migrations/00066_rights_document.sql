-- +goose Up
-- Rights model (T0703): the two rights_json columns hold a rights
-- document — a JSON object — and nothing else.
--
-- A published research asset version and a knowledge publication each
-- carry their rights declaration in rights_json (00010): the standard
-- license id, the custom agreement reference, the usage declarations
-- (commercial use, derivatives, redistribution, model training,
-- attribution, patent grant) and the two access axes. The document's
-- shape is the one specs/policies/rights-template.yaml fixes, modelled in
-- Go by internal/rights (Document, Usage, Visibility — with the JSON tags
-- that make that struct the stored format).
--
-- What the database guarantees is deliberately narrow: the column holds a
-- JSON OBJECT. A scalar or an array in a column named rights_json is not a
-- small document, it is a value of the wrong kind — no reader can take it
-- as a declaration, so every reader would have to special-case it. The
-- rule is the storage boundary's own; it says nothing about the contents,
-- and a reader may still find any field absent.
--
-- The vocabulary is NOT enforced here: no version check, no license-id
-- shape, no usage enum, no cross-field rule. The first reason is the
-- decisive one. The vocabulary has exactly one definition, internal/rights,
-- and a CHECK spelling its values again is a second definition that drifts
-- silently the first time one side changes — the reasoning 00064 gives for
-- leaving the origin ref's kind:value shape to internal/assets. The second
-- is expressiveness: "version is exactly 1", "patent_grant = see_agreement
-- requires custom_agreement_ref", "each usage value is one of three words"
-- are one-line predicates in Go and multi-line JSON-path expressions in
-- SQL, in columns whose only readers are Go.
--
-- {} is accepted, deliberately. It is what the existing fixtures store
-- (tests/integration/asset_core_test.go, append_only_test.go) and what any
-- row written before this model holds; it is not a rights document —
-- internal/rights.Parse refuses it for stating no version — and this
-- migration does not repair it. No application path has ever written these
-- columns (the publish command is T0705), so there is no data to repair,
-- and inventing a declaration for a row that stated none is precisely the
-- upgrade 00064 warns against: a backfill may fill in what the old schema
-- could not express, never decide what a publisher meant. A row that
-- carried a scalar would make this migration fail rather than pass
-- silently — the intended outcome, because such a row is a corrupt
-- document and not a legacy shape.
--
-- Both columns are constrained in the same migration because they are one
-- model: knowledge_publications.rights_json is the same document for a
-- published scientific object version that research_asset_versions
-- .rights_json is for an asset version. A rule applied to one column but
-- not the other is how the two documents' shapes diverge.

ALTER TABLE research_asset_versions
  ADD CONSTRAINT research_asset_versions_rights_json_document
    CHECK (jsonb_typeof(rights_json) = 'object');

ALTER TABLE knowledge_publications
  ADD CONSTRAINT knowledge_publications_rights_json_document
    CHECK (jsonb_typeof(rights_json) = 'object');
