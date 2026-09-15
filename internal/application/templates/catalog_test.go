package templates

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/domain"
)

// minimalEntry builds one valid catalog entry so tests only vary the field
// under test.
func minimalEntry() domain.ProjectTemplate {
	return domain.ProjectTemplate{
		ID:          "minimal",
		Version:     "v1",
		Name:        "Minimal Template",
		Description: "A minimal valid template for tests.",
	}
}

// TestOfficialCatalogValid runs the catalog validator over every official
// entry: the platform must not ship a template the service would refuse.
func TestOfficialCatalogValid(t *testing.T) {
	for _, e := range Official().List() {
		if err := validateEntry(e); err != nil {
			t.Errorf("official template %s@%s fails catalog validation: %v", e.ID, e.Version, err)
		}
	}
	if got := len(Official().LatestIDs()); got != 6 {
		t.Errorf("official catalog holds %d template ids, want 6", got)
	}
}

// TestOfficialCatalogCoversRequirement pins the six V1 template ids the
// spec names, so a rename/removal fails loudly.
func TestOfficialCatalogCoversRequirement(t *testing.T) {
	want := []string{
		"materials-discovery",
		"computational-screening",
		"experimental-validation",
		"paper-reproduction",
		"dataset-construction",
		"benchmarking",
	}
	got := map[string]bool{}
	for _, e := range Official().LatestIDs() {
		got[e.ID] = true
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("official catalog is missing template %q", id)
		}
	}
}

// TestOfficialTemplateDefaultsAreDefaults pins the "defaults only, never
// control" shape: every official profile pins an official base schema, and
// no official template sets required_schema_profiles (that rule would
// govern future object writes — a template initializes, it never controls).
func TestOfficialTemplateDefaultsAreDefaults(t *testing.T) {
	for _, e := range Official().List() {
		for _, p := range e.Schemas {
			if p.Base.ID == "" || p.Base.Version == "" {
				t.Errorf("%s@%s: profile %s has no base pin", e.ID, e.Version, p.Name)
			}
			if len(p.Properties) == 0 {
				t.Errorf("%s@%s: profile %s defines no custom fields", e.ID, e.Version, p.Name)
			}
		}
		if raw, ok := e.Review.Rules["required_schema_profiles"]; ok {
			t.Errorf("%s@%s: sets required_schema_profiles (%s) — that would control future writes, a template must only initialize", e.ID, e.Version, raw)
		}
		if e.Project.Visibility != "" && e.Project.Visibility != domain.VisibilityPrivate {
			t.Errorf("%s@%s: official templates must default private projects (no accidental public state), got %s", e.ID, e.Version, e.Project.Visibility)
		}
	}
}

// TestResolveLatestVsPinned covers the acceptance property "模板升级不会自
// 动改已有项目" at the catalog level: resolving without a version always
// returns the newest entry, resolving with a version returns exactly that
// pair — so an existing project pinned to an old version keeps resolving
// the old definition forever.
func TestResolveLatestVsPinned(t *testing.T) {
	v2 := minimalEntry()
	v2.Version = "v2"
	v2.Description = "The upgraded defaults."
	c := NewCatalogMust(minimalEntry(), v2)

	latest, err := c.Resolve("minimal", "")
	if err != nil {
		t.Fatalf("Resolve(latest) failed: %v", err)
	}
	if latest.Version != "v1" {
		t.Errorf("Resolve(\"\") = %s, want the first-declared (newest) v1", latest.Version)
	}

	pinned, err := c.Resolve("minimal", "v2")
	if err != nil {
		t.Fatalf("Resolve(v2) failed: %v", err)
	}
	if pinned.Description != "The upgraded defaults." {
		t.Errorf("Resolve(v2).Description = %q, want the v2 definition", pinned.Description)
	}

	old, err := c.Resolve("minimal", "v1")
	if err != nil {
		t.Fatalf("Resolve(v1) failed: %v", err)
	}
	if old.Description != "A minimal valid template for tests." {
		t.Errorf("Resolve(v1).Description = %q, want the v1 definition", old.Description)
	}

	if _, err := c.Resolve("unknown", ""); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("Resolve(unknown) = %v, want ErrTemplateNotFound", err)
	}
	if _, err := c.Resolve("minimal", "v9"); !errors.Is(err, ErrTemplateNotFound) {
		t.Errorf("Resolve(minimal@v9) = %v, want ErrTemplateNotFound", err)
	}
	if _, err := c.Resolve("NOT-VALID", ""); !errors.Is(err, ErrValidation) {
		t.Errorf("Resolve(NOT-VALID) = %v, want ErrValidation", err)
	}
}

