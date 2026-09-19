package researchprofile

import (
	"context"
	"fmt"

	"github.com/lichman0405/post/internal/domain"
)

// Service is the two read use cases: a person's Research Profile and an
// organization's.
//
// It has no actor parameter, and that is a property of the surface rather
// than an omission: both profiles are the same answer for an anonymous
// caller and a signed-in one. Every dimension is public-or-absent, decided
// against the row's own state instead of against the caller's memberships —
// the reason internal/application/explore states for the same choice
// ("a section that answered differently per reader would ask N authorization
// questions about N third parties, which is itself the disclosure docs/23 §5
// forbids"). Whatever the API's guard lets through is therefore rendered
// identically, which is what makes it safe to serve without a session.
type Service struct {
	reader Reader
}

// NewService wires the service over its reader.
func NewService(reader Reader) *Service {
	return &Service{reader: reader}
}

// PersonProfile reads one person's Research Profile.
//
// An unknown id, an id that is not a uuid at all, and a DISABLED account all
// answer ErrNotFound (see ErrNotFound): the transport renders one 404 and the
// three cases cannot be told apart from outside.
func (s *Service) PersonProfile(ctx context.Context, userID string) (PersonProfile, error) {
	person, ok, err := s.reader.GetPerson(ctx, userID)
	if err != nil {
		return PersonProfile{}, fmt.Errorf("%w: person: %w", ErrStore, err)
	}
	if !ok || !PersonVisible(person) {
		return PersonProfile{}, ErrNotFound
	}

	// The five reads are SEQUENTIAL, and a failure in any one fails the whole
	// read rather than rendering the dimensions that worked. The alternative
	// answers "there is nothing here" for a dimension that might be full —
	// a wrong answer wearing the shape of a complete one (internal/application
	// /explore states the same rule for its sections) — and the profile is a
	// page whose empty state means something specific.
	affiliations, err := s.reader.ListAffiliations(ctx, person.ID)
	if err != nil {
		return PersonProfile{}, fmt.Errorf("%w: affiliations: %w", ErrStore, err)
	}
	contributions, err := s.reader.ListContributions(ctx, person.ID)
	if err != nil {
		return PersonProfile{}, fmt.Errorf("%w: contributions: %w", ErrStore, err)
	}
	assetCredits, err := s.reader.ListPersonAssets(ctx, person.ID)
	if err != nil {
		return PersonProfile{}, fmt.Errorf("%w: assets: %w", ErrStore, err)
	}
	reuse, err := s.reader.ListReuses(ctx, person.ID)
	if err != nil {
		return PersonProfile{}, fmt.Errorf("%w: reuse: %w", ErrStore, err)
	}
	reproductions, err := s.reader.ListReproductions(ctx, person.ID)
	if err != nil {
		return PersonProfile{}, fmt.Errorf("%w: reproductions: %w", ErrStore, err)
	}

	return BuildPersonProfile(PersonInput{
		Person:        person,
		Affiliations:  affiliations,
		Contributions: contributions,
		Assets:        assetCredits,
		Reuse:         reuse,
		Reproductions: reproductions,
	}), nil
}

// OrganizationProfile reads one organization's profile, addressed by its
// slug — the organization's stable public identity (internal/application
// /orgs: the slug is set at creation and is not mutable, which is what makes
// it the address a public page may use; the id stays the reference identity
// on the API's own management routes).
//
// A slug that cannot name an organization (empty, or outside the slug shape
// domain.ValidOrgSlug states) is ErrNotFound rather than a validation error:
// the transport answers the same existence-hiding 404 for it, so a caller
// cannot use the error code to probe which slugs are shaped like real ones.
func (s *Service) OrganizationProfile(ctx context.Context, slug string) (OrganizationProfile, error) {
	normalized := domain.NormalizeOrgSlug(slug)
	if !domain.ValidOrgSlug(normalized) {
		return OrganizationProfile{}, ErrNotFound
	}

	org, ok, err := s.reader.GetOrganizationBySlug(ctx, normalized)
	if err != nil {
		return OrganizationProfile{}, fmt.Errorf("%w: organization: %w", ErrStore, err)
	}
	if !ok || !OrganizationActive(org) {
		return OrganizationProfile{}, ErrNotFound
	}

	projects, err := s.reader.ListOrganizationProjects(ctx, org.ID)
	if err != nil {
		return OrganizationProfile{}, fmt.Errorf("%w: projects: %w", ErrStore, err)
	}
	activity, err := s.reader.ListOrganizationActivity(ctx, org.ID)
	if err != nil {
		return OrganizationProfile{}, fmt.Errorf("%w: activity: %w", ErrStore, err)
	}
	assets, err := s.reader.ListOrganizationAssets(ctx, org.ID)
	if err != nil {
		return OrganizationProfile{}, fmt.Errorf("%w: assets: %w", ErrStore, err)
	}

	return BuildOrganizationProfile(OrganizationInput{
		Organization: org,
		Projects:     projects,
		Activity:     activity,
		Assets:       assets,
	}), nil
}
