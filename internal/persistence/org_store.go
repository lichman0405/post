package persistence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/orgs"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// OrgStore is the production orgs.OrgStore adapter over PostgreSQL (sqlc
// generated queries, pgx). Governance invariants that must hold under
// concurrency (the last-owner guard) run inside one transaction per
// operation, serialized per organization by a FOR UPDATE lock on the
// organization row.
type OrgStore struct {
	pool *pgxpool.Pool
}

// NewOrgStore builds the store on pool. The pool may be lazy (OpenLazy):
// the API keeps starting while PostgreSQL is down.
func NewOrgStore(pool *pgxpool.Pool) *OrgStore {
	return &OrgStore{pool: pool}
}

// CreateOrganization implements orgs.OrgStore: inserts the organization and
// the creator's owner membership (verified, starting affiliationStart) in
// one transaction — an organization can never exist without its first
// owner.
func (s *OrgStore) CreateOrganization(ctx context.Context, org domain.Organization, creatorUserID string, affiliationStart time.Time) (domain.Organization, domain.OrganizationMembership, error) {
	creatorID, err := textUUID(creatorUserID)
	if err != nil {
		return domain.Organization{}, domain.OrganizationMembership{}, fmt.Errorf("persistence: creator id: %w", err)
	}
	var created domain.Organization
	var membership domain.OrganizationMembership
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		row, err := q.CreateOrganization(ctx, sqlc.CreateOrganizationParams{
			Slug:        org.Slug,
			Name:        org.Name,
			Description: nullString(org.Description),
		})
		if err != nil {
			return mapOrgWriteError(err)
		}
		created = orgFromRow(row)
		if err := q.AddOrganizationMembership(ctx, sqlc.AddOrganizationMembershipParams{
			OrganizationID:   row.ID,
			UserID:           creatorID,
			Role:             string(domain.OrgRoleOwner),
			AffiliationStart: dateToPG(affiliationStart),
			Verified:         true,
		}); err != nil {
			return mapOrgWriteError(err)
		}
		membership = domain.OrganizationMembership{
			OrganizationID:   pgUUIDToText(row.ID),
			UserID:           creatorUserID,
			Role:             domain.OrgRoleOwner,
			AffiliationStart: affiliationStart,
			Verified:         true,
		}
		// The audit row commits (or rolls back) with the organization
		// itself (T0110: a governance action is never recorded without
		// its audit record, or recorded without the action).
		return appendAudit(ctx, q, domain.AuditEntry{
			Action:         domain.ActionOrgCreated,
			ActorID:        creatorUserID,
			TargetRef:      "organization:" + pgUUIDToText(row.ID),
			OrganizationID: pgUUIDToText(row.ID),
			AfterSummary: map[string]any{
				"slug": org.Slug,
				"name": org.Name,
			},
		})
	})
	if err != nil {
		return domain.Organization{}, domain.OrganizationMembership{}, err
	}
	return created, membership, nil
}

// GetOrganization implements orgs.OrgStore.
func (s *OrgStore) GetOrganization(ctx context.Context, orgID string) (domain.Organization, error) {
	id, err := textUUID(orgID)
	if err != nil {
		return domain.Organization{}, orgs.ErrOrgNotFound
	}
	row, err := sqlc.New(s.pool).GetOrganizationByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Organization{}, orgs.ErrOrgNotFound
	}
	if err != nil {
		return domain.Organization{}, fmt.Errorf("persistence: get organization: %w", err)
	}
	return orgFromRow(row), nil
}

// UpdateOrganization implements orgs.OrgStore. The rename/description
// rewrite runs inside one transaction with its audit row: the before
// summary is read in the same transaction the UPDATE commits.
func (s *OrgStore) UpdateOrganization(ctx context.Context, org domain.Organization) (domain.Organization, error) {
	id, err := textUUID(org.ID)
	if err != nil {
		return domain.Organization{}, orgs.ErrOrgNotFound
	}
	var updated domain.Organization
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		before, err := q.GetOrganizationByID(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return orgs.ErrOrgNotFound
		}
		if err != nil {
			return err
		}
		row, err := q.UpdateOrganization(ctx, sqlc.UpdateOrganizationParams{
			ID:          id,
			Name:        org.Name,
			Description: nullString(org.Description),
		})
		if err != nil {
			return err
		}
		updated = orgFromRow(row)
		return appendAudit(ctx, q, domain.AuditEntry{
			Action:         domain.ActionOrgUpdated,
			TargetRef:      "organization:" + org.ID,
			OrganizationID: org.ID,
			BeforeSummary: map[string]any{
				"name":        before.Name,
				"description": nullStringToValue(before.Description),
			},
			AfterSummary: map[string]any{
				"name":        org.Name,
				"description": nullString(org.Description),
			},
		})
	})
	if err != nil {
		return domain.Organization{}, err
	}
	return updated, nil
}

