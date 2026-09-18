-- +goose Up
-- Contribution Ledger projection (T0807, docs/13).
--
-- contribution_events has existed since 00011 with no writer anywhere in
-- the repository: the table, its append-only guard (00014/00015) and
-- docs/13 §1's list of what a contribution event carries were all in place,
-- and nothing had ever produced a row. T0807 is the projection that turns
-- the domain event log (research_events) into ledger rows, and this
-- migration adds the three things that projection needs and the two tables
-- cannot express.
--
-- 1. The source event: contribution_events.research_event_id.
--
-- A ledger row is the projection of exactly ONE domain event, and the
-- column pins which (docs/13 §2/§3: a correction is a NEW event and the
-- original is never edited; CLAUDE.md §9 invariant 14). The partial unique
-- index below is what makes the projection IDEMPOTENT: a re-run — the
-- projection is a rebuildable read of the append-only log, so re-running it
-- is normal, not exceptional — inserts ON CONFLICT DO NOTHING and adds
-- nothing, exactly as the outbox publish step does one table over
-- (00046: research_events.outbox_event_id + research_events_outbox_event_uniq,
-- the shape this one is copied from).
--
-- The index is PARTIAL for 00046's own reason: NULL is never a conflict, so
-- a row written by a path that is not the projection (a direct ledger write
-- with no source event) stays legal, and every projection-written row is
-- non-NULL. The projection never writes a NULL source.
--
-- The FK is ON DELETE RESTRICT like every other reference to history in
-- this schema, and it points at research_events rather than at outbox_events
-- because research_events is the append-only log the projection reads:
-- release.published, research_asset.version_published and
-- knowledge.version_published are written into it DIRECTLY (the persistence
-- stores' RecordResearchEvent), with no outbox row at all.
--
-- 2. via — the channel the contribution arrived through (docs/13 §1).
--
-- docs/13 §1 names seven things every contribution event carries; six of
-- them are columns of 00011 already (actor person id, affiliation at time,
-- project/object refs, role tags, timestamp, accepted/released context) and
-- the seventh — "via agent/client" — was never a column. This adds it.
--
-- The vocabulary is the CHANNEL one, not the authentication one:
-- state_commits.via (00004: web, api, mcp, claude_code, git_compat, system;
-- internal/domain/state.go, StateVia) is "which channel did this write come
-- through", while audit_log.via (00012: session, password, oidc, internal;
-- internal/domain/audit.go) is "how was the actor authenticated". docs/13 §1
-- says agent/client — that is the channel.
--
-- No CHECK, deliberately: the vocabulary lives in Go (the domain constant
-- list), and a CHECK here would be a second definition of it that drifts.
-- This is the convention four existing migrations state verbatim for their
-- own vocabularies (00064, 00066, 00067, 00045), and role_codes follows it
-- too — docs/04 §4's thirteen contribution roles are a Go-side vocabulary
-- with no database CHECK either.
--
-- NULL is legal and means "the source event carried no channel" — never a
-- guessed default. A guessed 'api' would read exactly like a recorded fact.
--
-- 3. The envelope chain: outbox_events.via and research_events.via.
--
-- A ledger row's via may not be re-derived from the event payload: the
-- envelope rule 00046 states for the publish step is verbatim "the publisher
-- must copy them from the outbox row, never re-derive them from the
-- payload", and the reason it gives (a private event must not be publishable
-- as a public one) is about envelope columns in general — the payload is
-- producer-written data about the event, the envelope column is the record
-- the write path made. A ledger column that read payload->>'via' would be
-- trusting the same data the rule refuses to trust one table earlier.
--
-- So the channel travels as a column at every hop: the outbox row carries
-- it from the write path, the publisher copies it into the research event,
-- and the projection copies it into the ledger row that names that event.
-- Both columns land here so the chain has somewhere to live.
--
-- HONEST STATE OF THE CHAIN, since a column nothing writes is worth naming:
-- the two write paths that would set outbox_events.via belong to
-- internal/events (the outbox recorder's INSERT and the publisher's
-- envelope copy), which is outside T0807's write scope. Until that
-- one-field change lands, research_events.via is NULL on tonight's writes
-- and every projected via is NULL — which is the truthful answer ("the
-- source event carries no channel"), not a fabricated one. The projection
-- reads the column and copies it verbatim; it never invents a value, and
-- the moment the recorder sets it the ledger fills in with no change here.
--
-- Nothing else changes. contribution_events keeps its shape otherwise (00011
-- designed it; T0807 does not), its append-only guard already covers every
-- new write path (00014/00015: the guard is on the table, not on a
-- statement, so INSERT-only is enforced for this projection exactly as for
-- anything else), and no aggregate, counter, weight or rank column is added
-- — docs/13 §4 forbids a single score, docs/13 §6 forbids raw counts as
-- quality proxies, and CLAUDE.md §9 invariant 13 forbids a Truth Score. The
-- ledger records facts; a Reputation Profile derives from them elsewhere.

ALTER TABLE contribution_events
  ADD COLUMN research_event_id uuid REFERENCES research_events(id) ON DELETE RESTRICT,
  ADD COLUMN via text;

COMMENT ON COLUMN contribution_events.research_event_id IS
  'The research_events row this ledger row projects (T0807). The partial unique index contribution_events_research_event_uniq makes the projection idempotent: a re-run of the projection conflicts on this key and inserts nothing. NULL for a ledger row written directly, with no source event — the projection never writes one.';

COMMENT ON COLUMN contribution_events.via IS
  'The channel the contribution arrived through (docs/13 §1 "via agent/client"): the state_commits.via vocabulary (00004: web, api, mcp, claude_code, git_compat, system), NOT the audit_log.via authentication vocabulary (00012). Defined in Go (internal/domain/state.go StateVia), no CHECK here by the 00064/00066/00067/00045 convention. Copied verbatim from the source research_event''s via column, which the publisher copies from the outbox row (00046: envelope columns are copied, never re-derived from the payload). NULL means the source event carries no channel — never a guessed default.';

-- The projection dedupe: one ledger row per source event, ever.
CREATE UNIQUE INDEX contribution_events_research_event_uniq
  ON contribution_events (research_event_id)
  WHERE research_event_id IS NOT NULL;

-- Two columns 00011 created without a producer now have one, and their
-- content is pinned here rather than left to whoever reads them next:
-- event_type is the domain event the row projects (the vocabulary of
-- specs/events/event-types.yaml, the machine-readable event list), and
-- object_refs is an array of canonical "kind:value" strings naming the
-- subject entities of that event — the same text form research_asset_versions
-- .origin_refs uses (00064), with the kinds this subsystem defines.
COMMENT ON COLUMN contribution_events.event_type IS
  'The domain event this ledger row projects: a name from specs/events/event-types.yaml (the machine-readable event vocabulary), e.g. scientific_object.version_created, pull_request.merged, contribution.accepted. docs/13 §1 lists the ledger''s event categories in prose and ends it with 等; the spec''s list is the closed, checkable one, and the row also pins its exact source in research_event_id. Written by the T0807 projection.';

COMMENT ON COLUMN contribution_events.object_refs IS
  'The subject entities of the projected event, as an array of canonical "kind:value" strings (the text form research_asset_versions.origin_refs uses; the kinds are internal/contribution''s LedgerRefKind set). Read from the event''s own payload — an id the payload does not carry contributes no ref — and never from a later read of current state: the ledger records what the event said. Written by the T0807 projection.';

ALTER TABLE outbox_events
  ADD COLUMN via text;

COMMENT ON COLUMN outbox_events.via IS
  'The channel the write that produced this event arrived through, in the state_commits.via vocabulary (00004) — the envelope column the publisher copies verbatim into research_events.via (00046: envelope columns are copied from the outbox row, never re-derived from the payload). NULL means the writing path did not record a channel; the outbox recorder (internal/events) is the path that sets it.';

ALTER TABLE research_events
  ADD COLUMN via text;

COMMENT ON COLUMN research_events.via IS
  'The channel the write that produced this event arrived through, in the state_commits.via vocabulary (00004), copied verbatim from the outbox row it was published from (00046). Read by the Contribution Ledger projection (T0807) into contribution_events.via — the ledger never re-derives a channel from an event payload. NULL means no channel was carried.';

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