func TestResolveFailsClosedForAmbiguousLatest(t *testing.T) {
	// Two versions of the same id must be declared newest-first; a catalog
	// that declares an id with the same version twice is refused.
	if _, err := NewCatalog(minimalEntry(), minimalEntry()); !errors.Is(err, ErrValidation) {
		t.Errorf("duplicate (id, version) = %v, want ErrValidation", err)
	}
}

func TestCatalogValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*domain.ProjectTemplate)
		wantErr error
	}{
		{"empty id", func(e *domain.ProjectTemplate) { e.ID = "" }, ErrValidation},
		{"bad id chars", func(e *domain.ProjectTemplate) { e.ID = "Bad_ID" }, ErrValidation},
		{"empty version", func(e *domain.ProjectTemplate) { e.Version = "" }, ErrValidation},
		{"bad version chars", func(e *domain.ProjectTemplate) { e.Version = "v 1" }, ErrValidation},
		{"empty name", func(e *domain.ProjectTemplate) { e.Name = "  " }, ErrValidation},
		{"empty description", func(e *domain.ProjectTemplate) { e.Description = "" }, ErrValidation},
		{"bad visibility", func(e *domain.ProjectTemplate) { e.Project.Visibility = "internal" }, ErrValidation},
		{"profile empty base", func(e *domain.ProjectTemplate) {
			e.Schemas = []domain.TemplateSchemaProfile{{Name: "material_ext"}}
		}, ErrValidation},
		{"profile non-official base", func(e *domain.ProjectTemplate) {
			e.Schemas = []domain.TemplateSchemaProfile{{Name: "material_ext", Base: domain.SchemaRef{ID: "project:x:custom", Version: "1"}}}
		}, ErrValidation},
		{"profile property not an object", func(e *domain.ProjectTemplate) {
			e.Schemas = []domain.TemplateSchemaProfile{{
				Name:       "material_ext",
				Base:       domain.SchemaRef{ID: "https://open-rd.example/schemas/material", Version: "1"},
				Properties: map[string]any{"bad": "not-an-object"},
			}}
		}, ErrValidation},
		{"profile required not among properties", func(e *domain.ProjectTemplate) {
			e.Schemas = []domain.TemplateSchemaProfile{{
				Name:       "material_ext",
				Base:       domain.SchemaRef{ID: "https://open-rd.example/schemas/material", Version: "1"},
				Properties: map[string]any{"f": map[string]any{"type": "string"}},
				Required:   []string{"missing"},
			}}
		}, ErrValidation},
		{"bad rule key shape", func(e *domain.ProjectTemplate) {
			e.Review.Rules = map[string]json.RawMessage{"Bad-Key": json.RawMessage(`true`)}
		}, ErrValidation},
		{"short question", func(e *domain.ProjectTemplate) {
			e.Map = []domain.TemplateQuestion{{Statement: "ab"}}
		}, ErrValidation},
		{"short child question", func(e *domain.ProjectTemplate) {
			e.Map = []domain.TemplateQuestion{{Statement: "parent", Children: []domain.TemplateQuestion{{Statement: "x"}}}}
		}, ErrValidation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := minimalEntry()
			tt.mutate(&e)
			if _, err := NewCatalog(e); !errors.Is(err, tt.wantErr) {
				t.Errorf("NewCatalog = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestLatestIDsSortedById(t *testing.T) {
	c := NewCatalogMust(
		domain.ProjectTemplate{ID: "zeta", Version: "v1", Name: "Z", Description: "z"},
		domain.ProjectTemplate{ID: "alpha", Version: "v1", Name: "A", Description: "a"},
		domain.ProjectTemplate{ID: "zeta", Version: "v2", Name: "Z", Description: "z2"},
	)
	got := c.LatestIDs()
	if len(got) != 2 {
		t.Fatalf("LatestIDs = %d entries, want 2", len(got))
	}
	if got[0].ID != "alpha" || got[1].ID != "zeta" {
		t.Errorf("LatestIDs order = [%s, %s], want [alpha, zeta]", got[0].ID, got[1].ID)
	}
	if got[1].Version != "v1" {
		t.Errorf("LatestIDs zeta version = %s, want the first-declared v1", got[1].Version)
	}
}
