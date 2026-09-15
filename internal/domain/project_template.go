package domain

import (
	"encoding/json"
	"time"
)

// Project templates (T0214, docs/03 §5 Blueprint): an official template is
// a platform-owned, versioned set of DEFAULTS a user can create a project
// from. The template initializes the project once — the project row
// (purpose/visibility presets), schema profile registrations, the first
// project policy version (review defaults) and the initial research-map
// questions — and never controls the project afterwards: everything
// applied is an ordinary project-owned row the owner evolves through the
// normal surfaces. A template upgrade is a new template version; existing
// projects record the id + version they were created from
// (project_template_instantiations, migration 00056) and are never
// touched by catalog changes.
//
// The canonical table lives in infra/migrations/00056_project_template_
// instantiations.sql; the catalog itself is platform code (official
// templates are not user data), so there is no template table.

// ProjectTemplate is one official template version: the defaults it
// initializes a new project with. Every field the caller of the
// instantiation API supplies wins over the template's default — the
// template fills only what the caller left unset.
type ProjectTemplate struct {
	// ID is the stable template slug, e.g. "materials-discovery".
	ID string
	// Version is the template version label (e.g. "v1"). A catalog holds
	// at most one entry per (id, version); an upgrade is a new version.
	Version string
	// Name is the human display name.
	Name string
	// Description explains what the template is for (rendered by the
	// catalog UI).
	Description string
	// Project holds the project-row defaults.
	Project TemplateProjectDefaults
	// Schemas lists the schema profiles the template registers on the new
	// project (T0213): each extends an official base schema with
	// workflow-specific typed fields.
	Schemas []TemplateSchemaProfile
	// Review holds the governance defaults: the rules of the project's
	// FIRST policy version (docs/12 §5). An empty Rules set writes no
	// policy — a template may default nothing.
	Review TemplateReviewDefaults
	// Map holds the research-map seeds: the initial research questions
	// (the Research page's question axis, T0211). An empty slice seeds
	// nothing — the project starts as empty as a template-less one.
	Map []TemplateQuestion
}

// TemplateProjectDefaults is the project-row part of a template: the
// fields a template may pre-fill when the caller left them unset.
type TemplateProjectDefaults struct {
	// Name is the suggested project display name; "" means no default.
	Name string
	// Purpose is the suggested one-sentence research goal; "" means no
	// default (the create still requires a purpose from the caller).
	Purpose string
	// Visibility is the suggested visibility preset; "" means no default
	// (the create still requires an explicit visibility from the caller).
	Visibility ProjectVisibility
}

// TemplateSchemaProfile is one schema profile definition inside a
// template. It is the T0213 RegisterInput minus the caller-actor parts:
// the instantiation registers it as version "1" of the project's
// namespaced profile id.
type TemplateSchemaProfile struct {
	// Name is the profile name token (lowercase snake_case); the schema
	// id is derived as "project:<project_id>:<name>".
	Name string
	// Base pins the official base schema, by registry id + version.
	Base SchemaRef
	// Properties are the custom field definitions (JSON Schema fragments);
	// names must be NEW fields — a profile extends the base, never
	// redefines base fields (the registration enforces this).
	Properties map[string]any
	// Required lists additional required field names, each among the
	// merged (base + custom) properties.
	Required []string
}

// SchemaRef is a {id, version} schema pin (the same shape the scientific
// object version rows store).
type SchemaRef struct {
	ID      string
	Version string
}

// TemplateReviewDefaults is the governance part of a template: the rules
// of the project's first policy version. Values are raw JSON, exactly the
// domain.Policy shape — the same rule keys, kinds and strictness orders
// (docs/12 §5), set as defaults, never as permanent control.
type TemplateReviewDefaults struct {
	Rules map[string]json.RawMessage
}

// TemplateQuestion is one research-map seed: an initial research question
// created on the new project's first (main) branch as a normal scientific
// object. Children nest the question tree; the instantiation links each
// child to its parent through the payload's parent_question_id.
type TemplateQuestion struct {
	// Statement is the question text (the payload statement field, the
	// version title derives from it).
	Statement string
	// Purpose is the question's payload purpose (optional).
	Purpose string
	// Children are the nested sub-questions, seeded after their parent.
	Children []TemplateQuestion
}

// TemplateInstantiation is the provenance record of one project creation
// from a template: which template id + version the project was created
// from, and when (project_template_instantiations, migration 00056). It
// is a recorded fact only — nothing reads it back to re-apply defaults.
type TemplateInstantiation struct {
	ID              string
	ProjectID       string
	TemplateID      string
	TemplateVersion string
	TemplateName    string
	CreatedBy       string
	CreatedAt       time.Time
}

// maxTemplateIDLen / maxTemplateVersionLen bound the recorded labels
// (generous but finite — they mirror the table CHECK constraints).
const (
	maxTemplateIDLen      = 64
	maxTemplateVersionLen = 64
)

// ValidTemplateID reports whether id has the template-id shape: 1..64
// characters of [a-z0-9-], starting with a letter or digit (the table's
// CHECK constraint, mirrored so the service refuses before the store).
func ValidTemplateID(id string) bool {
	if id == "" || len(id) > maxTemplateIDLen {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// ValidTemplateVersion reports whether v has the version-label shape:
// 1..64 characters of [A-Za-z0-9._-] (the table's CHECK constraint).
func ValidTemplateVersion(v string) bool {
	if v == "" || len(v) > maxTemplateVersionLen {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
