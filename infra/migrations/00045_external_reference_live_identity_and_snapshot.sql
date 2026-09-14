-- +goose Up
-- External Reference live identity + snapshot guards (task T0508).
--
-- The 00011 tables already give the shape: external_references is the
-- LIVE identity (source_type + external_identifier UNIQUE, canonical_url)
-- and external_reference_snapshots is the pinned snapshot log
-- (accessed_at, upstream_version, metadata, snapshot_hash, blob_id),
-- already append-only via 00014/00015. What 00011 left open — and what
-- this migration closes for ANY write path (application code, psql, a
-- leaked credential), in the 00040 discipline — is four things:
--
--   1. THE IDENTITY IS LIVE. The identity row is synced from
--      external_reference scientific object payloads: whenever a version
--      of an external_reference object carries its identity pair
--      (source_type + external_identifier — the schema's required pair),
--      the identity row exists afterwards. One identity per normalized
--      pair, across all projects: every project citing the same DOI
--      shares one identity row (docs/19 §2). The upsert fills a missing
--      canonical_url but never overwrites an existing one (first writer
--      wins; the identity is the shared canonical fact).
--
--   2. IDENTIFIERS ARE NORMALIZED. DOI-shaped identifiers are
--      normalized before the unique pair is formed: the doi:/URL prefix
--      is stripped and the DOI is case-folded (DOI names are
--      case-insensitive, ISO 26324). Without this, "doi:10.1000/XYZ"
--      and "10.1000/xyz" would key two identities for one DOI. The Go
--      side is domain.NormalizeExternalIdentifier; the integration test
--      pins the SQL copy to the Go copy. Both copies trim and match
--      with the SAME explicit deterministic ASCII whitespace set
--      (SPACE TAB LF CR FF VT: Go spells it " \t\n\r\f\v", SQL spells
--      it E' \t\n\r\f\x0b' — E'' strings have no \v escape, so VT is
--      written \x0b) and therefore answer identically for identical
--      inputs. POSIX [[:space:]] and unicode-aware trimming are
--      deliberately NOT used on this surface: [[:space:]] is
--      locale-dependent, and the two copies must never disagree.
--
--   3. SNAPSHOTS CANNOT LIE. metadata must be a JSON object (a snapshot
--      of upstream metadata is a document, never a scalar); accessed_at
--      must not be in the future (it records when upstream WAS
--      observed); and snapshot_hash is derived from the stored metadata
--      bytes: a NULL hash is filled with the sha256 of the
--      jsonb-canonical metadata text, a supplied hash that does not
--      match is refused. The hash therefore always pins the bytes the
--      row actually holds — the same integrity contract as
--      scientific_object_versions.integrity_hash (docs/21 §10).
--
--   4. CITATION/DEPENDENCY IS THE ONLY ADMITTED EDGE. docs/19 §3: a
--      relation pointing AT an external reference is either "references"
--      (citation: background knowledge, upstream changes only notify) or
--      "depends_on" (dependency: actual input, upstream changes trigger
--      impact analysis). No other relation type may target an external
--      reference, and an external reference may never be a relation
--      SOURCE in V1 (relations are claims about the project's research;
--      a claim about what an external source says is an evidence
--      assertion, T0504's surface, not a relation). The two admitted
--      types are declared in external_reference_relation_types, kept in
--      lockstep with internal/rsg/relationcatalog by an integration
--      test.
--
-- PRECONDITION — DATABASE COLLATION. This database must be created with
-- a Unicode-aware collation (an initdb --locale other than C/POSIX, or
-- an explicit ICU/glibc Unicode collation). The identity normalization
-- case-folds with SQL lower(), and lower() folds non-ASCII characters
-- ('Ä' -> 'ä') ONLY under a Unicode-aware collation, while the Go copy
-- (strings.ToLower) folds Unicode unconditionally. On a C-collation
-- database the two copies would disagree on non-ASCII DOI spellings and
-- silently build two identity rows for one DOI. The integration test
-- TestExternalReferenceCollationPrecondition asserts the fold and FAILS
-- on a C-collation database, turning the mismatch from a silent
-- duplicate into a red build. Changing how databases are created
-- (infra/docker-compose, initdb parameters) is outside this migration's
-- scope.
--
-- Deliberately NOT enforced here (documented boundaries, not omissions):
--
--   * source_type enums: they are schema-governed scientific content,
--     validated by the schemareg/gate ladder per write; a DB CHECK would
--     couple every future schema enum change to a migration (the same
--     decision 00040 records for question_state/assessment).
--   * canonical_url shape: external_reference.schema.json declares
--     format: uri and says so itself — "this schema only constrains the
--     data shape". The DB guards enforce invariants (pairing,
--     normalization, hash integrity), never re-litigate schema shape;
--     dereferencing policy (https-only, SSRF guard) lives at FETCH time
--     in internal/rsg/externalref, the only place a URL is ever
--     dereferenced.
--   * snapshot presence on citation: whether a citation must pin a
--     snapshot before it may exist is a product-semantics decision
--     (docs/19 §2 says a snapshot is kept "when the project actually
--     references it" — the refresh flow records it, it is not a
--     citation precondition in V1). Left unenforced.
--   * which projects may share/see an identity: external_references
--     carries no project scope by design (00011 shape); the snapshot
--     READ model and its visibility belong to the consuming API task.
--   * the identity row itself is NOT append-only: it is live state by
--     definition (docs/19 §2), and refresh may fill its canonical_url.
--     History lives in the snapshots, and those ARE append-only.
--   * identity-row visibility timing: identity rows are written by a
--     DEFERRABLE INITIALLY DEFERRED constraint trigger, which fires at
--     the writing transaction's COMMIT — until then the row is invisible
--     to every transaction, the writer's own snapshot included.
--     Consumers that key on an identity row must re-read after commit;
--     the read-model consequences are the consuming API task's to design
--     (T0509, Issue #175).

-- ---------------------------------------------------------------------------
-- Identifier normalization (SQL copy of domain.NormalizeExternalIdentifier)
-- ---------------------------------------------------------------------------

-- The explicit deterministic ASCII whitespace set shared by every trim
-- and match on this migration's identity/snapshot surface: SPACE, TAB,
-- LF, CR, FF, VT. The Go copies (domain.ASCIIWhitespace, the doiShape
-- bracket classes) spell the same set as " \t\n\r\f\v"; SQL spells VT
-- as \x0b because E'' strings have no \v escape (E'\v' is the literal
-- character 'v', which would silently admit 'v' into the class). POSIX
-- [[:space:]] is deliberately NOT used: it is locale-dependent and
-- includes characters outside this set. The integration drift test feeds
-- the same inputs to both copies and asserts equality one by one — any
-- divergence fails there.
-- +goose StatementBegin
CREATE FUNCTION external_reference_trim(v text)
RETURNS text
LANGUAGE sql
IMMUTABLE
AS $$
  SELECT btrim(v, E' \t\n\r\f\x0b');
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION external_reference_normalize_identifier(identifier text)
RETURNS text
LANGUAGE sql
IMMUTABLE
AS $$
  SELECT CASE
    WHEN trimmed ~* E'^(https?://(dx\.)?doi\.org/|doi:[ \t\n\r\f\x0b]*)?10\.[0-9]{4,9}/[^ \t\n\r\f\x0b]+$'
      THEN lower(substring(trimmed FROM E'10\.[0-9]{4,9}/[^ \t\n\r\f\x0b]+$'))
    ELSE trimmed
  END
  FROM (SELECT external_reference_trim(identifier) AS trimmed) t;
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- Live identity: validation + normalization on write
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION external_references_identity_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.source_type IS NULL OR external_reference_trim(NEW.source_type) = '' THEN
    RAISE EXCEPTION 'external reference identity: source_type must not be empty'
      USING ERRCODE = 'P0001';
  END IF;
  IF NEW.external_identifier IS NULL OR external_reference_trim(NEW.external_identifier) = '' THEN
    RAISE EXCEPTION 'external reference identity: external_identifier must not be empty'
      USING ERRCODE = 'P0001';
  END IF;
  NEW.external_identifier := external_reference_normalize_identifier(NEW.external_identifier);
  -- The row stores the TRIMMED spelling, not the caller's: without this,
  -- a direct INSERT of ' publication ' would create a SECOND identity
  -- row keyed by the whitespace-wrapped value while the clean spelling
  -- keys the first — one DOI, two identities. The value is trimmed, not
  -- normalized further: source_type is a schema enum, and the enum values
  -- themselves are the gate ladder's business (see the header).
  NEW.source_type := external_reference_trim(NEW.source_type);
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER external_references_identity_guard
  BEFORE INSERT OR UPDATE ON external_references
  FOR EACH ROW EXECUTE FUNCTION external_references_identity_guard();

-- ---------------------------------------------------------------------------
-- Live identity: sync from external_reference object payloads
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION external_reference_identity_sync() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  obj_type text;
  src_type text;
  ext_id text;
  canon_url text;
BEGIN
  SELECT object_type INTO obj_type
    FROM scientific_objects WHERE id = NEW.object_id;
  IF NOT FOUND THEN
    -- Cannot happen through the FK; guard against misleading errors.
    RETURN NEW;
  END IF;
  IF obj_type <> 'external_reference' THEN
    RETURN NEW;
  END IF;

  -- Absent (or JSON null, or blank) means "no identity yet": a draft
  -- external reference may not know its identifier, and the gate ladder
  -- decides whether that omission is acceptable at this point. The
  -- pairing rule is identical in BOTH worlds (Go and SQL): both sides
  -- absent or blank is a valid draft, and a HALF-given pair — one side
  -- present, the other absent or blank — is a hard error. The
  -- application-level semantic checks (semantics.checkExternalReference)
  -- refuse the half-given pair before the write; this trigger refuses it
  -- for every other write path.
  src_type := NULLIF(external_reference_trim(NEW.payload->>'source_type'), '');
  ext_id := NULLIF(external_reference_trim(NEW.payload->>'external_identifier'), '');

  IF src_type IS NULL AND ext_id IS NULL THEN
    RETURN NEW;
  END IF;
  IF src_type IS NULL OR ext_id IS NULL THEN
    RAISE EXCEPTION 'external_reference object %: source_type and external_identifier must be given together (got source_type=%, external_identifier=%)',
      NEW.object_id, src_type, ext_id
      USING ERRCODE = 'P0001';
  END IF;

  canon_url := NEW.payload->>'canonical_url';
  IF canon_url IS NOT NULL AND external_reference_trim(canon_url) = '' THEN
    canon_url := NULL;
  END IF;

  -- One identity per normalized pair across all projects. The upsert
  -- fills a missing canonical_url from the first object that names it;
  -- an existing canonical_url is never overwritten by a later object.
  INSERT INTO external_references (source_type, external_identifier, canonical_url)
  VALUES (src_type, ext_id, canon_url)
  ON CONFLICT (source_type, external_identifier) DO UPDATE
     SET canonical_url = COALESCE(external_references.canonical_url, EXCLUDED.canonical_url);
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER scientific_object_versions_external_reference_identity
  AFTER INSERT ON scientific_object_versions
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION external_reference_identity_sync();

-- ---------------------------------------------------------------------------
-- Snapshots: metadata shape, accessed_at, derived hash
-- ---------------------------------------------------------------------------

-- snapshot_hash becomes derivable: NULL means "derive it server-side".
-- A supplied non-NULL hash is verified against the stored metadata bytes.
ALTER TABLE external_reference_snapshots
  ALTER COLUMN snapshot_hash DROP NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION external_reference_snapshots_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  derived text;
BEGIN
  IF jsonb_typeof(NEW.metadata) IS DISTINCT FROM 'object' THEN
    RAISE EXCEPTION 'external reference snapshot %: metadata must be a JSON object (got %)',
      NEW.id, jsonb_typeof(NEW.metadata)
      USING ERRCODE = 'P0001';
  END IF;
  -- clock_timestamp() is the actual current time: an observation stamped
  -- by the application's clock inside a long transaction must land, and
  -- now() would refuse it — now() is the TRANSACTION start, so any
  -- observation written after the transaction began compares as "future".
  -- The tolerance is zero in both directions: any stamp ahead of the
  -- database's actual clock is still refused, full stop.
  IF NEW.accessed_at IS NULL OR NEW.accessed_at > clock_timestamp() THEN
    RAISE EXCEPTION 'external reference snapshot %: accessed_at % records a future access',
      NEW.id, NEW.accessed_at
      USING ERRCODE = 'P0001';
  END IF;
  IF NEW.upstream_version IS NOT NULL AND external_reference_trim(NEW.upstream_version) = '' THEN
    RAISE EXCEPTION 'external reference snapshot %: upstream_version must not be empty when present',
      NEW.id
      USING ERRCODE = 'P0001';
  END IF;

  -- The hash pins the bytes the row actually holds: sha256 of the
  -- jsonb-canonical metadata text (jsonb normalizes key order and
  -- whitespace, so the digest is a stable function of the stored
  -- document — the same integrity contract as the object version
  -- payload hash, docs/21 §10). NULL derives; a supplied hash that
  -- disagrees is refused rather than silently rewritten, so a caller
  -- can never assert a hash the row does not have.
  derived := encode(digest(NEW.metadata::text, 'sha256'), 'hex');
  IF NEW.snapshot_hash IS NULL THEN
    NEW.snapshot_hash := derived;
  ELSIF NEW.snapshot_hash <> derived THEN
    RAISE EXCEPTION 'external reference snapshot %: snapshot_hash % does not match the stored metadata (sha256 is %)',
      NEW.id, NEW.snapshot_hash, derived
      USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER external_reference_snapshots_guard
  BEFORE INSERT ON external_reference_snapshots
  FOR EACH ROW EXECUTE FUNCTION external_reference_snapshots_guard();

-- The snapshot log is read per identity; 00011 never indexed the FK.
CREATE INDEX external_reference_snapshots_ref_idx
  ON external_reference_snapshots (external_reference_id);

-- ---------------------------------------------------------------------------
-- Citation/dependency endpoint admission
-- ---------------------------------------------------------------------------

-- The relation types that may point AT an external reference (docs/19
-- §3). The Go relation catalog declares the same set
-- (relationcatalog.ExternalRefTargetTypes); an integration test pins the
-- two copies together. Relations whose type is not declared here are
-- untouched — the catalog remains the authority on which relation types
-- exist, and the guard below only polices edges whose target IS an
-- external reference.
CREATE TABLE external_reference_relation_types (
  relation_type text PRIMARY KEY
);

INSERT INTO external_reference_relation_types (relation_type) VALUES
  ('references'),
  ('depends_on');

-- The policy set is SEALED: no ordinary write path may change which
-- relation types are admitted — a write path that could INSERT 'contains'
-- here would silently rewrite the citation/dependency policy of docs/19
-- §3 past the endpoint guard below. The set may only change through a
-- FUTURE MIGRATION that deliberately drops the seal first:
--   DROP TRIGGER external_reference_relation_types_seal
--     ON external_reference_relation_types;
-- change the rows, then re-create the trigger with the exact CREATE
-- TRIGGER statement below — and update the Go catalog
-- (relationcatalog.ExternalRefTargetTypes) and the drift test in the
-- same change, because the integration test pins the table to the
-- catalog.
-- +goose StatementBegin
CREATE FUNCTION external_reference_relation_types_seal() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'external_reference_relation_types is a sealed policy table: drop trigger external_reference_relation_types_seal in a migration, change the rows, then re-create it'
    USING ERRCODE = 'P0001';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER external_reference_relation_types_seal
  BEFORE INSERT OR UPDATE OR DELETE ON external_reference_relation_types
  FOR EACH ROW EXECUTE FUNCTION external_reference_relation_types_seal();

-- +goose StatementBegin
CREATE FUNCTION external_reference_relation_endpoint_guard() RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  rel_project uuid;
  src_type text;
  tgt_type text;
  tgt_project uuid;
  admitted boolean;
BEGIN
  SELECT so.object_type INTO src_type
    FROM scientific_object_versions sov
    JOIN scientific_objects so ON so.id = sov.object_id
   WHERE sov.id = NEW.source_object_version_id;
  IF NOT FOUND THEN
    RETURN NEW; -- the endpoint FKs police missing versions
  END IF;
  SELECT so.object_type, so.project_id INTO tgt_type, tgt_project
    FROM scientific_object_versions sov
    JOIN scientific_objects so ON so.id = sov.object_id
   WHERE sov.id = NEW.target_object_version_id;
  IF NOT FOUND THEN
    RETURN NEW;
  END IF;

  IF src_type = 'external_reference' THEN
    RAISE EXCEPTION 'relation %: an external_reference object may not be a relation source in V1 (claims about external sources are evidence assertions, not relations)',
      NEW.relation_type
      USING ERRCODE = 'P0001';
  END IF;
  IF tgt_type <> 'external_reference' THEN
    RETURN NEW; -- edges between ordinary objects are not this guard's business
  END IF;

  SELECT true INTO admitted
    FROM external_reference_relation_types
   WHERE relation_type = NEW.relation_type;
  IF admitted IS NULL THEN
    RAISE EXCEPTION 'relation %: the only types that may target an external reference are references (citation) and depends_on (dependency)',
      NEW.relation_type
      USING ERRCODE = 'P0001';
  END IF;

  -- The external reference object the edge points at must live in the
  -- relation's project: a citation is a claim about the project's
  -- research (the same same-project rule the 00040 knowledge guard
  -- enforces).
  SELECT project_id INTO rel_project FROM relations WHERE id = NEW.relation_id;
  IF tgt_project IS DISTINCT FROM rel_project THEN
    RAISE EXCEPTION 'relation %: target external_reference object belongs to a different project',
      NEW.relation_type
      USING ERRCODE = 'P0001';
  END IF;
  RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER relation_versions_external_reference_endpoints
  AFTER INSERT ON relation_versions
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION external_reference_relation_endpoint_guard();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
