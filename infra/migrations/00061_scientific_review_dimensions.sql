-- +goose Up
-- Scientific Review model (task T0404): per-dimension review records on
-- pull requests (docs/09 §5: Scientific Review and Integrity Review are
-- recorded per dimension, "Review 状态可以按维度记录，不用单一 approve
-- 抹平所有差异"; docs/43: "Scientific/Integrity review 各自状态").
--
-- What changes here, and why each piece:
--
--   1. review_kind narrowed to the two documented dimensions
--      ('scientific','integrity'). The base schema (00009) also admitted
--      'rights' and 'ip', but docs/09 §5 defines exactly two review
--      dimensions and no writer ever existed — zero rows are migrated,
--      and a later task (T0604 routing) can re-widen the vocabulary in
--      its own forward migration if a third dimension is spec'd.
--
--   2. decision vocabulary becomes ('approved','changes_requested',
--      'comment') — the task requirement's exact three decisions. The
--      base schema's third token was 'commented'; no row ever carried
--      it (the reviews table had no writer before this task), so the
--      rename costs no data migration.
--
--   3. reviewed_state_id pins the review to the EXACT proposed head the
--      reviewer evaluated: a review is a judgment about one state, not
--      about a moving target. The adapter always fills it from the PR's
--      current proposed_state_id inside the submission transaction, so
--      a review of the current head is one row; after the author
--      updates the head and re-requests review, the next round's
--      decisions are new rows on the new head, and stale decisions can
--      never leak into the new round's projection.
--
--   4. responsibility records which scientific responsibility the
--      reviewer acted under (docs/04 §3: responsibility is for review
--      routing, separate from access roles). The label is resolved by
--      the application's reviewer-responsibility hook (T0604 lands the
--      rule-based resolver); empty means no responsibility label was
--      resolved. Bounded by the domain layer, not here.
--
--   5. UNIQUE(pull_request_id, reviewer_id, review_kind,
--      reviewed_state_id): one person records ONE decision per
--      dimension per head — the acceptance "一人不同 review kind 可
--      记录" encoded as a database fact (different kinds and later
--      heads are never blocked; a duplicate decision about the same
--      head is refused instead of silently double-counted). A review of
--      a state is final: the machine's only path forward after a
--      decision is the author updating the head (docs/43).
--
-- The backfill path for reviewed_state_id keeps the migration valid on
-- a non-empty table (add nullable -> backfill from the owning PR ->
-- set NOT NULL), even though no production row exists.

ALTER TABLE reviews
  DROP CONSTRAINT reviews_review_kind_check,
  ADD CONSTRAINT reviews_review_kind_check
    CHECK (review_kind IN ('scientific','integrity')),
  DROP CONSTRAINT reviews_decision_check,
  ADD CONSTRAINT reviews_decision_check
    CHECK (decision IN ('approved','changes_requested','comment'));

ALTER TABLE reviews
  ADD COLUMN reviewed_state_id uuid REFERENCES project_states(id) ON DELETE RESTRICT;

UPDATE reviews r
SET reviewed_state_id = pr.proposed_state_id
FROM pull_requests pr
WHERE pr.id = r.pull_request_id;

ALTER TABLE reviews
  ALTER COLUMN reviewed_state_id SET NOT NULL;

ALTER TABLE reviews
  ADD COLUMN responsibility text NOT NULL DEFAULT '',
  ADD CONSTRAINT reviews_reviewer_kind_state_key
    UNIQUE (pull_request_id, reviewer_id, review_kind, reviewed_state_id);

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
