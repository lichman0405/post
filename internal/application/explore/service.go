package explore

import (
	"context"
	"fmt"
)

// Service is the Explore read use case: it reads the six dimensions and
// builds the index (docs/52: the application orchestrates, the transport
// translates). It has no actor parameter because it has no per-caller
// variant to compute — the index is the same answer for anonymous callers
// and signed-in ones alike, exactly like the asset hub's browse list
// (T0709). That property is what makes it safe to serve without a session.
type Service struct {
	reader Reader
}

// NewService wires the service over its reader.
func NewService(reader Reader) *Service {
	return &Service{reader: reader}
}

// Index reads every section and returns the surface's answer.
//
// A section that cannot be read fails the whole read (ErrStore). The
// alternative — rendering the sections that worked and an empty one for the
// rest — would answer "there is nothing here" for a dimension that might be
// full, which is a wrong answer wearing the shape of a complete one; the
// transport answers 503 instead, and the page shows an error state rather
// than a short list (docs/51: every page designs its empty and error
// states).
func (s *Service) Index(ctx context.Context) (Index, error) {
	in, err := s.read(ctx)
	if err != nil {
		return Index{}, err
	}
	return BuildIndex(in), nil
}

// read runs the six reads in order.
//
// They are sequential rather than concurrent on purpose: each is a bounded
// read of a few dozen rows over one pool, so the fan-out this could buy is
// dwarfed by the pool's own multiplexing, and a sequential read is the one
// whose failure the error above can name.
func (s *Service) read(ctx context.Context) (Input, error) {
	projects, err := s.reader.ListPublicProjects(ctx)
	if err != nil {
		return Input{}, fmt.Errorf("%w: projects: %w", ErrStore, err)
	}
	assetItems, err := s.reader.ListPublicAssets(ctx)
	if err != nil {
		return Input{}, fmt.Errorf("%w: assets: %w", ErrStore, err)
	}
	knowledge, err := s.reader.ListPublishedKnowledge(ctx)
	if err != nil {
		return Input{}, fmt.Errorf("%w: knowledge: %w", ErrStore, err)
	}
	people, err := s.reader.ListPublicPeople(ctx)
	if err != nil {
		return Input{}, fmt.Errorf("%w: people: %w", ErrStore, err)
	}
	organizations, err := s.reader.ListPublicOrganizations(ctx)
	if err != nil {
		return Input{}, fmt.Errorf("%w: organizations: %w", ErrStore, err)
	}
	contributions, err := s.reader.ListPublicContributions(ctx)
	if err != nil {
		return Input{}, fmt.Errorf("%w: contributions: %w", ErrStore, err)
	}
	return Input{
		Projects:      projects,
		Assets:        assetItems,
		Knowledge:     knowledge,
		People:        people,
		Organizations: organizations,
		Contributions: contributions,
	}, nil
}
