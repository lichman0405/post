package contribution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// OpportunityStore is the production contribution.Repository adapter over
// PostgreSQL (plain pgx: the columns belong to this subsystem, sqlc
// queries would put them on the shared persistence surface — the same
// arrangement as internal/gitprovider's stores, and the same reason).
//
// Creation invariants run in ONE transaction: the project row is read
// (and locked — the serialization point that keeps the audit append and
// the insert atomic), the title is snapshotted from the target row (the
// issue's title, or the research question's current version title —
// never caller-supplied), and the row is inserted with state/visibility
// per the creation kind. A second active opportunity for the same target
// conflicts on migration 00062's partial unique index and fails with
// ErrTargetAlreadyActive.
//
// The two machines (state, visibility) are compare-and-swaps here, and
// migration 00062's guard enforces them for ANY write path: the state
// map cannot be skipped, and visibility moves ONLY through
// PublicizeOpportunity — the one method that sets the transaction-scoped
// session flag the guard requires (the same sanctioned-path discipline
// as 00051's PR head refresh). A publicized row's metadata is frozen by
// the guard whatever the caller.
//
// Every state-changing write appends its audit row in the same
// transaction (docs/12 §3: visibility widening "产生 audit/event"; the
// same one-unit rule the persistence stores apply), filling actor/via/
// correlation from the request context when the caller leaves them
// empty.
type OpportunityStore struct {
	pool *pgxpool.Pool
}

// NewOpportunityStore builds the store on pool. The pool may be lazy
// (persistence.OpenLazy): the API keeps starting while PostgreSQL is
// down.
func NewOpportunityStore(pool *pgxpool.Pool) *OpportunityStore {
	return &OpportunityStore{pool: pool}
}

// Stable audit action names for the opportunity surface (the
// audit_log.action column; the domain package's Action* registry covers
// the surfaces it owns — these live with their subsystem, rendered
// verbatim by the Activity UI).
const (
	AuditActionOpportunityMarked     = "contribution.opportunity.marked"
	AuditActionOpportunitySuggested  = "contribution.opportunity.suggested"
	AuditActionOpportunityApproved   = "contribution.opportunity.approved"
	AuditActionOpportunityRejected   = "contribution.opportunity.rejected"
	AuditActionOpportunityClosed     = "contribution.opportunity.closed"
	AuditActionOpportunityPublicized = "contribution.opportunity.publicized"
)

// ErrOpportunityNotFound: no opportunity row exists for the
// (project, id) pair — an unknown id, or an opportunity of another
// project, which reports the same outcome without leaking the foreign
// entity's existence.
var ErrOpportunityNotFound = errors.New("contribution: opportunity not found")

// ErrTargetNotFound: the target does not exist in the project (an
// unknown id, a foreign row, or — for a research_question target — an
// object of another type; same outcome, no foreign existence leak).
var ErrTargetNotFound = errors.New("contribution: target not found in the project")

// ErrTargetAlreadyActive: the target already carries a non-closed
// opportunity (suggested or open) — one active opportunity per target;
// close it (or reject the suggestion) and mark again to start a new
// lifecycle round, the previous row staying as history.
var ErrTargetAlreadyActive = errors.New("contribution: target already carries an active opportunity")

// ErrPublicizedFrozen: the row is publicized — its metadata and
// publicize stamps are immutable (migration 00062) so the open network
// can index it without drift.
var ErrPublicizedFrozen = errors.New("contribution: publicized opportunity is frozen")

// ErrValidation: an input fails the domain shape rules (an unknown
// target type/difficulty/state, an oversized text, a malformed id, an
// agent actor on a governance action, ...).
var ErrValidation = errors.New("contribution: validation failed")

// ErrStore: the persistence adapter failed (cause kept for the log).
var ErrStore = errors.New("contribution: store failure")

// PublicizeError reports a publicize attempt on a row that cannot be
// publicized: not open, or already public.
type PublicizeError struct {
	// ID is the opportunity the attempt was made on.
	ID string
	// State is the row's current state ("" when irrelevant).
	State OpportunityState
	// AlreadyPublic reports that the row is already public (visibility
	// is terminal; the state field carries the row's state).
	AlreadyPublic bool
}