// DeactivateOrganization implements orgs.OrgStore. The soft-delete and its
// audit row commit in one transaction.
func (s *OrgStore) DeactivateOrganization(ctx context.Context, orgID string) error {
	id, err := textUUID(orgID)
	if err != nil {
		return orgs.ErrOrgNotFound
	}
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		row, err := q.DeactivateOrganization(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return orgs.ErrOrgNotFound
		}
		if err != nil {
			return err
		}
		return appendAudit(ctx, q, domain.AuditEntry{
			Action:         domain.ActionOrgDeactivated,
			TargetRef:      "organization:" + orgID,
			OrganizationID: orgID,
			AfterSummary: map[string]any{
				"deactivated_at": timestamptzPtr(row.DeactivatedAt),
			},
		})
	})
	if err != nil {
		return err
	}
	return nil
}

// ListOrganizationsForUser implements orgs.OrgStore.
func (s *OrgStore) ListOrganizationsForUser(ctx context.Context, userID string) ([]domain.Organization, error) {
	id, err := textUUID(userID)
	if err != nil {
		return nil, nil // an id that is not a uuid matches no memberships
	}
	rows, err := sqlc.New(s.pool).ListOrganizationsForUser(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list organizations: %w", err)
	}
	out := make([]domain.Organization, 0, len(rows))
	for _, row := range rows {
		out = append(out, orgFromRow(row))
	}
	return out, nil
}

// GetMembership implements orgs.OrgStore.
func (s *OrgStore) GetMembership(ctx context.Context, orgID, userID string) (domain.OrganizationMembership, error) {
	oID, uID, err := twoUUIDs(orgID, userID)
	if err != nil {
		return domain.OrganizationMembership{}, orgs.ErrMemberNotFound
	}
	row, err := sqlc.New(s.pool).GetOrganizationMembership(ctx, sqlc.GetOrganizationMembershipParams{
		OrganizationID: oID,
		UserID:         uID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.OrganizationMembership{}, orgs.ErrMemberNotFound
	}
	if err != nil {
		return domain.OrganizationMembership{}, fmt.Errorf("persistence: get membership: %w", err)
	}
	return membershipFromRow(row), nil
}

// GetUserByHandle implements orgs.OrgStore.
func (s *OrgStore) GetUserByHandle(ctx context.Context, handle string) (domain.User, error) {
	row, err := sqlc.New(s.pool).GetUserByHandle(ctx, handle)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.User{}, orgs.ErrUserNotFound
	}
	if err != nil {
		return domain.User{}, fmt.Errorf("persistence: get user by handle: %w", err)
	}
	return domain.User{
		ID:          pgUUIDToText(row.ID),
		Handle:      row.Handle,
		Email:       nullStringToValue(row.Email),
		DisplayName: row.DisplayName,
		CreatedAt:   row.CreatedAt.Time,
	}, nil
}

// ListMembers implements orgs.OrgStore.
func (s *OrgStore) ListMembers(ctx context.Context, orgID string) ([]domain.OrganizationMembership, error) {
	id, err := textUUID(orgID)
	if err != nil {
		return nil, orgs.ErrOrgNotFound
	}
	rows, err := sqlc.New(s.pool).ListOrganizationMemberships(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list memberships: %w", err)
	}
	out := make([]domain.OrganizationMembership, 0, len(rows))
	for _, row := range rows {
		out = append(out, membershipFromRow(row))
	}
	return out, nil
}

// AddMembership implements orgs.OrgStore. The insert runs inside one
// transaction that row-locks the organization and re-checks
// deactivated_at: an invite racing a concurrent deactivation can never
// land a membership in an already-deactivated organization. Constraint
// violations classify the three conflicts: existing row →
// ErrAlreadyMember, unknown user → ErrUserNotFound, unknown organization
// → ErrOrgNotFound.
func (s *OrgStore) AddMembership(ctx context.Context, m domain.OrganizationMembership) (domain.OrganizationMembership, error) {
	oID, uID, err := twoUUIDs(m.OrganizationID, m.UserID)
	if err != nil {
		return domain.OrganizationMembership{}, fmt.Errorf("persistence: membership ids: %w", err)
	}
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		orgRow, err := q.GetOrganizationByIDForUpdate(ctx, oID)
		if errors.Is(err, pgx.ErrNoRows) {
			return orgs.ErrOrgNotFound
		}
		if err != nil {
			return err
		}
		if !orgFromRow(orgRow).Active() {
			return orgs.ErrOrgDeactivated
		}
		if err := q.AddOrganizationMembership(ctx, sqlc.AddOrganizationMembershipParams{
			OrganizationID:   oID,
			UserID:           uID,
			Role:             string(m.Role),
			AffiliationStart: dateToPG(m.AffiliationStart),
			Verified:         m.Verified,
		}); err != nil {
			return err
		}
		return appendAudit(ctx, q, domain.AuditEntry{
			Action:         domain.ActionOrgMemberInvited,
			TargetRef:      "user:" + m.UserID,
			OrganizationID: m.OrganizationID,
			AfterSummary: map[string]any{
				"user_id":           m.UserID,
				"role":              string(m.Role),
				"affiliation_start": m.AffiliationStart,
				"verified":          m.Verified,
			},
		})
	})
	if err != nil {
		return domain.OrganizationMembership{}, mapMembershipWriteError(err)
	}
	return m, nil
}

