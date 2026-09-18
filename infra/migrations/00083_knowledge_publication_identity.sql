-- +goose Up
-- Published knowledge object identity and publish idempotency (T0805).
--
-- knowledge_publications has existed since 00010 with no writer at all: the
-- table was created for the publication of a scientific object version
-- (docs/03 §: a Published Knowledge Object is a Research Question /
-- Hypothesis / Claim / Finding version explicitly published to the
-- network), the event name has been in specs/events/event-types.yaml since
-- the vocabulary was written, and the public read route has been in the
-- contract — but nothing had ever inserted a row. T0805 is the publish
-- path, and this migration adds the two things that path needs and the
-- table cannot express.
--
-- 1. pid — the publication's persistent identifier.
--
-- The published knowledge object is the thing that is referenced across
-- projects (docs/05 §: every page displays a persistent identity) and the
-- thing GET /knowledge/{knowledgeId} resolves. Its identity is minted ONCE,
-- at publication, exactly as a research asset's pid is minted at its
-- publication (00064): 26 lowercase Crockford base32 characters, random,
-- never derived from the version label, the title, the object, or the
-- project — all of which are revisable, and an identifier derived from a
-- revisable attribute is not persistent (00064's own acceptance: asset ID
-- 不随 slug/org 变化).
--
-- The column sits on the PUBLICATION rather than on scientific_objects, and
-- that is the shape of the domain and not a preference:
--
--   - docs/03 defines the Published Knowledge Object as a VERSION, not as
--     the object that carries it; what is published (and what a foreign
--     project cites) is one version.
--   - owner ruling L3-20260916-1 #3 (tasks/decisions.md) makes a version
--     publishable exactly once, so publication and version are 1:1 and the
--     publication's pid IS that version's public identity.
--   - the same ruling is what removes the ambiguity the public read would
--     otherwise have: resolving a pid yields exactly one row, so
--     GET /knowledge/{knowledgeId} never has to invent a rule for which row
--     of a multi-row publication history to answer with.
--
-- A pid on scientific_objects would instead hand out a public identity to
-- objects nobody has published (the identity must mean "this is out on the
-- network"), and would still leave a pid naming several published versions.
--
-- The DEFAULT is a random uuid's first 26 hex characters — a Crockford
-- subset — so a row inserted by anything that does not go through the
-- application still gets a valid, unique pid. It is a fallback, not the
-- path: the publish command mints the pid with assets.NewPID() (the
-- generator 00064 names as "the application-generated path ... arrives with
-- the publish command"), and the CHECK below is the Go predicate's mirror.
-- The table is empty at this commit — no code path has ever written a row —
-- so the NOT NULL + DEFAULT is a column addition, never a backfill.
--
-- 2. knowledge_publication_creations — the Idempotency-Key ledger.
--
-- docs/22 gives an Idempotency-Key to create/command/publish/merge/release,
-- and the knowledge publish carries one (specs/api/openapi.yaml, the same
-- header the asset publish declares). A publish is a governance write whose
-- result is immutable, so a retried publish must NOT write a second
-- publication and must not be answered with a conflict either: the request
-- that already succeeded is the request being repeated, and the honest
-- answer is the publication it produced. This ledger is what makes that
-- answer possible after the fact — the key maps to the publication the
-- FIRST request wrote, and a replay is a read of that row.
--
-- The four existing ledgers cannot carry it: release_creations (00053) and
-- project_milestone_creations (00063) both have a NOT NULL foreign key to
-- the row their operation creates (a publish writes no release and no
-- milestone), and asset_publish_creations (00072) is scoped to a
-- research_asset_versions row, a different table with a different subject.
-- The scope is the project, as it is for all three.
--
-- Append-only, like the row it points at and like the other ledgers: both
-- halves of the guard are taken from 00053/00063/00072 — the 00014 row guard
-- (BEFORE UPDATE OR DELETE) and the 00015 TRUNCATE guard (BEFORE TRUNCATE,
-- statement level), because a ledger that can be truncated is not
-- append-only and TRUNCATE fires no row trigger.
--
-- UNIQUE(project_id, idempotency_key) is the whole correctness claim: two
-- concurrent publishes carrying one key either create one ledger row (and
-- one publication) or fail the second on this constraint, which the store
-- answers as a replay of the winner, never as a second publication.

ALTER TABLE knowledge_publications
  ADD COLUMN pid text NOT NULL
    DEFAULT substr(replace(gen_random_uuid()::text, '-', ''), 1, 26);

ALTER TABLE knowledge_publications
  ADD CONSTRAINT knowledge_publications_pid_format
    CHECK (pid ~ '^[0-9a-hjkmnp-tv-z]{26}$');

CREATE UNIQUE INDEX knowledge_publications_pid_uniq ON knowledge_publications (pid);

-- One publication per object version (owner ruling L3-20260916-1 #3).
--
-- This is an APPLICATION rule and it is enforced there: the command reads
-- the version's publication before it writes and refuses a second one,
-- because the table's own UNIQUE(object_version_id, public_version) (00010)
-- only forbids reusing the same NAME — publishing the same version again
-- under a different public_version would satisfy it and still be the second
-- publication the ruling forbids. A unique index on object_version_id alone
-- would express the rule exactly, and it is deliberately NOT added here:
-- the ruling is a product rule ("同一个知识对象版本只能对外发布一次"), and
-- encoding a product rule as a schema constraint makes it unchangeable by
-- any later ruling that allows, say, a corrected republication under a new
-- public_version. The application refuses it, with a test that pins the
-- refusal (tests/integration/knowledge_publish_test.go), and that test is
-- what the reviewer checks — not a constraint nobody can cite.

CREATE TABLE knowledge_publication_creations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  idempotency_key text NOT NULL,
  publication_id uuid NOT NULL REFERENCES knowledge_publications(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(project_id, idempotency_key)
);

CREATE TRIGGER knowledge_publication_creations_append_only
  BEFORE UPDATE OR DELETE ON knowledge_publication_creations
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER knowledge_publication_creations_no_truncate
  BEFORE TRUNCATE ON knowledge_publication_creations FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();