// Error implements error.
func (e *PublicizeError) Error() string {
	if e.AlreadyPublic {
		return fmt.Sprintf("contribution: opportunity %s is already public — visibility is terminal", e.ID)
	}
	return fmt.Sprintf("contribution: opportunity %s is %s — only an open opportunity can be publicized", e.ID, e.State)
}

// TerminalError reports a metadata update on a closed row (closed is
// terminal: the row stays as history, it accepts no changes).
type TerminalError struct {
	// ID is the closed opportunity.
	ID string
	// State is the terminal state it is in.
	State OpportunityState
}

// Error implements error.
func (e *TerminalError) Error() string {
	return fmt.Sprintf("contribution: opportunity %s is %s — terminal, no metadata updates", e.ID, e.State)
}

// StateConflictError reports a transition whose compare-and-swap missed:
// the row moved between the read and the update (a concurrent
// transition or publicize landed first). The caller may re-read and
// retry.
type StateConflictError struct {
	// ID is the opportunity whose state moved underneath the
	// transition.
	ID string
	// Current is the state the row is in now.
	Current OpportunityState
}

// Error implements error.
func (e *StateConflictError) Error() string {
	return fmt.Sprintf("contribution: opportunity %s state moved underneath the transition (now %s); re-read and retry",
		e.ID, e.Current)
}