// UpdateMembershipRoleAndDates implements orgs.OrgStore. The last-owner
// guard runs in the same transaction as the UPDATE, serialized by the
// organization row lock: demoting the last active owner refuses with
// ErrLastOwner no matter how many owners act concurrently. affiliation_end
// is never written here — it is EndAffiliation's exclusive job — so an
// adjustment of an ended membership keeps the departure date.
func (s *OrgStore) UpdateMembershipRoleAndDates(ctx context.Context, orgID, userID string, role domain.OrgRole, affiliationStart time.Time, verified bool) (domain.OrganizationMembership, error) {
	oID, uID, err := twoUUIDs(orgID, userID)
	if err != nil {
		return domain.OrganizationMembership{}, orgs.ErrMemberNotFound
	}
	var updated domain.OrganizationMembership
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetOrganizationByIDForUpdate(ctx, oID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return orgs.ErrOrgNotFound
			}
			return err
		}
		current, err := q.GetOrganizationMembership(ctx, sqlc.GetOrganizationMembershipParams{
			OrganizationID: oID,
			UserID:         uID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return orgs.ErrMemberNotFound
		}
		if err != nil {
			return err
		}
		if current.Role == string(domain.OrgRoleOwner) && !current.AffiliationEnd.Valid &&
			role != domain.OrgRoleOwner {
			if err := refuseLastOwnerRemoval(ctx, q, oID); err != nil {
				return err
			}
		}
		row, err := q.UpdateOrganizationMembership(ctx, sqlc.UpdateOrganizationMembershipParams{
			OrganizationID:   oID,
			UserID:           uID,
			Role:             string(role),
			AffiliationStart: dateToPG(affiliationStart),
			Verified:         verified,
		})
		if err != nil {
			return err
		}
		updated = membershipFromRow(row)
		// The before summary comes from the row read under the same lock
		// as the UPDATE — the audit pair can never disagree with what
		// actually changed.
		return appendAudit(ctx, q, domain.AuditEntry{
			Action:         domain.ActionOrgMemberUpdated,
			TargetRef:      "user:" + userID,
			OrganizationID: orgID,
			BeforeSummary: map[string]any{
				"role":              current.Role,
				"affiliation_start": dateFromPG(current.AffiliationStart),
				"verified":          current.Verified,
			},
			AfterSummary: map[string]any{
				"role":              string(role),
				"affiliation_start": affiliationStart,
				"verified":          verified,
			},
		})
	})
	if err != nil {
		return domain.OrganizationMembership{}, mapMembershipWriteError(err)
	}
	return updated, nil
}

// EndAffiliation implements orgs.OrgStore. The row is kept — only
// affiliation_end is set (离职不删除历史). Ending the last active owner's
// affiliation refuses with ErrLastOwner, in the same transaction as the
// UPDATE.
func (s *OrgStore) EndAffiliation(ctx context.Context, orgID, userID string, end time.Time) error {
	oID, uID, err := twoUUIDs(orgID, userID)
	if err != nil {
		return orgs.ErrMemberNotFound
	}
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetOrganizationByIDForUpdate(ctx, oID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return orgs.ErrOrgNotFound
			}
			return err
		}
		current, err := q.GetOrganizationMembership(ctx, sqlc.GetOrganizationMembershipParams{
			OrganizationID: oID,
			UserID:         uID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return orgs.ErrMemberNotFound
		}
		if err != nil {
			return err
		}
		if current.AffiliationEnd.Valid {
			// Already ended: no-op — the historical end date is never
			// re-stamped (离职不删除历史; a repeated remove must not
			// rewrite when the member departed).
			return nil
		}
		if current.Role == string(domain.OrgRoleOwner) {
			if err := refuseLastOwnerRemoval(ctx, q, oID); err != nil {
				return err
			}
		}
		if _, err := q.EndOrganizationAffiliation(ctx, sqlc.EndOrganizationAffiliationParams{
			OrganizationID: oID,
			UserID:         uID,
			AffiliationEnd: dateToPG(end),
		}); err != nil {
			return err
		}
		return appendAudit(ctx, q, domain.AuditEntry{
			Action:         domain.ActionOrgMemberRemoved,
			TargetRef:      "user:" + userID,
			OrganizationID: orgID,
			AfterSummary: map[string]any{
				"affiliation_end": end,
			},
		})
	})
	if err != nil {
		return mapMembershipWriteError(err)
	}
	return nil
}

