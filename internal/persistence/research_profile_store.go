package persistence

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lichman0405/post/internal/application/researchprofile"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// ResearchProfileStore is the production researchprofile.Reader adapter over
// PostgreSQL (T0808): the ten reads of the Research Profile and the
// Organization Profile, over the canonical queries of
// internal/persistence/queries/research_profile.sql.
//
// Each query states the render predicate of the dimension it feeds, with the
// LIMIT after it, so a bounded window is counted in rows these surfaces may
// actually RENDER (the SQL file's header records the split, and
// tests/integration's window test is the reason). Every value a predicate
// filtered on still comes back with the row: the decision about what may be
// rendered belongs to internal/application/researchprofile, where the rules
// have unit tests that name the document each one comes from, and that model
// re-checks every row rather than trusting what a query returned. So this
// adapter neither decides anything nor hides anything: it maps rows.
//
// Every method is a READ. Nothing here writes a row or takes a lock —
// contribution_events is append-only (00014) and this surface only ever
// looks.
type ResearchProfileStore struct {
	queries *sqlc.Queries
}

// NewResearchProfileStore wires the reader over any sqlc executor — the
// production value is the pgx pool cmd/api already builds, and the
// integration suite hands it a pool whose connections are read-only from the
// moment they connect (tests/integration), so "the profile cannot write" is
// a property of the server rather than a promise about this code.
func NewResearchProfileStore(db sqlc.DBTX) *ResearchProfileStore {
	return &ResearchProfileStore{queries: sqlc.New(db)}
}

// rowLimit is the bound every list read passes to @row_limit
// (researchprofile.FetchLimit). It is a read bound and never a rendered
// number: the model renders at most RenderLimit of what comes back, and no
// total is disclosed anywhere.
const rowLimit = researchprofile.FetchLimit

var _ researchprofile.Reader = (*ResearchProfileStore)(nil)

// GetPerson implements researchprofile.Reader. ok is false for an unknown id
// AND for a value that cannot be a uuid at all: users.id is a uuid, so a
// string outside that shape can never name an account and is answered the
// same way as one that names nothing (PostgreSQL's 22P02 is caught rather
// than raised) — the same reading ProfileStore.GetByUserID takes for the same
// reason, and it keeps a bad path segment from becoming a 500.
func (s *ResearchProfileStore) GetPerson(ctx context.Context, userID string) (researchprofile.PersonRow, bool, error) {
	id, err := textUUID(userID)
	if err != nil {
		return researchprofile.PersonRow{}, false, nil
	}
	row, err := s.queries.GetResearchProfilePerson(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return researchprofile.PersonRow{}, false, nil
	}
	if err != nil {
		return researchprofile.PersonRow{}, false, fmt.Errorf("persistence: get research profile person: %w", err)
	}
	return researchprofile.PersonRow{
		ID:          pgUUIDToText(row.ID),
		Handle:      row.Handle,
		DisplayName: row.DisplayName,
		Bio:         row.Bio,
		DisabledAt:  nullableInstant(row.DisabledAt),
	}, true, nil
}

// ListAffiliations implements researchprofile.Reader.
func (s *ResearchProfileStore) ListAffiliations(ctx context.Context, userID string) ([]researchprofile.AffiliationRow, error) {
	id, err := textUUID(userID)
	if err != nil {
		return nil, nil
	}
	rows, err := s.queries.ListPersonAffiliations(ctx, sqlc.ListPersonAffiliationsParams{
		UserID:   id,
		RowLimit: rowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list person affiliations: %w", err)
	}
	out := make([]researchprofile.AffiliationRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, researchprofile.AffiliationRow{
			OrganizationID:            pgUUIDToText(row.OrganizationID),
			OrganizationSlug:          row.Slug,
			OrganizationName:          row.Name,
			OrganizationDeactivatedAt: nullableInstant(row.DeactivatedAt),
			Role:                      row.Role,
			AffiliationStart:          nullableDay(row.AffiliationStart),
			AffiliationEnd:            nullableDay(row.AffiliationEnd),
			Verified:                  row.Verified,
		})
	}
	return out, nil
}

