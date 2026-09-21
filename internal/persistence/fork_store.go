package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// ForkStore is the production forks.StorePort adapter over PostgreSQL.
//
// Its two compare-and-swaps follow the shape
// internal/persistence/main_freeze_store.go established: the atomic
// statement decides, and a read is issued only to EXPLAIN an outcome that
// has already been decided —
//
//   - ClaimFork wins or loses the lineage insert on 00086's
//     UNIQUE (parent_project_id, forked_by), so two concurrent fork
//     requests can never both write a lineage row, an audit row or an
//     event; the loser reads the winner's row and writes nothing.
//   - SetForkSHA moves the one-shot cell (forked_sha NULL → value) and
//     reports whether this call was the one that moved it.
//
// There is no read-then-write anywhere: the reads below either run inside
// the transaction whose statements already decided the outcome or explain
// an outcome that a statement outside this store has already decided
// (PersonalProjectCreator explains the fork service's lost slug insert).
type ForkStore struct {
	pool *pgxpool.Pool
}

// NewForkStore builds the store on pool. The pool may be lazy (OpenLazy):
// the API keeps starting while PostgreSQL is down.
func NewForkStore(pool *pgxpool.Pool) *ForkStore {
	return &ForkStore{pool: pool}
}

// forkColumns is the lineage row's read shape, shared by every read here
// so the Fork projection exists once.
const forkColumns = `fork_project_id::text, parent_project_id::text, forked_by::text,
	relation_type, source_branch_id::text, fork_branch_id::text, forked_sha, created_at`

// ClaimFork implements forks.StorePort.
//
// One row back — this call won the lineage insert: the audit row and the
// branch.created event are written on the same transaction, so the fork,
// the record of who made it and the event other services react to are one
// fact (docs/53; a fork without its audit row would be exactly the
// unrecorded state change the rule forbids), and a failed claim leaves
// nothing behind.
//
// Zero rows — the (parent, actor) pair already had a fork. One read tells
// the caller which row it is; NOTHING is written, no second audit row and
// no second event, which is what makes a repeated fork request a no-op by
// construction rather than by remembering request keys in a ledger.
func (s *ForkStore) ClaimFork(ctx context.Context, req forks.ClaimRequest) (forks.Fork, bool, error) {
	forkProjectID, err := textUUID(req.ForkProjectID)
	if err != nil {
		return forks.Fork{}, false, fmt.Errorf("%w: fork project id: %v", forks.ErrStore, err)
	}
	parentProjectID, err := textUUID(req.ParentProjectID)
	if err != nil {
		return forks.Fork{}, false, fmt.Errorf("%w: parent project id: %v", forks.ErrStore, err)
	}
	actorID, err := textUUID(req.ActorID)
	if err != nil {
		return forks.Fork{}, false, fmt.Errorf("%w: forking actor id: %v", forks.ErrStore, err)
	}
	sourceBranchID, err := textUUID(req.SourceBranchID)
	if err != nil {
		return forks.Fork{}, false, fmt.Errorf("%w: source branch id: %v", forks.ErrStore, err)
	}
	forkBranchID, err := textUUID(req.ForkBranchID)
	if err != nil {
		return forks.Fork{}, false, fmt.Errorf("%w: fork branch id: %v", forks.ErrStore, err)
	}
	// One correlation id for the whole claim: the request's when the
	// observability middleware attached one, else a fresh one — the audit
	// row, the outbox event and the operator's trace share it.
	correlationID := ""
	if info, ok := domain.RequestInfoFrom(ctx); ok {
		correlationID = info.CorrelationID
	}
	if correlationID == "" {
		id, err := observability.NewCorrelationID()
		if err != nil {
			return forks.Fork{}, false, fmt.Errorf("%w: fork correlation id: %v", forks.ErrStore, err)
		}
		correlationID = id.String()
	}

	var out forks.Fork
	var inserted bool
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`INSERT INTO project_forks (fork_project_id, parent_project_id, forked_by, source_branch_id, fork_branch_id)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (parent_project_id, forked_by) DO NOTHING
			 RETURNING `+forkColumns,
			forkProjectID, parentProjectID, actorID, sourceBranchID, forkBranchID)
		claimed, err := scanFork(row)
		switch {
		case err == nil:
			visibility, err := forkEventVisibility(ctx, tx, forkProjectID, parentProjectID)
			if err != nil {
				return err
			}
			audit := req.Audit
			if audit.CorrelationID == "" {
				audit.CorrelationID = correlationID
			}
			if err := appendAudit(ctx, sqlc.New(tx), audit); err != nil {
				return err
			}
			if err := recordForkEvent(ctx, tx, claimed, visibility, correlationID); err != nil {
				return err
			}
			out, inserted = claimed, true
			return nil
		case errors.Is(err, pgx.ErrNoRows):
			// The claim lost: this actor already forked this project. The
			// winner's row is the answer; nothing is written here.
			existing, ok, err := findFork(ctx, tx, parentProjectID, actorID)
			if err != nil {
				return err
			}
			if !ok {
				// Unreachable under READ COMMITTED — the insert loses only
				// to a committed lineage row for this very pair, and
				// project_forks rows are never deleted (the guard trigger
				// refuses DELETE) — so a missing row here means the table
				// changed under rules this store does not know about.
				// Refuse rather than report a fork whose record cannot be
				// found.
				return fmt.Errorf("%w: the lineage insert conflicted with no readable row for (%s, %s)",
					forks.ErrStore, req.ParentProjectID, req.ActorID)
			}
			out = existing
			return nil
		default:
			return err
		}
	})
	if err != nil {
		return forks.Fork{}, false, mapForkError(err)
	}
	return out, inserted, nil
}