// CreateOpportunity implements Repository.
func (s *OpportunityStore) CreateOpportunity(ctx context.Context, in CreateOpportunityParams) (ContributionOpportunity, error) {
	if !ValidOpportunityTargetType(in.TargetType) ||
		!ValidDifficulty(in.Difficulty) ||
		!ValidRequiredCapabilities(in.RequiredCapabilities) ||
		!ValidOpportunityDescription(in.Description) ||
		(in.State != OpportunityStateSuggested && in.State != OpportunityStateOpen) {
		return ContributionOpportunity{}, ErrValidation
	}
	projectID, err := textUUID(in.ProjectID)
	if err != nil {
		return ContributionOpportunity{}, ErrValidation
	}
	targetID, err := textUUID(in.TargetID)
	if err != nil {
		return ContributionOpportunity{}, ErrValidation
	}
	createdBy, err := textUUID(in.By.UserID)
	if err != nil {
		return ContributionOpportunity{}, ErrValidation
	}
	var suggestedBy *pgtype.UUID
	if in.State == OpportunityStateSuggested {
		suggestedBy = &createdBy // the suggester is the row creator
	}

	var created ContributionOpportunity
	err = withTx(ctx, s.pool, func(tx pgx.Tx) error {
		// The project read doubles as the serialization point: the row
		// lock orders concurrent creations of one project, so the audit
		// append and the insert commit as one unit without racing a
		// sibling write (the same discipline as the PR store's creation
		// lock).
		var one int
		if err := tx.QueryRow(ctx,
			`SELECT 1 FROM projects WHERE id = $1 FOR UPDATE`, projectID).Scan(&one); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return projects.ErrProjectNotFound
			}
			return err
		}
		// The title is snapshotted from the target, never caller
		// supplied: an issue's title, or the research question's current
		// version title. The snapshot also proves target existence
		// project-scoped (the deferred guard backstops it at COMMIT).
		var title string
		switch in.TargetType {
		case TargetIssue:
			err = tx.QueryRow(ctx,
				`SELECT title FROM issues WHERE id = $1 AND project_id = $2`,
				targetID, projectID).Scan(&title)
		case TargetResearchQuestion:
			err = tx.QueryRow(ctx,
				`SELECT v.title
				   FROM scientific_objects o
				   JOIN scientific_object_versions v ON v.object_id = o.id
				  WHERE o.id = $1 AND o.project_id = $2
				    AND o.object_type = 'research_question'
				    AND v.version_no = o.current_version_no`,
				targetID, projectID).Scan(&title)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTargetNotFound
		}
		if err != nil {
			return err
		}
		if !ValidOpportunityTitle(title) {
			return fmt.Errorf("contribution: target title %q is not a usable opportunity title: %w", title, ErrValidation)
		}

		created, err = s.scanOpportunity(ctx, tx.QueryRow(ctx, `
			INSERT INTO contribution_opportunities
			    (project_id, target_type, target_id, title, description,
			     difficulty, required_capabilities, state, created_by,
			     suggested_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			RETURNING `+opportunityReturning+``,
			projectID, string(in.TargetType), targetID, title, in.Description,
			string(in.Difficulty), SortedCapabilities(in.RequiredCapabilities),
			string(in.State), createdBy, suggestedBy))
		if err != nil {
			return mapWriteError(err)
		}
		action := AuditActionOpportunityMarked
		if in.State == OpportunityStateSuggested {
			action = AuditActionOpportunitySuggested
		}
		return s.appendAudit(ctx, tx, domain.AuditEntry{
			ActorID:   in.By.UserID,
			Action:    action,
			TargetRef: "contribution_opportunity:" + created.ID,
			ProjectID: in.ProjectID,
			AfterSummary: map[string]any{
				"state":      string(created.State),
				"visibility": string(created.Visibility),
			},
			Metadata: map[string]any{
				"target_type": string(created.TargetType),
				"target_id":   created.TargetID,
			},
		})
	})
	if err != nil {
		if errors.Is(err, ErrValidation) || errors.Is(err, ErrTargetNotFound) ||
			errors.Is(err, ErrTargetAlreadyActive) || errors.Is(err, projects.ErrProjectNotFound) {
			return ContributionOpportunity{}, err
		}
		return ContributionOpportunity{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return created, nil
}

// GetOpportunity implements Repository.
func (s *OpportunityStore) GetOpportunity(ctx context.Context, projectID, id string) (ContributionOpportunity, error) {
	pid, err := textUUID(projectID)
	if err != nil {
		return ContributionOpportunity{}, ErrOpportunityNotFound
	}
	oid, err := textUUID(id)
	if err != nil {
		return ContributionOpportunity{}, ErrOpportunityNotFound
	}
	row, err := s.scanOpportunity(ctx, s.pool.QueryRow(ctx, opportunitySelect+`
		 WHERE o.project_id = $1 AND o.id = $2`, pid, oid))
	if errors.Is(err, pgx.ErrNoRows) {
		return ContributionOpportunity{}, ErrOpportunityNotFound
	}
	if err != nil {
		return ContributionOpportunity{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return row, nil
}

// ListOpportunities implements Repository. An unknown project has no
// opportunities: empty list, not an error (the same read discipline as
// the version logs).
func (s *OpportunityStore) ListOpportunities(ctx context.Context, projectID string) ([]ContributionOpportunity, error) {
	id, err := textUUID(projectID)
	if err != nil {
		return nil, nil // cannot name a project; empty, not an error
	}
	return s.list(ctx, opportunitySelect+`
		 WHERE o.project_id = $1
		 ORDER BY o.created_at DESC, o.id`, id)
}

// ListPublicOpportunities implements Repository: the open-network view —
// publicized, open opportunities across all projects, newest first.
// Only rows that went through the explicit publicize action ever
// qualify.
func (s *OpportunityStore) ListPublicOpportunities(ctx context.Context) ([]ContributionOpportunity, error) {
	return s.list(ctx, opportunitySelect+`
		 WHERE o.visibility = 'public' AND o.state = 'open'
		 ORDER BY o.created_at DESC, o.id`)
}

// SetOpportunityState implements Repository. The transition is a
// compare-and-swap on the expected state; the zero-row outcome is
// distinguished by one read: not in the project (never leak a foreign
// entity) or moved underneath (concurrent transition).
func (s *OpportunityStore) SetOpportunityState(ctx context.Context, projectID, id string, expected, to OpportunityState, by Actor) (ContributionOpportunity, error) {
	if !ValidOpportunityState(expected) || !ValidOpportunityState(to) {
		return ContributionOpportunity{}, ErrValidation
	}
	pid, err := textUUID(projectID)
	if err != nil {
		return ContributionOpportunity{}, ErrValidation
	}
	oid, err := textUUID(id)
	if err != nil {
		return ContributionOpportunity{}, ErrValidation
	}
	actorID, err := textUUID(by.UserID)
	if err != nil {
		return ContributionOpportunity{}, ErrValidation
	}
	var moved ContributionOpportunity
	err = withTx(ctx, s.pool, func(tx pgx.Tx) error {
		row, err := s.scanOpportunity(ctx, tx.QueryRow(ctx, `
			UPDATE contribution_opportunities o
			   SET state = $3,
			       approved_by = CASE WHEN $4 = 'suggested' AND $3 = 'open'
			                         THEN $5 ELSE approved_by END,
			       updated_at = now()
			 WHERE o.project_id = $1 AND o.id = $2 AND o.state = $4
			RETURNING `+opportunityReturning,
			pid, oid, string(to), string(expected), actorID))
		if errors.Is(err, pgx.ErrNoRows) {
			current, gerr := s.currentState(ctx, tx, pid, oid)
			if gerr != nil {
				return gerr
			}
			return &StateConflictError{ID: id, Current: current}
		}
		if err != nil {
			return mapWriteError(err)
		}
		moved = row
		action := AuditActionOpportunityClosed
		if expected == OpportunityStateSuggested && to == OpportunityStateOpen {
			action = AuditActionOpportunityApproved
		} else if expected == OpportunityStateSuggested && to == OpportunityStateClosed {
			action = AuditActionOpportunityRejected
		}
		return s.appendAudit(ctx, tx, domain.AuditEntry{
			ActorID:       by.UserID,
			Action:        action,
			TargetRef:     "contribution_opportunity:" + id,
			ProjectID:     projectID,
			BeforeSummary: map[string]any{"state": string(expected)},
			AfterSummary:  map[string]any{"state": string(to)},
		})
	})
	if err != nil {
		if errors.Is(err, ErrValidation) || errors.Is(err, ErrOpportunityNotFound) ||
			errors.As(err, new(*StateConflictError)) {
			return ContributionOpportunity{}, err
		}
		return ContributionOpportunity{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return moved, nil
}

// PublicizeOpportunity implements Repository: the explicit, audited
// internal → public widening (docs/12 §3). The row must be open and
// internal; the session flag this method sets is the one migration
// 00062's guard requires, so this is the ONLY write path that can make
// a row public — an UPDATE from anywhere else fails at the database.
func (s *OpportunityStore) PublicizeOpportunity(ctx context.Context, projectID, id string, by Actor) (ContributionOpportunity, error) {
	pid, err := textUUID(projectID)
	if err != nil {
		return ContributionOpportunity{}, ErrValidation
	}
	oid, err := textUUID(id)
	if err != nil {
		return ContributionOpportunity{}, ErrValidation
	}
	actorID, err := textUUID(by.UserID)
	if err != nil {
		return ContributionOpportunity{}, ErrValidation
	}
	var publicized ContributionOpportunity
	err = withTx(ctx, s.pool, func(tx pgx.Tx) error {
		// The row lock serializes concurrent publicize/metadata/state
		// writes of the same row: the checks below read a stable row.
		row, err := s.scanOpportunity(ctx, tx.QueryRow(ctx, opportunitySelect+`
			 WHERE o.project_id = $1 AND o.id = $2 FOR UPDATE OF o`, pid, oid))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOpportunityNotFound
		}
		if err != nil {
			return err
		}
		if row.Visibility == OpportunityVisibilityPublic {
			return &PublicizeError{ID: id, State: row.State, AlreadyPublic: true}
		}
		if row.State != OpportunityStateOpen {
			return &PublicizeError{ID: id, State: row.State}
		}
		// The transaction-scoped session flag migration 00062's guard
		// requires (set_config is_local=true resets at transaction end):
		// the visibility column moves ONLY through this flagged path,
		// whatever else runs in the database.
		if _, err := tx.Exec(ctx,
			`SELECT set_config('post.co_publicize', 'on', true)`); err != nil {
			return err
		}
		moved, err := s.scanOpportunity(ctx, tx.QueryRow(ctx, `
			UPDATE contribution_opportunities o
			   SET visibility = 'public',
			       publicized_by = $3,
			       publicized_at = now(),
			       updated_at = now()
			 WHERE o.project_id = $1 AND o.id = $2
			   AND o.visibility = 'internal' AND o.state = 'open'
			RETURNING `+opportunityReturning,
			pid, oid, actorID))
		if errors.Is(err, pgx.ErrNoRows) {
			// The row moved between the locked read and the update —
			// impossible under the row lock, but never assume.
			return &StateConflictError{ID: id, Current: row.State}
		}
		if err != nil {
			return mapWriteError(err)
		}
		publicized = moved
		return s.appendAudit(ctx, tx, domain.AuditEntry{
			ActorID:       by.UserID,
			Action:        AuditActionOpportunityPublicized,
			TargetRef:     "contribution_opportunity:" + id,
			ProjectID:     projectID,
			BeforeSummary: map[string]any{"visibility": "internal"},
			AfterSummary:  map[string]any{"visibility": "public"},
			Metadata: map[string]any{
				"target_type": string(moved.TargetType),
				"target_id":   moved.TargetID,
			},
		})
	})
	if err != nil {
		if errors.Is(err, ErrValidation) || errors.Is(err, ErrOpportunityNotFound) ||
			errors.As(err, new(*PublicizeError)) || errors.As(err, new(*StateConflictError)) {
			return ContributionOpportunity{}, err
		}
		return ContributionOpportunity{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return publicized, nil
}

// UpdateOpportunityMetadata implements Repository. The patch applies
// only to a non-closed, internal row: a closed row is terminal, and a
// publicized row is frozen (migration 00062 refuses any other path to
// either write).
func (s *OpportunityStore) UpdateOpportunityMetadata(ctx context.Context, projectID, id string, patch MetadataPatch) (ContributionOpportunity, error) {
	if patch.Title == nil && patch.Description == nil && patch.Difficulty == nil && patch.RequiredCapabilities == nil {
		return ContributionOpportunity{}, ErrValidation
	}
	if (patch.Title != nil && !ValidOpportunityTitle(*patch.Title)) ||
		(patch.Description != nil && !ValidOpportunityDescription(*patch.Description)) ||
		(patch.Difficulty != nil && !ValidDifficulty(*patch.Difficulty)) ||
		(patch.RequiredCapabilities != nil && !ValidRequiredCapabilities(*patch.RequiredCapabilities)) {
		return ContributionOpportunity{}, ErrValidation
	}
	pid, err := textUUID(projectID)
	if err != nil {
		return ContributionOpportunity{}, ErrValidation
	}
	oid, err := textUUID(id)
	if err != nil {
		return ContributionOpportunity{}, ErrValidation
	}
	var caps *[]string
	if patch.RequiredCapabilities != nil {
		sorted := SortedCapabilities(*patch.RequiredCapabilities)
		caps = &sorted
	}
	var difficulty *string
	if patch.Difficulty != nil {
		d := string(*patch.Difficulty)
		difficulty = &d
	}
	row, err := s.scanOpportunity(ctx, s.pool.QueryRow(ctx, `
		UPDATE contribution_opportunities o
		   SET title = COALESCE($3, o.title),
		       description = COALESCE($4, o.description),
		       difficulty = COALESCE($5, o.difficulty),
		       required_capabilities = COALESCE($6, o.required_capabilities),
		       updated_at = now()
		 WHERE o.project_id = $1 AND o.id = $2
		   AND o.visibility = 'internal' AND o.state <> 'closed'
		RETURNING `+opportunityReturning,
		pid, oid, patch.Title, patch.Description, difficulty, caps))
	if errors.Is(err, pgx.ErrNoRows) {
		current, gerr := s.currentRow(ctx, s.pool, pid, oid)
		if gerr != nil {
			return ContributionOpportunity{}, gerr
		}
		if current.State == OpportunityStateClosed {
			return ContributionOpportunity{}, &TerminalError{ID: id, State: current.State}
		}
		return ContributionOpportunity{}, ErrPublicizedFrozen
	}
	if err != nil {
		return ContributionOpportunity{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return row, nil
}

// opportunitySelect is the shared column list for the opportunity row.
const opportunitySelect = `
	SELECT o.id, o.project_id, o.target_type, o.target_id, o.title,
	       o.description, o.difficulty, o.required_capabilities, o.state,
	       o.visibility, o.created_by, o.suggested_by, o.approved_by,
	       o.publicized_by, o.publicized_at, o.created_at, o.updated_at
	  FROM contribution_opportunities o`

// opportunityReturning is the shared RETURNING column list for the
// write paths (unqualified, so it works with or without the alias).
const opportunityReturning = `
	id, project_id, target_type, target_id, title,
	description, difficulty, required_capabilities, state,
	visibility, created_by, suggested_by, approved_by,
	publicized_by, publicized_at, created_at, updated_at`

// scanOpportunity reads one row from the shared column list. Nullable
// columns scan into pgtype values (pgx refuses NULL into *string /
// *time.Time) and are converted to the domain's pointer form.
func (s *OpportunityStore) scanOpportunity(ctx context.Context, row pgx.Row) (ContributionOpportunity, error) {
	var o ContributionOpportunity
	var targetType, difficulty, state, visibility string
	var suggestedBy, approvedBy, publicizedBy pgtype.UUID
	var publicizedAt pgtype.Timestamptz
	if err := row.Scan(&o.ID, &o.ProjectID, &targetType, &o.TargetID,
		&o.Title, &o.Description, &difficulty, &o.RequiredCapabilities,
		&state, &visibility, &o.CreatedBy, &suggestedBy, &approvedBy,
		&publicizedBy, &publicizedAt, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return ContributionOpportunity{}, err
	}
	o.TargetType = OpportunityTargetType(targetType)
	o.Difficulty = Difficulty(difficulty)
	o.State = OpportunityState(state)
	o.Visibility = OpportunityVisibility(visibility)
	o.SuggestedBy = uuidTextPtr(suggestedBy)
	o.ApprovedBy = uuidTextPtr(approvedBy)
	o.PublicizedBy = uuidTextPtr(publicizedBy)
	if publicizedAt.Valid {
		t := publicizedAt.Time
		o.PublicizedAt = &t
	}
	return o, nil
}

// uuidTextPtr converts a nullable scanned uuid to the domain's pointer
// text form (nil when absent).
func uuidTextPtr(u pgtype.UUID) *string {
	if !u.Valid {
		return nil
	}
	s := uuidText(u)
	return &s
}

// uuidText renders a scanned uuid in the domain text form.
func uuidText(u pgtype.UUID) string {
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (s *OpportunityStore) list(ctx context.Context, query string, args ...any) ([]ContributionOpportunity, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	defer rows.Close()
	out := []ContributionOpportunity{}
	for rows.Next() {
		o, err := s.scanOpportunity(ctx, rows)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrStore, err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return out, nil
}

// currentState reads the row's state after a missed CAS, mapping a
// foreign/unknown row to ErrOpportunityNotFound.
func (s *OpportunityStore) currentState(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, pid, oid pgtype.UUID) (OpportunityState, error) {
	row, err := s.currentRow(ctx, q, pid, oid)
	if err != nil {
		return "", err
	}
	return row.State, nil
}

// currentRow reads one row by (project, id) through either a
// transaction or the pool.
func (s *OpportunityStore) currentRow(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, pid, oid pgtype.UUID) (ContributionOpportunity, error) {
	row, err := s.scanOpportunity(ctx, q.QueryRow(ctx, opportunitySelect+`
		 WHERE o.project_id = $1 AND o.id = $2`, pid, oid))
	if errors.Is(err, pgx.ErrNoRows) {
		return ContributionOpportunity{}, ErrOpportunityNotFound
	}
	if err != nil {
		return ContributionOpportunity{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return row, nil
}

// appendAudit writes one audit row inside the caller's transaction —
// the same one-unit rule as the persistence stores: the state change
// and its audit record commit or roll back together. Empty actor/via/
// correlation fields fall back to the request context, then to
// domain.ViaInternal.
func (s *OpportunityStore) appendAudit(ctx context.Context, tx pgx.Tx, e domain.AuditEntry) error {
	actor, via, corr := e.ActorID, e.Via, e.CorrelationID
	if info, ok := domain.RequestInfoFrom(ctx); ok {
		if actor == "" {
			actor = info.ActorID
		}
		if via == "" {
			via = info.Via
		}
		if corr == "" {
			corr = info.CorrelationID
		}
	}
	if via == "" {
		via = domain.ViaInternal
	}
	var actorID *pgtype.UUID
	if actor != "" {
		id, err := textUUID(actor)
		if err != nil {
			return fmt.Errorf("contribution: audit actor: %w", err)
		}
		actorID = &id
	}
	before, err := marshalSummary(e.BeforeSummary)
	if err != nil {
		return err
	}
	after, err := marshalSummary(e.AfterSummary)
	if err != nil {
		return err
	}
	metadata := []byte(`{}`)
	if e.Metadata != nil {
		if metadata, err = json.Marshal(e.Metadata); err != nil {
			return fmt.Errorf("contribution: audit metadata: %w", err)
		}
	}
	projectRef, err := nullableUUID(e.ProjectID)
	if err != nil {
		return fmt.Errorf("contribution: audit project: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO audit_log
		    (actor_id, via, action, target_ref, project_id, correlation_id,
		     before_summary, after_summary, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		actorID, via, e.Action, nullableText(e.TargetRef), projectRef,
		corr, before, after, metadata)
	if err != nil {
		return fmt.Errorf("contribution: audit %s: %w", e.Action, err)
	}
	return nil
}

func marshalSummary(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("contribution: audit summary: %w", err)
	}
	return b, nil
}

func nullableText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableUUID(s string) (any, error) {
	if s == "" {
		return nil, nil
	}
	return textUUID(s)
}

// mapWriteError maps the constraint outcomes the guard and the indexes
// produce for the creation/update paths: the partial unique index's
// conflict (a second active opportunity for the target), the guard's
// refusal (P0001 raise_exception), and malformed identifiers.
func mapWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505": // unique_violation: the target's active-row index
			return ErrTargetAlreadyActive
		case "22P02": // invalid_text_representation: a malformed id
			return ErrValidation
		case "23503": // foreign_key_violation: a dangling actor/target ref
			return ErrValidation
		case "P0001": // the migration 00062 guard's raise_exception
			return fmt.Errorf("%w: %s", ErrStore, pgErr.Message)
		}
	}
	return err
}

// textUUID parses the domain text form of a uuid into pgx's uuid type
// (the local twin of the persistence helper; this package's columns are
// not on the sqlc surface).
func textUUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return pgtype.UUID{}, fmt.Errorf("contribution: invalid uuid %q: %w", s, err)
	}
	return u, nil
}

// withTx runs fn inside one transaction: committed on nil, rolled back
// otherwise (the local twin of the persistence helper).
//
// The rollback runs on a non-cancelled context, the convention the events
// package states for its own transactions (internal/events/publish.go, and
// fanout.go / inbox_store.go / subscription_store.go / webhook_store.go the
// same shape): a caller that cancels ctx — the LedgerProjector does exactly
// that when the worker shuts down — must not be able to abort the rollback
// itself and leave the connection mid-transaction. The T0807 review asked
// for this at wiring time, which is now.
func withTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var _ Repository = (*OpportunityStore)(nil)