// ListContributions implements researchprofile.Reader.
func (s *ResearchProfileStore) ListContributions(ctx context.Context, userID string) ([]researchprofile.ContributionRow, error) {
	id, err := textUUID(userID)
	if err != nil {
		return nil, nil
	}
	rows, err := s.queries.ListActorContributions(ctx, sqlc.ListActorContributionsParams{
		ActorID:  id,
		RowLimit: rowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list actor contributions: %w", err)
	}
	return contributionsFromActorRows(rows), nil
}

// ListOrganizationActivity implements researchprofile.Reader. It shares the
// column list — and therefore the scanner — with ListContributions above; the
// two reads ask the same question about different keys.
func (s *ResearchProfileStore) ListOrganizationActivity(ctx context.Context, organizationID string) ([]researchprofile.ContributionRow, error) {
	id, err := textUUID(organizationID)
	if err != nil {
		return nil, nil
	}
	rows, err := s.queries.ListOrganizationActivity(ctx, sqlc.ListOrganizationActivityParams{
		OrganizationID: id,
		RowLimit:       rowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list organization activity: %w", err)
	}
	return contributionsFromOrganizationRows(rows), nil
}

// ListPersonAssets implements researchprofile.Reader.
func (s *ResearchProfileStore) ListPersonAssets(ctx context.Context, userID string) ([]researchprofile.AssetCreditRow, error) {
	id, err := textUUID(userID)
	if err != nil {
		return nil, nil
	}
	rows, err := s.queries.ListPersonAssetCredits(ctx, sqlc.ListPersonAssetCreditsParams{
		UserID:   id,
		RowLimit: rowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list person asset credits: %w", err)
	}
	out := make([]researchprofile.AssetCreditRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, researchprofile.AssetCreditRow{
			PID:               row.Pid,
			Title:             row.Title,
			AssetType:         row.AssetType,
			Version:           row.Version,
			VersionVisibility: row.Visibility,
			Role:              row.Role,
			ProjectID:         row.RaOriginProjectID,
			ProjectSlug:       row.Slug,
			ProjectName:       row.Name,
			ProjectVisibility: row.Visibility_2,
			PublishedAt:       row.PublishedAt.Time.UTC(),
		})
	}
	return out, nil
}

// ListOrganizationAssets implements researchprofile.Reader.
func (s *ResearchProfileStore) ListOrganizationAssets(ctx context.Context, organizationID string) ([]researchprofile.AssetCreditRow, error) {
	id, err := textUUID(organizationID)
	if err != nil {
		return nil, nil
	}
	rows, err := s.queries.ListOrganizationAssetCredits(ctx, sqlc.ListOrganizationAssetCreditsParams{
		OrganizationID: id,
		RowLimit:       rowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list organization asset credits: %w", err)
	}
	out := make([]researchprofile.AssetCreditRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, researchprofile.AssetCreditRow{
			PID:               row.Pid,
			Title:             row.Title,
			AssetType:         row.AssetType,
			Version:           row.Version,
			VersionVisibility: row.Visibility,
			// The query selects an empty role literal: this surface reads a
			// version's origin, not who is credited on it.
			ProjectID:         row.RaOriginProjectID,
			ProjectSlug:       row.Slug,
			ProjectName:       row.Name,
			ProjectVisibility: row.Visibility_2,
			PublishedAt:       row.PublishedAt.Time.UTC(),
		})
	}
	return out, nil
}

// ListReuses implements researchprofile.Reader.
func (s *ResearchProfileStore) ListReuses(ctx context.Context, userID string) ([]researchprofile.ReuseRow, error) {
	id, err := textUUID(userID)
	if err != nil {
		return nil, nil
	}
	rows, err := s.queries.ListPersonReuses(ctx, sqlc.ListPersonReusesParams{
		UserID:   id,
		RowLimit: rowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list person reuses: %w", err)
	}
	out := make([]researchprofile.ReuseRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, researchprofile.ReuseRow{
			PID:                      row.Pid,
			Title:                    row.Title,
			AssetType:                row.AssetType,
			Version:                  row.Version,
			VersionVisibility:        row.VersionVisibility,
			VersionProjectVisibility: row.VersionProjectVisibility,
			UsageVisibility:          row.VisibilityOfUsage,
			DependencyType:           row.DependencyType,
			DeclaredAt:               row.CreatedAt.Time.UTC(),
			ProjectID:                row.ProjectID,
			ProjectSlug:              row.Slug,
			ProjectName:              row.Name,
			ProjectVisibility:        row.Visibility,
		})
	}
	return out, nil
}

// ListReproductions implements researchprofile.Reader.
func (s *ResearchProfileStore) ListReproductions(ctx context.Context, userID string) ([]researchprofile.ReproductionRow, error) {
	id, err := textUUID(userID)
	if err != nil {
		return nil, nil
	}
	rows, err := s.queries.ListPersonReproductions(ctx, sqlc.ListPersonReproductionsParams{
		UserID:   id,
		RowLimit: rowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list person reproductions: %w", err)
	}
	out := make([]researchprofile.ReproductionRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, researchprofile.ReproductionRow{
			Relation:            row.RelationType,
			ReviewState:         row.ReviewState,
			CreatedAt:           row.CreatedAt.Time.UTC(),
			AssertionVisibility: row.AssertionVisibility,
			ProjectID:           row.EaProjectID,
			ProjectSlug:         row.Slug,
			ProjectName:         row.Name,
			ProjectVisibility:   row.Visibility,
		})
	}
	return out, nil
}

// GetOrganizationBySlug implements researchprofile.Reader.
//
// ok is false for an unknown slug only. A DEACTIVATED organization comes back
// with ok true and its deactivated_at set, because the two are different
// facts and the model is where the rule about rendering it lives
// (researchprofile.OrganizationActive) — the store does not decide what may
// be shown.
func (s *ResearchProfileStore) GetOrganizationBySlug(ctx context.Context, slug string) (researchprofile.OrganizationRow, bool, error) {
	org, err := s.queries.GetOrganizationBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return researchprofile.OrganizationRow{}, false, nil
	}
	if err != nil {
		return researchprofile.OrganizationRow{}, false, fmt.Errorf("persistence: get organization by slug: %w", err)
	}
	description := ""
	if org.Description != nil {
		description = *org.Description
	}
	return researchprofile.OrganizationRow{
		ID:            pgUUIDToText(org.ID),
		Slug:          org.Slug,
		Name:          org.Name,
		Description:   description,
		DeactivatedAt: nullableInstant(org.DeactivatedAt),
	}, true, nil
}

// ListOrganizationProjects implements researchprofile.Reader.
func (s *ResearchProfileStore) ListOrganizationProjects(ctx context.Context, organizationID string) ([]researchprofile.ProjectRow, error) {
	id, err := textUUID(organizationID)
	if err != nil {
		return nil, nil
	}
	rows, err := s.queries.ListOrganizationProjects(ctx, sqlc.ListOrganizationProjectsParams{
		OrganizationID: id,
		RowLimit:       rowLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list organization projects: %w", err)
	}
	out := make([]researchprofile.ProjectRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, researchprofile.ProjectRow{
			ID:             pgUUIDToText(row.ID),
			Slug:           row.Slug,
			Name:           row.Name,
			Purpose:        row.Purpose,
			ActivityStatus: row.ActivityStatus,
			Visibility:     row.Visibility,
		})
	}
	return out, nil
}

// contributionsFromActorRows maps the person-side read.
func contributionsFromActorRows(rows []sqlc.ListActorContributionsRow) []researchprofile.ContributionRow {
	out := make([]researchprofile.ContributionRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, researchprofile.ContributionRow{
			EventType:         row.EventType,
			RoleCodes:         row.RoleCodes,
			OccurredAt:        row.OccurredAt.Time.UTC(),
			Accepted:          row.AcceptedContext,
			Released:          row.ReleasedContext,
			Via:               row.Via,
			ProjectID:         row.ProjectID,
			ProjectSlug:       row.Slug,
			ProjectName:       row.Name,
			ProjectVisibility: row.Visibility,
			ActorID:           pgUUIDToText(row.ActorID),
			ActorHandle:       row.Handle,
			ActorDisplayName:  row.DisplayName,
			ActorDisabledAt:   nullableInstant(row.DisabledAt),
		})
	}
	return out
}