// FindFork implements forks.StorePort.
func (s *ForkStore) FindFork(ctx context.Context, parentProjectID, actorID string) (forks.Fork, bool, error) {
	parentUUID, err := textUUID(parentProjectID)
	if err != nil {
		return forks.Fork{}, false, fmt.Errorf("%w: parent project id: %v", forks.ErrStore, err)
	}
	actorUUID, err := textUUID(actorID)
	if err != nil {
		return forks.Fork{}, false, fmt.Errorf("%w: actor id: %v", forks.ErrStore, err)
	}
	return findFork(ctx, s.pool, parentUUID, actorUUID)
}

// findFork reads one actor's fork of one project. It is the read that
// explains a lost claim and the read a repeated request resolves against
// a taken slug; both run after the statement that decided, never before.
// ok=false means there is no such fork, which is an answer and not an
// error.
func findFork(ctx context.Context, q queryRower, parentProjectID, actorID pgtype.UUID) (forks.Fork, bool, error) {
	row := q.QueryRow(ctx,
		`SELECT `+forkColumns+` FROM project_forks WHERE parent_project_id = $1 AND forked_by = $2`,
		parentProjectID, actorID)
	fork, err := scanFork(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return forks.Fork{}, false, nil
	}
	if err != nil {
		return forks.Fork{}, false, mapForkError(err)
	}
	return fork, true, nil
}

// IsForkOf implements merge.ForkLineage (T0817): whether one project is a
// fork of another made by one actor — the triple 00086's
// pull_request_fork_gate enforces on a pull request row, asked as a
// decision-shaped question by the merge.
//
// It reads the lineage row rather than the claim path's unique-key
// conflict, because the caller is judging a row that already exists: a
// lineage row that is not there is a false answer, not an error, and the
// merge refuses on it.
func (s *ForkStore) IsForkOf(ctx context.Context, forkProjectID, parentProjectID, forkedBy string) (bool, error) {
	forkUUID, err := textUUID(forkProjectID)
	if err != nil {
		return false, nil
	}
	parentUUID, err := textUUID(parentProjectID)
	if err != nil {
		return false, nil
	}
	actorUUID, err := textUUID(forkedBy)
	if err != nil {
		return false, nil
	}
	var isFork bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM project_forks
		   WHERE fork_project_id = $1 AND parent_project_id = $2 AND forked_by = $3)`,
		forkUUID, parentUUID, actorUUID).Scan(&isFork); err != nil {
		return false, mapForkError(err)
	}
	return isFork, nil
}

// ForkOfProject implements forks.StorePort.
func (s *ForkStore) ForkOfProject(ctx context.Context, forkProjectID string) (forks.Fork, bool, error) {
	id, err := textUUID(forkProjectID)
	if err != nil {
		return forks.Fork{}, false, fmt.Errorf("%w: fork project id: %v", forks.ErrStore, err)
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+forkColumns+` FROM project_forks WHERE fork_project_id = $1`, id)
	fork, err := scanFork(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return forks.Fork{}, false, nil
	}
	if err != nil {
		return forks.Fork{}, false, mapForkError(err)
	}
	return fork, true, nil
}

// ForkOfBranch implements forks.StorePort: the lineage governing a branch,
// read through the branch's project. A branch of a project that is not a
// fork has no lineage row, which is the answer allow_from_fork turns into
// a refusal.
//
// The branch's project is resolved in a subquery rather than a join: the
// shared column list is unqualified (ClaimFork needs it that way for its
// RETURNING clause), and branches carries created_at too, so a join would
// make the list ambiguous at the first SELECT (SQLSTATE 42702).
func (s *ForkStore) ForkOfBranch(ctx context.Context, branchID string) (forks.Fork, bool, error) {
	id, err := textUUID(branchID)
	if err != nil {
		return forks.Fork{}, false, fmt.Errorf("%w: branch id: %v", forks.ErrStore, err)
	}
	row := s.pool.QueryRow(ctx,
		`SELECT `+forkColumns+`
		   FROM project_forks
		  WHERE fork_project_id = (SELECT project_id FROM branches WHERE id = $1)`, id)
	fork, err := scanFork(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return forks.Fork{}, false, nil
	}
	if err != nil {
		return forks.Fork{}, false, mapForkError(err)
	}
	return fork, true, nil
}

// ListForks implements forks.StorePort: a parent's forks, newest first
// (project_forks_parent_idx).
func (s *ForkStore) ListForks(ctx context.Context, parentProjectID string) ([]forks.Fork, error) {
	id, err := textUUID(parentProjectID)
	if err != nil {
		return nil, fmt.Errorf("%w: parent project id: %v", forks.ErrStore, err)
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+forkColumns+`
		   FROM project_forks WHERE parent_project_id = $1
		  ORDER BY created_at DESC, fork_project_id`, id)
	if err != nil {
		return nil, mapForkError(err)
	}
	defer rows.Close()
	var out []forks.Fork
	for rows.Next() {
		fork, err := scanFork(rows)
		if err != nil {
			return nil, mapForkError(err)
		}
		out = append(out, fork)
	}
	if err := rows.Err(); err != nil {
		return nil, mapForkError(err)
	}
	return out, nil
}

// SetForkSHA implements forks.StorePort: the one-shot cell.
//
// The UPDATE matches only while forked_sha IS NULL, so the first import to
// finish records the fork point and a second one (a concurrent request, or
// a retry racing the provider's own webhook) changes nothing and reports
// that it did not. 00086's guard refuses any other write to the column, so
// the discipline holds for update paths this store does not own.
func (s *ForkStore) SetForkSHA(ctx context.Context, forkProjectID, sha string) (bool, error) {
	id, err := textUUID(forkProjectID)
	if err != nil {
		return false, fmt.Errorf("%w: fork project id: %v", forks.ErrStore, err)
	}
	if sha == "" {
		return false, fmt.Errorf("%w: the fork point is empty", forks.ErrStore)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE project_forks SET forked_sha = $2 WHERE fork_project_id = $1 AND forked_sha IS NULL`,
		id, sha)
	if err != nil {
		return false, mapForkError(err)
	}
	return tag.RowsAffected() == 1, nil
}

