-- +goose Up
-- Credit attribution and credit disputes (T0809; docs/13 §2, §3).
--
-- # Two halves, and why they are stored differently
--
-- docs/13 §2: "Asset/Finding/Release 可声明 creators/major contributors/
-- method designer 等高层 credit。它可以被纠正，但 correction 作为新 event，
-- 旧 attribution 和 dispute history 保留。" and docs/13 §3: "Contributor 可
-- 发起 dispute，附带 ledger evidence；Maintainer/Organization governance
-- 处理。不得直接改原事件。Dispute 本身不参与公共 reputation 排名直到
-- resolution."
--
-- So there are two records with two different shapes, and the shapes are
-- not interchangeable:
--
--   - a HIGH-LEVEL CREDIT DECLARATION is never revised. A correction is a
--     new declaration, and the one it corrects stays readable ("旧
--     attribution ... 保留"). That is an append-only chain, exactly like
--     the rights-holder chain 00082 built for the same sentence's other
--     half, so it takes the same 00014 row guard and 00015 statement
--     guard: credit_attribution_statements is the declaration and
--     credit_attribution_parties its items, and NEITHER is ever updated or
--     deleted by any path.
--
--   - a DISPUTE has a current state (open → resolved | rejected) and a
--     history. The current state is 00011's credit_disputes row, updated in
--     place when governance closes it — that table is on
--     tests/integration/append_only_test.go's "mutable by design" list and
--     closing a dispute by updating that row is the design, not a hole. The
--     history is NOT the row: it is the pair of domain events
--     credit.dispute_opened / credit.dispute_resolved
--     (specs/events/event-types.yaml), which the Contribution Ledger
--     projects into the append-only contribution_events (00014's trigger).
--     "dispute history 可查" therefore reads from the ledger, and the
--     ledger row is the copy nothing can rewrite.
--
-- The split is what docs/13 §2's "correction 作为新 event，旧 attribution 和
-- dispute history 保留" asks for: the immutable half is append-only, the
-- mutable half is state, and closing a dispute writes an EVENT rather than
-- editing a record of one.
--
-- # What this migration adds to credit_disputes: the state machine
--
-- 00011 created credit_disputes with no guard at all, so "结案就地更新" was
-- also "任何 UPDATE 都行": a resolver could rewrite the claim it was
-- resolving, re-open a closed dispute, or stamp a resolution without a
-- state. The guard below pins the one transition the design has —
-- open → resolved | rejected, terminal — and pins the identity and the
-- claim of the row for every write path. It does NOT make the table
-- append-only: state, resolution and resolved_at are exactly the three
-- columns it leaves open, which is what "mutable by design" means here.
-- The distinction is the point of this paragraph: the guard makes the
-- current-state table honest, it does not turn it into history.
--
-- # Two target refs, one canonical shape
--
-- Both tables address what they are about with the "kind:value" text ref
-- this repository already spells in research_asset_versions.origin_refs
-- (00064) and in the Contribution Ledger's object_refs
-- (internal/contribution/ledger.go, RefTexts): the kind names the entity
-- class and the value its identity. The three kinds are docs/13 §2's own
-- list, nothing wider:
--
--   - "asset:<pid>"      — the asset's persistent identifier, the identity a
--                          citation resolves (00064), not its row id;
--   - "release:<uuid>"   — a releases row id;
--   - "finding:<uuid>"   — a findings row id (00057; its primary key is the
--                          finding's scientific_object_versions row).
--
-- An unknown kind is refused by the CHECK rather than by a reader's
-- judgement, and target_ref is stored with its kind attached so no row can
-- hold an id without saying what the id names — the same rule 00082 applies
-- to party ids.
--
-- There is deliberately NO foreign key to the target: one column cannot
-- reference three tables, and the alternative (three nullable columns plus
-- a CHECK that exactly one is set) would put the kind in the database twice
-- with two chances to disagree. Integrity is the writing command's
-- business, and it resolves the ref against the table its kind names
-- BEFORE it writes — the discipline internal/application/assetrights
-- established for asset_rights_holder_events (00082's header).
--
-- # The credit role vocabulary is two values, and the omission is deliberate
--
-- docs/13 §2 names three roles and leaves the list open: "creators/major
-- contributors/method designer 等". The open end is a product decision
-- about scientific semantics (who counts as a major contributor; whether
-- the 13 contribution roles of docs/04 §4 — which docs/04 §4 itself calls
-- "不是身份，不赋权，不计固定分值" — may double as credit titles). That
-- decision is recorded as an open L3 in tasks/decisions.md and is NOT made
-- here. This table admits exactly the two roles the task requires —
-- creator, major_contributor — and refuses every other value, INCLUDING
-- "method_designer" and anything the 等 could later name: fail closed on
-- the undecided end of the list, so no unratified credit title can be
-- written while the question is open. Widening the CHECK is the migration
-- that ratifies one.
--
-- The first role is not invented: "creator" is already a credit role in
-- this schema (asset_version_parties, 00082, from docs/11 §6), and this
-- table spells it identically so the two surfaces cannot disagree about
-- the word. "major_contributor" is the task requirement's own term.
--
-- The platform stores DECLARATIONS, never a computed ranking: nothing here
-- derives a credit from the ledger, from a count, or from any other fact
-- (docs/13 §4 forbids a single reputation score, §6 refuses raw counts as
-- proxies, CLAUDE.md §9 invariant 13 forbids a Truth Score). What lands is
-- what a person declared, attributable to the person who declared it.
--
-- # No party foreign key, and no project column on the items
--
-- credit_attribution_parties carries the same (party_kind, party_id) pair
-- asset_version_parties does, for the same reason (00082's header): the id
-- points into users for one kind and organizations for the other, so it
-- cannot be a single-column foreign key, and the writing command resolves
-- it against the table its kind names. A credit names a person or an
-- institution; 'project' is not a credit role here (docs/13 §2 names
-- people-shaped roles, and the originating project is already a column on
-- research_assets — storing it twice would give one fact two homes).

CREATE TABLE credit_attribution_statements (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  -- The research boundary the declaration belongs to. NOT NULL: a credit
  -- declaration is a governance act inside a project, and the project is
  -- what the actor's membership — and therefore the authorization — is
  -- resolved against.
  project_id uuid NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  -- target_kind is carried as a column AND inside target_ref. The
  -- duplication is deliberate and is what makes the pair checkable: the
  -- CHECK below refuses a row whose two spellings disagree, so a reader may
  -- use either without trusting the other.
  target_kind text NOT NULL CHECK (target_kind IN ('asset', 'release', 'finding')),
  target_ref text NOT NULL,
  CONSTRAINT credit_attribution_statements_target_ref_kind
    CHECK (target_ref LIKE target_kind || ':%' AND length(target_ref) > length(target_kind) + 1),
  -- ordinal orders the declarations about one target. It is a per-target
  -- sequence rather than a timestamp, for the reason 00082 gave the
  -- rights-holder chain the same column: the chain's whole meaning is an
  -- order, two declarations in one transaction share now() exactly, and
  -- "the declaration a correction corrects" would otherwise be left to a
  -- tie-break nobody declared. The writing command takes a row lock on the
  -- target before reading the current maximum, so the sequence is
  -- serialized per target rather than computed in a race.
  ordinal integer NOT NULL CHECK (ordinal >= 1),
  -- recorded_by is the actor whose request declared the credit. NOT NULL: a
  -- credit with no attributable declarer would be an anonymous assertion
  -- about someone else's authorship (00082's rule for the same column).
  recorded_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  recorded_at timestamptz NOT NULL DEFAULT now(),
  -- One declaration per position: two rows at one ordinal would make "the
  -- declaration after this one" ambiguous.
  UNIQUE (project_id, target_kind, target_ref, ordinal)
);

COMMENT ON TABLE credit_attribution_statements IS
  'One high-level credit declaration for one target (docs/13 §2: Asset/Finding/Release 可声明 creators/major contributors/method designer 等高层 credit). Append-only: "它可以被纠正，但 correction 作为新 event，旧 attribution ... 保留" — a correction is a NEW row at the next ordinal, and every earlier declaration stays readable. Never updated, never deleted, by any path (00014 row guard + 00015 statement guard).';

CREATE TABLE credit_attribution_parties (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  statement_id uuid NOT NULL REFERENCES credit_attribution_statements(id) ON DELETE RESTRICT,
  -- The admitted vocabulary is two values, deliberately (see the header):
  -- widening it is a product decision, not a code change.
  role text NOT NULL CHECK (role IN ('creator', 'major_contributor')),
  party_kind text NOT NULL CHECK (party_kind IN ('user', 'organization')),
  party_id uuid NOT NULL,
  -- position is the order the declaring caller sent this party in, stored
  -- rather than reconstructed, for the reason 00082 gives the identical
  -- column: a declaration is a list, and an unordered set does not
  -- reproduce one.
  position integer NOT NULL CHECK (position >= 0),
  -- One party per role per position, and no party twice inside one role: a
  -- declaration naming the same person twice is a malformed list, not a
  -- stronger claim.
  UNIQUE (statement_id, role, party_id),
  UNIQUE (statement_id, role, position)
);

COMMENT ON TABLE credit_attribution_parties IS
  'The parties one credit declaration names, per role (docs/13 §2). Append-only, and scoped to its declaration: the current credit for a target is the party rows of that target''s greatest-ordinal statement (credit_attribution_statements), while every earlier declaration''s rows remain readable. Never updated, never deleted, by any path.';

CREATE TRIGGER credit_attribution_statements_append_only
  BEFORE UPDATE OR DELETE ON credit_attribution_statements
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER credit_attribution_statements_no_truncate
  BEFORE TRUNCATE ON credit_attribution_statements FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER credit_attribution_parties_append_only
  BEFORE UPDATE OR DELETE ON credit_attribution_parties
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER credit_attribution_parties_no_truncate
  BEFORE TRUNCATE ON credit_attribution_parties FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE INDEX credit_attribution_statements_target_ordinal
  ON credit_attribution_statements (project_id, target_kind, target_ref, ordinal DESC);

CREATE INDEX credit_attribution_parties_statement
  ON credit_attribution_parties (statement_id, role, position);

-- The dispute state machine. It is a targeted guard rather than the
-- append-only pair, because credit_disputes is a CURRENT-STATE table by
-- design (tests/integration/append_only_test.go, "Exempt tables (mutable by
-- design)"): state, resolution and resolved_at are the three columns this
-- function lets a write touch, and it lets it touch them exactly once per
-- dispute.
--
-- A row is BORN open: a dispute that starts resolved would have no
-- opening, so the record of who raised it — the credit.dispute_opened
-- event — could not exist, and the ledger would show a resolution of a
-- dispute that was never opened.
-- +goose StatementBegin
CREATE FUNCTION credit_dispute_state_guard() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.state <> 'open' THEN
      RAISE EXCEPTION 'credit_disputes: a dispute is born open (state=%, credit_disputes is INSERT-immutable in its state)', NEW.state;
    END IF;
    IF NEW.resolution IS NOT NULL OR NEW.resolved_at IS NOT NULL THEN
      RAISE EXCEPTION 'credit_disputes: a newly opened dispute carries no resolution (credit_disputes is INSERT-immutable in resolution/resolved_at)';
    END IF;
    RETURN NEW;
  END IF;

  -- The identity of the dispute and the claim it makes are immutable:
  -- rewriting either would rewrite what the recorded open event is about
  -- (docs/13 §3 "不得直接改原事件" — a resolution answers the claim, it
  -- never edits it).
  IF NEW.id IS DISTINCT FROM OLD.id
     OR NEW.project_id IS DISTINCT FROM OLD.project_id
     OR NEW.opened_by IS DISTINCT FROM OLD.opened_by
     OR NEW.target_ref IS DISTINCT FROM OLD.target_ref
     OR NEW.claim IS DISTINCT FROM OLD.claim
     OR NEW.opened_at IS DISTINCT FROM OLD.opened_at THEN
    RAISE EXCEPTION 'credit_disputes: the dispute identity, its target and its claim are immutable (credit_disputes is UPDATE-immutable in id/project_id/opened_by/target_ref/claim/opened_at)';
  END IF;

  -- Terminal means terminal: a closed dispute is not re-opened and not
  -- re-decided. A later claim is a new dispute, which is the same rule
  -- docs/13 §2 applies to attribution corrections.
  IF OLD.state <> 'open' THEN
    RAISE EXCEPTION 'credit_disputes: a closed dispute is terminal (state % cannot be updated; credit_disputes is UPDATE-immutable once closed)', OLD.state;
  END IF;
  IF NEW.state NOT IN ('resolved', 'rejected') THEN
    RAISE EXCEPTION 'credit_disputes: the only transition is open -> resolved|rejected (credit_disputes is UPDATE-immutable in state, got %)', NEW.state;
  END IF;
  -- A close states its resolution: "resolved" with no reasoning would be a
  -- decision nobody can read, and resolved_at is the instant the decision
  -- was taken — the pair travels with the state or the row is not closed.
  IF NEW.resolution IS NULL OR btrim(NEW.resolution) = '' THEN
    RAISE EXCEPTION 'credit_disputes: closing a dispute requires a resolution (credit_disputes is UPDATE-immutable in resolution)';
  END IF;
  IF NEW.resolved_at IS NULL THEN
    RAISE EXCEPTION 'credit_disputes: closing a dispute requires resolved_at (credit_disputes is UPDATE-immutable in resolved_at)';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER credit_disputes_state_guard
  BEFORE INSERT OR UPDATE ON credit_disputes
  FOR EACH ROW EXECUTE FUNCTION credit_dispute_state_guard();

-- The dispute reads: one project's disputes, newest first, and the open
-- ones for a target. Both are indexed because the dispute list is a
-- governance surface an operator scans, not a point lookup.
CREATE INDEX credit_disputes_project_opened
  ON credit_disputes (project_id, opened_at DESC);

CREATE INDEX credit_disputes_target
  ON credit_disputes (target_ref, opened_at DESC);

-- +goose Down
DROP TRIGGER credit_disputes_state_guard ON credit_disputes;
DROP FUNCTION credit_dispute_state_guard();
DROP INDEX credit_disputes_target;
DROP INDEX credit_disputes_project_opened;
DROP INDEX credit_attribution_parties_statement;
DROP INDEX credit_attribution_statements_target_ordinal;
DROP TRIGGER credit_attribution_parties_no_truncate ON credit_attribution_parties;
DROP TRIGGER credit_attribution_parties_append_only ON credit_attribution_parties;
DROP TRIGGER credit_attribution_statements_no_truncate ON credit_attribution_statements;
DROP TRIGGER credit_attribution_statements_append_only ON credit_attribution_statements;
DROP TABLE credit_attribution_parties;
DROP TABLE credit_attribution_statements;