// refuseLastOwnerRemoval returns ErrLastOwner when userID's membership is
// the only active owner (called while the organization row is locked).
func refuseLastOwnerRemoval(ctx context.Context, q *sqlc.Queries, orgID pgtype.UUID) error {
	n, err := q.CountActiveOrganizationOwners(ctx, orgID)
	if err != nil {
		return err
	}
	if n <= 1 {
		return orgs.ErrLastOwner
	}
	return nil
}

// mapOrgWriteError translates organization-table driver errors.
func mapOrgWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "slug"):
			return orgs.ErrSlugTaken
		case pgErr.Code == "23503":
			return orgs.ErrOrgNotFound
		}
	}
	return fmt.Errorf("persistence: write organization: %w", err)
}

// mapMembershipWriteError passes the store sentinels through and wraps the
// rest.
func mapMembershipWriteError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, orgs.ErrOrgNotFound),
		errors.Is(err, orgs.ErrMemberNotFound),
		errors.Is(err, orgs.ErrLastOwner),
		errors.Is(err, orgs.ErrAlreadyMember),
		errors.Is(err, orgs.ErrUserNotFound),
		errors.Is(err, orgs.ErrOrgDeactivated):
		return err
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23505" && strings.Contains(pgErr.ConstraintName, "pkey"):
			return orgs.ErrAlreadyMember
		case pgErr.Code == "23503" && strings.Contains(pgErr.ConstraintName, "user_id"):
			return orgs.ErrUserNotFound
		case pgErr.Code == "23503" && strings.Contains(pgErr.ConstraintName, "organization_id"):
			return orgs.ErrOrgNotFound
		}
	}
	return fmt.Errorf("persistence: write membership: %w", err)
}

// orgFromRow converts a sqlc organization row to the domain value.
func orgFromRow(row sqlc.Organization) domain.Organization {
	return domain.Organization{
		ID:            pgUUIDToText(row.ID),
		Slug:          row.Slug,
		Name:          row.Name,
		Description:   nullStringToValue(row.Description),
		CreatedAt:     row.CreatedAt.Time,
		DeactivatedAt: timestamptzPtr(row.DeactivatedAt),
	}
}

// membershipFromRow converts a sqlc membership row to the domain value.
func membershipFromRow(row sqlc.OrganizationMembership) domain.OrganizationMembership {
	return domain.OrganizationMembership{
		OrganizationID:   pgUUIDToText(row.OrganizationID),
		UserID:           pgUUIDToText(row.UserID),
		Role:             domain.OrgRole(row.Role),
		AffiliationStart: dateFromPG(row.AffiliationStart),
		AffiliationEnd:   datePtrFromPG(row.AffiliationEnd),
		Verified:         row.Verified,
	}
}

// textUUID parses a uuid text form (the domain representation) into the
// pgx type.
func textUUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return pgtype.UUID{}, fmt.Errorf("invalid uuid %q: %w", s, err)
	}
	return u, nil
}

// twoUUIDs parses a pair of ids and fails when either is not a uuid.
func twoUUIDs(a, b string) (pgtype.UUID, pgtype.UUID, error) {
	ua, err := textUUID(a)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	ub, err := textUUID(b)
	if err != nil {
		return pgtype.UUID{}, pgtype.UUID{}, err
	}
	return ua, ub, nil
}

// pgUUIDToText renders a pgx uuid in the canonical 8-4-4-4-12 text form.
func pgUUIDToText(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// dateToPG converts a calendar date to the pgx date type. Zero times are
// sent as NULL (affiliation_end may be open).
func dateToPG(t time.Time) pgtype.Date {
	if t.IsZero() {
		return pgtype.Date{}
	}
	return pgtype.Date{Time: t, Valid: true}
}

// dateFromPG converts a pgx date to the domain calendar date.
func dateFromPG(d pgtype.Date) time.Time {
	if !d.Valid {
		return time.Time{}
	}
	return d.Time
}

// datePtrFromPG converts a nullable pgx date to a *time.Time.
func datePtrFromPG(d pgtype.Date) *time.Time {
	if !d.Valid {
		return nil
	}
	t := d.Time
	return &t
}

// timestamptzPtr converts a nullable timestamptz to *time.Time.
func timestamptzPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}

// nullString converts a Go string to a nullable SQL string (empty → NULL).
func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nullStringToValue converts a nullable SQL string to a Go string.
func nullStringToValue(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

var _ orgs.OrgStore = (*OrgStore)(nil)