// PersonalProjectCreator implements forks.StorePort: who created the
// personal project that holds a slug. It is the read a LOST insert is
// explained with — the insert is the compare-and-swap, this only says whose
// name it lost to — and it answers a creator rather than a project because
// the only thing the caller may act on is whether the holder can be the
// fork project the caller is itself building: creating a project records
// its creator, so a holder created by somebody else cannot be it.
//
// The read is scoped to personal projects (organization_id IS NULL): the
// insert that lost states no organization, so projects_personal_slug_idx
// (00019) is the only unique index it can have violated, and an
// organization's project that happens to share the slug lives in a
// different namespace — it is not what the insert collided with.
func (s *ForkStore) PersonalProjectCreator(ctx context.Context, slug string) (string, bool, error) {
	var creator string
	err := s.pool.QueryRow(ctx,
		`SELECT created_by::text FROM projects WHERE organization_id IS NULL AND slug = $1`,
		slug).Scan(&creator)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, mapForkError(err)
	}
	return creator, true, nil
}

// OwnedFork answers the write_scientific_state condition (own_fork_only):
// whether projectID is a fork OWNED by actorID. It is a decision-shaped
// read — the caller asks the question the matrix cell names — so
// internal/application/rsg needs no knowledge of the lineage's shape to
// resolve it, and an engine-less or gate-less service still refuses
// (the port is required for the conditional path).
func (s *ForkStore) OwnedFork(ctx context.Context, projectID, actorID string) (bool, error) {
	id, err := textUUID(projectID)
	if err != nil {
		return false, fmt.Errorf("%w: project id: %v", forks.ErrStore, err)
	}
	actor, err := textUUID(actorID)
	if err != nil {
		return false, fmt.Errorf("%w: actor id: %v", forks.ErrStore, err)
	}
	var owned bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM project_forks WHERE fork_project_id = $1 AND forked_by = $2)`,
		id, actor).Scan(&owned); err != nil {
		return false, mapForkError(err)
	}
	return owned, nil
}

// The domain event a fork records (specs/events/event-types.yaml:15,
// `branch.created`). The vocabulary is closed and has no `project.forked`,
// and the fact the lineage row states — a branch came into existence whose
// content was copied from a parent project's line — is the event
// vocabulary's own name for it; the payload carries the fork's identity,
// so a subscriber routing on branch.created can tell a fork's branch from
// any other. No other producer emits branch.created today, so the fork is
// not restating somebody else's event.
const eventBranchCreated = "branch.created"

// branchCreatedEventPayload is the payload of branch.created. It carries
// identity/reference only (docs/52「Workers」: no bulk content in events).
type branchCreatedEventPayload struct {
	// PayloadVersion is the envelope's payload_version; it has no column
	// of its own, so it travels inside the payload (the same shape
	// project.main_frozen uses).
	PayloadVersion int `json:"payload_version"`
	// BranchID is the branch the event is about — the fork's branch, in
	// the fork's project.
	BranchID string `json:"branch_id"`
	// ForkProjectID / ParentProjectID name the lineage: the project the
	// branch belongs to, and the project its content was copied from.
	ForkProjectID   string `json:"fork_project_id"`
	ParentProjectID string `json:"parent_project_id"`
	// SourceBranchID is the parent branch the copy came from.
	SourceBranchID string `json:"source_branch_id"`
	// ForkedBy is the actor who forked; the audit row carries the rest of
	// the who/when/which-request.
	ForkedBy string `json:"forked_by"`
	// RelationType is the lineage relation's canonical name.
	RelationType string `json:"relation_type"`
}

// recordForkEvent writes branch.created into the outbox inside the claim's
// transaction (docs/53: the event commits with the state change; the
// dispatcher publishes it into research_events afterwards).
func recordForkEvent(ctx context.Context, tx pgx.Tx, fork forks.Fork, visibility, correlationID string) error {
	payload, err := json.Marshal(branchCreatedEventPayload{
		PayloadVersion:  1,
		BranchID:        fork.ForkBranchID,
		ForkProjectID:   fork.ForkProjectID,
		ParentProjectID: fork.ParentProjectID,
		SourceBranchID:  fork.SourceBranchID,
		ForkedBy:        fork.ForkedBy,
		RelationType:    fork.RelationType,
	})
	if err != nil {
		return fmt.Errorf("persistence: render fork event payload: %w", err)
	}
	if err := events.Record(ctx, tx, events.Event{
		EventType:     eventBranchCreated,
		ActorID:       fork.ForkedBy,
		ProjectID:     fork.ForkProjectID,
		Visibility:    visibility,
		CorrelationID: correlationID,
		Payload:       payload,
	}); err != nil {
		return fmt.Errorf("persistence: record fork event: %w", err)
	}
	return nil
}

// forkEventVisibility reads the visibility of both projects the event
// names and maps it to the event vocabulary (docs/12 §3: an event is never
// more visible than its subject). The fork is the event's subject and the
// parent is named in its payload, so the event is public only when BOTH
// are public — fail closed in the same shape as
// mainFrozenEventVisibility: anything that is not the canonical public
// value is private, never a widening.
func forkEventVisibility(ctx context.Context, tx pgx.Tx, forkProjectID, parentProjectID pgtype.UUID) (string, error) {
	var forkVisibility, parentVisibility string
	err := tx.QueryRow(ctx,
		`SELECT f.visibility, p.visibility
		   FROM projects f
		   JOIN projects p ON p.id = $2
		  WHERE f.id = $1`, forkProjectID, parentProjectID).Scan(&forkVisibility, &parentVisibility)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: the projects a fork event names were not readable", forks.ErrStore)
	}
	if err != nil {
		return "", err
	}
	if forkVisibility == string(domain.VisibilityPublic) && parentVisibility == string(domain.VisibilityPublic) {
		return events.VisibilityPublic, nil
	}
	return events.VisibilityPrivate, nil
}

// queryRower is the read surface findFork needs, so it can run on the
// claim's transaction and on the pool alike.
type queryRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// scanFork reads one lineage row from a row-shaped source.
func scanFork(row pgx.Row) (forks.Fork, error) {
	var out forks.Fork
	var forkedSHA *string
	var createdAt time.Time
	if err := row.Scan(&out.ForkProjectID, &out.ParentProjectID, &out.ForkedBy, &out.RelationType,
		&out.SourceBranchID, &out.ForkBranchID, &forkedSHA, &createdAt); err != nil {
		return forks.Fork{}, err
	}
	out.ForkedSHA = forkedSHA
	out.CreatedAt = createdAt
	return out, nil
}

// mapForkError keeps the store's own sentinel and wraps everything else
// with the persistence context.
func mapForkError(err error) error {
	switch {
	case err == nil, errors.Is(err, forks.ErrStore):
		return err
	}
	// Both verbs are %w: the sentence is unchanged, but the database's own
	// error stays in the chain. The claim's statements are what 00086's
	// guard trigger and audit_log's foreign keys refuse, and a caller that
	// can only see "store failure" cannot tell a rule refusal from an
	// outage.
	return fmt.Errorf("%w: %w", forks.ErrStore, err)
}
