package explore_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/explore"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/domain"
)

// fakeReader is the six-read port with one canned answer per read, and one
// failure per read the test wants to fail.
type fakeReader struct {
	projects      []domain.Project
	assets        []assets.BrowseItem
	knowledge     []explore.KnowledgeRow
	people        []explore.PersonRow
	organizations []explore.OrganizationRow
	contributions []contribution.ContributionOpportunity

	failOn string // the read that fails, by method name

	calls []string
}

func (f *fakeReader) note(name string) error {
	f.calls = append(f.calls, name)
	if f.failOn == name {
		return errors.New("store: connection refused")
	}
	return nil
}

func (f *fakeReader) ListPublicProjects(context.Context) ([]domain.Project, error) {
	return f.projects, f.note("projects")
}

func (f *fakeReader) ListPublicAssets(context.Context) ([]assets.BrowseItem, error) {
	return f.assets, f.note("assets")
}

func (f *fakeReader) ListPublishedKnowledge(context.Context) ([]explore.KnowledgeRow, error) {
	return f.knowledge, f.note("knowledge")
}

func (f *fakeReader) ListPublicPeople(context.Context) ([]explore.PersonRow, error) {
	return f.people, f.note("people")
}

func (f *fakeReader) ListPublicOrganizations(context.Context) ([]explore.OrganizationRow, error) {
	return f.organizations, f.note("organizations")
}

func (f *fakeReader) ListPublicContributions(context.Context) ([]contribution.ContributionOpportunity, error) {
	return f.contributions, f.note("contributions")
}

// TestIndexAggregatesEverySection is the requirement "Projects/Assets/
// Knowledge/People/Organizations/Open Contributions tabs" at the service
// level: one read per dimension, and every read's rows reach the answer.
func TestIndexAggregatesEverySection(t *testing.T) {
	reader := &fakeReader{
		projects:      []domain.Project{publicProject("p", 1)},
		assets:        []assets.BrowseItem{assetItem("a", 1)},
		knowledge:     []explore.KnowledgeRow{publishedKnowledge("k", "p", 1)},
		people:        []explore.PersonRow{{ID: "u", Handle: "u", DisplayName: "U", CreatedAt: at(1)}},
		organizations: []explore.OrganizationRow{{ID: "org", Slug: "org", Name: "Org", CreatedAt: at(1)}},
		contributions: []contribution.ContributionOpportunity{
			opportunity("c", "p", 1, contribution.OpportunityVisibilityPublic, contribution.OpportunityStateOpen),
		},
	}

	index, err := explore.NewService(reader).Index(context.Background())
	if err != nil {
		t.Fatalf("Index: %v", err)
	}

	wantCalls := []string{"projects", "assets", "knowledge", "people", "organizations", "contributions"}
	if !equalStrings(reader.calls, wantCalls) {
		t.Fatalf("reads = %v, want one per dimension %v", reader.calls, wantCalls)
	}
	empty := []string{"Projects", "Assets", "Knowledge", "People", "Organizations", "Contributions"}
	counts := map[string]int{
		"Projects":      len(index.Projects.Items),
		"Assets":        len(index.Assets.Items),
		"Knowledge":     len(index.Knowledge.Items),
		"People":        len(index.People.Items),
		"Organizations": len(index.Organizations.Items),
		"Contributions": len(index.Contributions.Items),
	}
	for _, name := range empty {
		if counts[name] == 0 {
			t.Errorf("%s section is empty although its reader returned a row", name)
		}
	}
}

// TestAFailedSectionFailsTheIndex proves the service never serves a partial
// index: every one of the six reads, when it fails, is a 503 at the edge —
// not a short list that a reader would take for "there is nothing there"
// (docs/51: empty and error are different states).
func TestAFailedSectionFailsTheIndex(t *testing.T) {
	for _, name := range []string{"projects", "assets", "knowledge", "people", "organizations", "contributions"} {
		t.Run(name, func(t *testing.T) {
			reader := &fakeReader{
				projects:  []domain.Project{publicProject("p", 1)},
				failOn:    name,
				knowledge: []explore.KnowledgeRow{publishedKnowledge("k", "p", 1)},
			}

			index, err := explore.NewService(reader).Index(context.Background())
			if err == nil {
				t.Fatalf("a failed %s read answered an index: %+v", name, index)
			}
			if !errors.Is(err, explore.ErrStore) {
				t.Fatalf("error = %v, want it to wrap explore.ErrStore", err)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name the section that failed", err)
			}
			// The zero index is returned, so a caller that ignored the error
			// could not render a partial answer as a complete one.
			if len(index.Projects.Items)+len(index.Assets.Items)+len(index.Knowledge.Items)+
				len(index.People.Items)+len(index.Organizations.Items)+len(index.Contributions.Items) != 0 {
				t.Errorf("a failed read returned rows anyway: %+v", index)
			}
		})
	}
}

// TestIndexTakesNoInputAtAll pins the shape that makes the surface anonymous: the
// use case has no principal parameter, so there is no caller for whom the
// answer could differ (the transport therefore serves it without a session).
func TestIndexTakesNoInputAtAll(t *testing.T) {
	reader := &fakeReader{}
	index, err := explore.NewService(reader).Index(context.Background())
	if err != nil {
		t.Fatalf("Index on an empty store: %v", err)
	}
	// An empty network renders six empty sections, each with its tab — never
	// a nil Items list, so a client iterating items need not null-check.
	for _, got := range [][]int{
		{len(index.Projects.Items)}, {len(index.Assets.Items)}, {len(index.Knowledge.Items)},
		{len(index.People.Items)}, {len(index.Organizations.Items)}, {len(index.Contributions.Items)},
	} {
		if got[0] != 0 {
			t.Fatalf("an empty store rendered %d rows", got[0])
		}
	}
	if index.People.Items == nil || index.Organizations.Items == nil || index.Knowledge.Items == nil {
		t.Errorf("an empty section rendered as JSON null instead of []")
	}
}
