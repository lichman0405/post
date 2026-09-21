-- +goose Up
-- The record of one search and the answer it produced (T0906).
--
-- docs/22 §8 (Search API) is one sentence long and it is the whole reason
-- this table exists: "接受 raw query + structured optional filters；服务端保存
-- query plan、selected entity ids、answer citations". Before this file, nothing
-- in the tree saved a search at all — POST /search did not exist, and the
-- contract already carries POST /search/{searchId}:start-project
-- (specs/api/openapi.yaml), a route that cannot be served without a record to
-- address a searchId to. The three things the spec names are the columns
-- below: the plan, the selected refs, the citations.
--
-- # A record, not a cache
--
-- The row is written once, when the answer is produced, and is never updated
-- and never read to answer the same question again. Reusing a stored answer
-- would publish a claim at its old age: the ranking is a function of the
-- corpus AS OF NOW (ranking/doc.go), so an answer whose sources have since
-- been superseded is a statement the platform would no longer make. What the
-- row is FOR is the two things that need the search to still exist after the
-- response was sent: the citation trail (an answer cites entity versions, and
-- a reader must be able to see which search produced a citation — docs/54
-- ranks a fabricated or unauthorized citation as a top-severity scenario), and
-- the start-project flow, which resumes a search's sources as a draft
-- research context rather than re-running the search under a different corpus.
--
-- # citations ⊆ selected_refs, enforced by the database
--
-- The package's central invariant is that an answer may cite only what
-- retrieval returned (internal/search/answer/doc.go, docs/22 §8's last
-- clause, docs/54 #7). The Go guard refuses a document that cites anything
-- else, and this CHECK is the second, independent refusal: a row whose
-- citations name a ref the search did not return cannot be inserted by ANY
-- writer, including one added later that has not read the guard. Containment
-- is the whole of it: it forbids a citation outside selected_refs and says
-- nothing about how many there are, so an empty citation list satisfies it on
-- any search — which is correct, because an empty list is exactly what a
-- fallback means (see the citations column below).
--
-- # What is jsonb and why
--
-- plan, signals and answer are documents, and they are stored as documents
-- rather than decomposed into rows. None of them is a fact the database has a
-- question about: nothing joins to a plan's narrowing or to a signal report,
-- and the answer is the answer layer's own canonical document — its shape is
-- that package's schema (internal/search/answer/schema.go, versioned by
-- answer_version), not this schema's. Decomposing them here would give the
-- platform a second, drifting definition of what an answer is. The two
-- columns that DO carry relational meaning — the selected refs and the
-- citations — are text[] precisely because the invariant above is a statement
-- about them as sets of refs.
--
-- plan is nullable and signals/answer are not: a deployment with no planner
-- configured still searches (planner.New requires a provider — planning's
-- absence changes what is READ), so "no plan" is a real state of a real
-- search; a search that ran always has a signal report and always has an
-- answer document, even when that answer is the structured fallback.
--
-- # No index
--
-- Reads of this table are by primary key (one searchId at a time), and that
-- is the only read the contract has. An index for a read that does not exist
-- is a write cost paid on every search for nothing; the catalog fixture
-- (tests/integration/catalog_test.go) requires the explicit index set to
-- match exactly, so an index added here would also be a schema change nobody
-- asked for.
CREATE TABLE search_records (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- The actor whose scope the search ran under. The record belongs to the
  -- person who asked: the scope resolved from this id decided which rows were
  -- readable (internal/search/scope.go is the only constructor of it), so an
  -- answer read back under a different actor would be a claim made about
  -- rows that actor may not read.
  actor_id uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  query text NOT NULL CHECK (btrim(query) <> ''),
  -- The structured filters the caller sent, verbatim and uninterpreted:
  -- an unknown shape is refused by the planner, not repaired here.
  filters jsonb,
  -- The query plan, when a planner ran.
  plan jsonb,
  -- The retrieval's own SignalReport[] — which signals ran, which were
  -- skipped and why. It is stored because it is what the answer's coverage
  -- limitations are derived from: without it, "the vector signal did not run"
  -- is a sentence in a saved answer that nothing can check.
  signals jsonb NOT NULL,
  -- The entity version refs the result selected, in rank order, in the
  -- retrieval's own "<kind>:<identity>@<version>" spelling. This is the set
  -- the answer may cite and the set start-project resumes.
  selected_refs text[] NOT NULL,
  -- The refs the answer cites: the subset of selected_refs the written summary
  -- leans on. It is non-empty exactly when a MODEL wrote that summary: the
  -- answer schema requires an answered document to cite at least one ref
  -- (internal/search/answer/schema.go), and every fallback is constructed
  -- with no citations at all (generator.go's fallback), so a fallback row's
  -- citations are always empty. Empty is a real state (a fallback over a
  -- result the model never saw).
  citations text[] NOT NULL,
  -- The answer document (internal/search/answer, answer_version 1).
  answer jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  -- The invariant, in the database. See the header.
  CHECK (citations <@ selected_refs)
);