// contributionsFromOrganizationRows maps the organization-side read, which
// returns the same columns (see the SQL file).
func contributionsFromOrganizationRows(rows []sqlc.ListOrganizationActivityRow) []researchprofile.ContributionRow {
	out := make([]researchprofile.ContributionRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, researchprofile.ContributionRow{
			EventType:         row.EventType,
			RoleCodes:         row.RoleCodes,
			OccurredAt:        row.OccurredAt.Time.UTC(),
			Accepted:          row.AcceptedContext,
			Released:          row.ReleasedContext,
			Via:               row.Via,
			ProjectID:         row.ProjectID,
			ProjectSlug:       row.Slug,
			ProjectName:       row.Name,
			ProjectVisibility: row.Visibility,
			ActorID:           pgUUIDToText(row.ActorID),
			ActorHandle:       row.Handle,
			ActorDisplayName:  row.DisplayName,
			ActorDisabledAt:   nullableInstant(row.DisabledAt),
		})
	}
	return out
}

// nullableInstant renders a nullable timestamptz as a pointer, and NULL as
// nil — never as the zero time, which is a real instant and would read as one
// (year 1 is "long disabled", not "not disabled").
func nullableInstant(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	v := ts.Time.UTC()
	return &v
}

// nullableDay renders a nullable `date` as a pointer, and NULL as nil. The
// value is a calendar date: it carries no time of day and none is invented
// here (internal/domain/affiliation.go is the convention).
func nullableDay(d pgtype.Date) *time.Time {
	if !d.Valid {
		return nil
	}
	v := d.Time
	return &v
}
