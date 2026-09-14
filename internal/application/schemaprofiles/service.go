package schemaprofiles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// Service orchestrates the schema profile use cases against the store port
// and the schema registry. It owns input validation (the port trusts, the
// service verifies) and maps store outcomes onto the package sentinels.
type Service struct {
	store    ProfileStore
	projects ProjectGate
	reg      *schemareg.Registry
}

// Deps wires the service. The registry and the project gate are required:
// an unwired service refuses at call time rather than guessing (fail
// closed, docs/12).
type Deps struct {
	Store    ProfileStore
	Projects ProjectGate
	Schemas  *schemareg.Registry
}

// NewService builds the service on the ports.
func NewService(deps Deps) *Service {
	return &Service{store: deps.Store, projects: deps.Projects, reg: deps.Schemas}
}

// Register registers one project schema profile version: it validates the
// request, resolves the actor's maintainer gate, generates the full profile
// document from the pinned base and the custom field definitions, registers
// it into the runtime registry and persists it (with its audit entry) as an
// immutable version row.
//
// Ordering. The registry check-and-set runs before the database insert: a
// conflicting re-registration (same id+version, different content) fails
// before anything is written, and the registry's mutex serializes
// concurrent in-process registrations — of two racers with different
// content exactly one wins the registry, the other answers
// ErrProfileVersionExists. A store failure after a successful registry
// registration can only leave an unpersisted entry when another process
// wrote the same id+version concurrently; the startup load (LoadAll)
// re-converges the registry to the database rows, so the divergence is
// transient and never survives a restart.
func (s *Service) Register(ctx context.Context, actor domain.User, projectID string, in RegisterInput) (domain.ProjectSchemaProfile, error) {
	if s.reg == nil {
		return domain.ProjectSchemaProfile{}, fmt.Errorf("%w: no schema registry configured", ErrStore)
	}
	if s.store == nil {
		return domain.ProjectSchemaProfile{}, fmt.Errorf("%w: no profile store configured", ErrStore)
	}
	// The actor identity is a precondition of the authorization check, so
	// its shape check runs first. Everything else validates AFTER the
	// maintainer gate (docs/45): an unauthorized caller answers the same
	// 403/404 whether its payload is well-formed or not — authorization
	// discloses nothing, and validation discloses nothing about a project
	// the caller may not touch.
	if actor.ID == "" {
		return domain.ProjectSchemaProfile{}, fmt.Errorf("%w: actor is required", ErrValidation)
	}
	if err := s.requireMaintainer(ctx, actor, projectID); err != nil {
		return domain.ProjectSchemaProfile{}, err
	}
	if !domain.ValidProfileName(in.Name) {
		return domain.ProjectSchemaProfile{}, fmt.Errorf("%w: name must be lowercase snake_case (a-z, 0-9, _)", ErrValidation)
	}
	if !domain.ValidProfileVersion(in.Version) {
		return domain.ProjectSchemaProfile{}, fmt.Errorf("%w: version must be 1-64 characters of A-Za-z0-9._-", ErrValidation)
	}
	base, err := s.resolveBase(in.Base)
	if err != nil {
		return domain.ProjectSchemaProfile{}, err
	}
	schemaID := "project:" + projectID + ":" + in.Name
	// One name, one base: every version registered under a name must
	// extend the same base schema. A v2 pinned to a different base would
	// silently change which object type the name governs, breaking
	// existing objects whose versions pin this schema id. The check reads
	// the append-only rows: once a version exists, every later
	// registration sees it, so the refusal converges (a brand-new name's
	// first versions are the only window, and the maintainer gate already
	// bound who may race it).
	existing, err := s.store.ListProfiles(ctx, projectID)
	if err != nil {
		return domain.ProjectSchemaProfile{}, wrapStoreError(err)
	}
	for _, p := range existing {
		if p.SchemaID == schemaID && (p.BaseSchemaID != base.ID || p.BaseSchemaVersion != base.Version) {
			return domain.ProjectSchemaProfile{}, fmt.Errorf("%w: schema %s already extends base %s v%s (version %s); version %s would extend base %s v%s — new versions of a name must extend the same base", ErrValidation, schemaID, p.BaseSchemaID, p.BaseSchemaVersion, p.Version, in.Version, base.ID, base.Version)
		}
	}
	doc, err := generateProfileDoc(base, schemaID, in.Name, in.Properties, in.Required)
	if err != nil {
		return domain.ProjectSchemaProfile{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	content, err := json.Marshal(doc)
	if err != nil {
		return domain.ProjectSchemaProfile{}, fmt.Errorf("%w: encoding the profile document: %v", ErrStore, err)
	}
	if _, err := s.reg.Register(schemaID, in.Version, content); err != nil {
		if errors.Is(err, schemareg.ErrAlreadyRegistered) {
			return domain.ProjectSchemaProfile{}, fmt.Errorf("%w: schema %s v%s is already registered (docs/21 §8: schema versions are immutable — register the new content under a new version): %v", ErrProfileVersionExists, schemaID, in.Version, err)
		}
		return domain.ProjectSchemaProfile{}, fmt.Errorf("%w: the generated profile is not a valid JSON Schema: %v", ErrValidation, err)
	}
	sum := sha256.Sum256(content)
	profile := domain.ProjectSchemaProfile{
		ProjectID:         projectID,
		SchemaID:          schemaID,
		Version:           in.Version,
		BaseSchemaID:      base.ID,
		BaseSchemaVersion: base.Version,
		Content:           string(content),
		ContentHash:       hex.EncodeToString(sum[:]),
		CreatedBy:         actor.ID,
	}
	audit, err := s.registerAudit(ctx, actor, projectID, profile)
	if err != nil {
		return domain.ProjectSchemaProfile{}, err
	}
	stored, err := s.store.RegisterProfile(ctx, profile, audit)
	if err != nil {
		if errors.Is(err, ErrProfileVersionExists) || errors.Is(err, ErrProjectNotFound) {
			return domain.ProjectSchemaProfile{}, err
		}
		return domain.ProjectSchemaProfile{}, wrapStoreError(err)
	}
	return stored, nil
}

// Get returns the profile's newest registered version, or
// ErrProfileNotFound. The read runs the project's visibility gate: the
// profile is exactly as visible as its project (T0106).
func (s *Service) Get(ctx context.Context, r projects.Reader, projectID, name string) (domain.ProjectSchemaProfile, error) {
	if s.store == nil {
		return domain.ProjectSchemaProfile{}, fmt.Errorf("%w: no profile store configured", ErrStore)
	}
	if err := s.requireRead(ctx, r, projectID); err != nil {
		return domain.ProjectSchemaProfile{}, err
	}
	return s.store.GetLatestProfile(ctx, projectID, "project:"+projectID+":"+name)
}

// GetVersion returns one profile version by name + version label — any
// age: old versions stay queryable forever (docs/21 §8). Reads run the
// project's visibility gate.
func (s *Service) GetVersion(ctx context.Context, r projects.Reader, projectID, name, version string) (domain.ProjectSchemaProfile, error) {
	if s.store == nil {
		return domain.ProjectSchemaProfile{}, fmt.Errorf("%w: no profile store configured", ErrStore)
	}
	if err := s.requireRead(ctx, r, projectID); err != nil {
		return domain.ProjectSchemaProfile{}, err
	}
	return s.store.GetProfile(ctx, projectID, "project:"+projectID+":"+name, version)
}

// List returns every profile version of the project, newest first. Reads
// run the project's visibility gate.
func (s *Service) List(ctx context.Context, r projects.Reader, projectID string) ([]domain.ProjectSchemaProfile, error) {
	if s.store == nil {
		return nil, fmt.Errorf("%w: no profile store configured", ErrStore)
	}
	if err := s.requireRead(ctx, r, projectID); err != nil {
		return nil, err
	}
	profiles, err := s.store.ListProfiles(ctx, projectID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return profiles, nil
}

// GetLatestProfile returns the profile's newest registered version by
// (project, schema id) — the resolution the object-create path runs when a
// client names a profile schema id. It does NOT run the read gate: the
// caller (the RSG service) has already authorized the write. Unknown
// profiles answer ErrProfileNotFound; there is no silent fallback to the
// canonical schema.
func (s *Service) GetLatestProfile(ctx context.Context, projectID, schemaID string) (domain.ProjectSchemaProfile, error) {
	if s.store == nil {
		return domain.ProjectSchemaProfile{}, fmt.Errorf("%w: no profile store configured", ErrStore)
	}
	profile, err := s.store.GetLatestProfile(ctx, projectID, schemaID)
	if err != nil {
		if errors.Is(err, ErrProfileNotFound) {
			return domain.ProjectSchemaProfile{}, err
		}
		return domain.ProjectSchemaProfile{}, wrapStoreError(err)
	}
	return profile, nil
}

// LoadAll re-registers every persisted profile row into the runtime
// registry — the API startup load. Each row's stored content must re-hash
// to its stored content hash (integrity, docs/21 §10) and register without
// conflicting (a database row diverging from the registry's immutable
// content is corruption, not a conflict to resolve). The load is
// idempotent: identical re-registration is a registry no-op.
//
// Two error classes (the startup loader's contract): ErrStore means the
// store is unreachable — retryable, the API keeps serving while the load
// retries; ErrCorruption means a persisted row is broken (hash mismatch,
// registry conflict, content that does not compile as a JSON Schema) —
// NOT retryable, the caller must fail fast rather than serve a registry
// that diverges from the database rows (docs/21 §10).
func (s *Service) LoadAll(ctx context.Context) error {
	if s.reg == nil {
		return fmt.Errorf("%w: no schema registry configured", ErrStore)
	}
	if s.store == nil {
		return fmt.Errorf("%w: no profile store configured", ErrStore)
	}
	rows, err := s.store.ListAllProfiles(ctx)
	if err != nil {
		return wrapStoreError(err)
	}
	for _, p := range rows {
		sum := sha256.Sum256([]byte(p.Content))
		if hex.EncodeToString(sum[:]) != p.ContentHash {
			return fmt.Errorf("%w: schema profile %s v%s: stored content does not re-hash to its stored content hash", ErrCorruption, p.SchemaID, p.Version)
		}
		if _, err := s.reg.Register(p.SchemaID, p.Version, []byte(p.Content)); err != nil {
			if errors.Is(err, schemareg.ErrAlreadyRegistered) {
				return fmt.Errorf("%w: schema profile %s v%s: registered content differs from the persisted row", ErrCorruption, p.SchemaID, p.Version)
			}
			return fmt.Errorf("%w: schema profile %s v%s: %v", ErrCorruption, p.SchemaID, p.Version, err)
		}
	}
	return nil
}

// resolveBase resolves and qualifies the base pin: the base must be a
// registered schema (registry authority), under the canonical namespace
// (profiles extend official schemas only — an extension cannot extend an
// extension, which would re-open the squatting problem), and typed (it
// must declare properties.type.const — the profile inherits the
// authoritative object type). The core-scientific-object schema declares
// no const and is refused: a profile always governs one concrete type.
// The registered *Schema travels back: the profile document is generated
// from its exact registered content (Raw), never from a re-read copy.
func (s *Service) resolveBase(base Ref) (*schemareg.Schema, error) {
	if base.ID == "" || base.Version == "" {
		return nil, fmt.Errorf("%w: base schema id and version are required", ErrValidation)
	}
	ref := schemareg.Ref{ID: base.ID, Version: base.Version}
	if !strings.HasPrefix(strings.ToLower(ref.ID), strings.ToLower(schemareg.CanonicalNamespace)) {
		return nil, fmt.Errorf("%w: base schema %q is not an official schema (extensions extend official base schemas only)", ErrValidation, ref.ID)
	}
	schema, err := s.reg.Lookup(ref)
	if err != nil {
		if errors.Is(err, schemareg.ErrUnknownSchema) || errors.Is(err, schemareg.ErrUnknownVersion) {
			return nil, fmt.Errorf("%w: base schema %s is not registered", ErrValidation, ref)
		}
		return nil, fmt.Errorf("%w: resolving base schema: %v", ErrStore, err)
	}
	if _, ok := schema.TypeConst(); !ok {
		return nil, fmt.Errorf("%w: base schema %s declares no authoritative object type (properties.type.const); a profile must extend a typed object schema", ErrValidation, ref.ID)
	}
	return schema, nil
}

// baseTopLevelKeywords is the exact set of top-level JSON Schema keywords
// the generator carries into the profile document. A base using anything
// else at the top level (applicator vocabulary like $defs, $ref, if/then,
// allOf, unevaluatedProperties — anything whose definitions the generated
// document would silently drop) is refused at registration: a profile
// generated from it would no longer mean what its base meant, so it must
// never be registered. Every schema currently in the canonical catalog
// uses exactly these keywords.
var baseTopLevelKeywords = map[string]bool{
	"$schema":              true,
	"$id":                  true,
	"title":                true,
	"description":          true,
	"type":                 true,
	"required":             true,
	"properties":           true,
	"additionalProperties": true,
}

// generateProfileDoc builds the profile document: the pinned base schema's
// property definitions merged with the project's custom field definitions.
// Extension is enforced by construction:
//
//   - the base's top-level keywords must all be carryable (see
//     baseTopLevelKeywords) — a base whose meaning the generated document
//     could not preserve is refused outright;
//   - custom property names must be NEW fields — a profile adds typed
//     fields to the base, it never redefines base fields (which could
//     weaken or change platform semantics);
//   - custom required names must exist among the merged properties —
//     tightening the base's required array is allowed, inventing a field
//     is not;
//   - the base's own required array and the strict additionalProperties
//     policy are preserved, and the authoritative type (properties.type)
//     travels with the base's definition — a payload can never smuggle a
//     different type past its own profile.
//
// The generated document is a standalone schema (no $ref back to the base):
// the base's definitions are snapshotted into the profile at registration,
// exactly the immutability docs/21 §8 demands — a later base schema change
// never silently rewrites an existing profile.
func generateProfileDoc(base *schemareg.Schema, schemaID, name string, customProperties map[string]any, customRequired []string) (map[string]any, error) {
	var baseDoc map[string]any
	if err := json.Unmarshal(base.Raw(), &baseDoc); err != nil {
		return nil, fmt.Errorf("base schema %s does not decode: %v", base, err)
	}
	for key := range baseDoc {
		if !baseTopLevelKeywords[key] {
			return nil, fmt.Errorf("base schema %s uses top-level keyword %q, which a generated profile cannot carry — the platform refuses to register an extension of this base", base, key)
		}
	}
	baseProps, _ := baseDoc["properties"].(map[string]any)
	if baseProps == nil {
		return nil, fmt.Errorf("base schema %s declares no properties", base)
	}
	merged := make(map[string]any, len(baseProps)+len(customProperties))
	for k, v := range baseProps {
		merged[k] = v
	}
	for name, def := range customProperties {
		if _, exists := baseProps[name]; exists {
			return nil, fmt.Errorf("custom property %q redefines a base field; a profile adds new fields, it never changes base fields", name)
		}
		if _, ok := def.(map[string]any); !ok {
			return nil, fmt.Errorf("custom property %q must be a JSON Schema fragment (a JSON object)", name)
		}
		merged[name] = def
	}
	required := requiredUnion(baseDoc, customRequired)
	for _, name := range customRequired {
		if _, ok := merged[name]; !ok {
			return nil, fmt.Errorf("required field %q is not among the base or custom properties", name)
		}
	}
	title, _ := baseDoc["title"].(string)
	return map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"$id":     schemaID,
		"title":   profileTitle(title, name),
		"description": fmt.Sprintf(
			"Project schema profile registered by the POST platform (T0213): extends the official base schema %s v%s with project-specific typed fields. Generated server-side — content is immutable, a change is a new version.",
			base.Ref.ID, base.Ref.Version),
		"type":                 "object",
		"required":             required,
		"properties":           merged,
		"additionalProperties": false,
	}, nil
}

// requiredUnion merges the base schema's required array with the custom
// required names: base order first, then the request's order, duplicates
// removed. Only names are kept — malformed base entries (a required array
// holding non-strings) are dropped, since the base schema already compiled.
func requiredUnion(baseDoc map[string]any, custom []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, r := range baseRequired(baseDoc) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	for _, r := range custom {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// baseRequired extracts the base schema's required array as strings.
func baseRequired(baseDoc map[string]any) []string {
	raw, _ := baseDoc["required"].([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// profileTitle derives the profile's title from the base title and the
// profile name.
func profileTitle(baseTitle, name string) string {
	if baseTitle == "" {
		baseTitle = "ScientificObject"
	}
	return baseTitle + " (" + name + " project profile)"
}

// requireRead runs the project's visibility-aware read gate (T0106): a
// denied read answers the existence-hiding not-found, exactly like every
// other project read.
func (s *Service) requireRead(ctx context.Context, r projects.Reader, projectID string) error {
	if s.projects == nil {
		return fmt.Errorf("%w: no project gate configured", ErrStore)
	}
	if _, err := s.projects.Get(ctx, r, projectID); err != nil {
		if errors.Is(err, projects.ErrProjectNotFound) {
			return ErrProjectNotFound
		}
		return wrapStoreError(err)
	}
	return nil
}

// requireMaintainer runs the registration gate: the actor must hold a
// maintainer-or-above membership. "Not a member", "role too low" and a
// hidden project all answer the same stable outcomes as the settings
// surface — ErrProjectNotFound for a denied read, ErrForbidden for a
// readable project without a sufficient role (nothing is disclosed either
// way, docs/45).
func (s *Service) requireMaintainer(ctx context.Context, actor domain.User, projectID string) error {
	if s.projects == nil {
		return fmt.Errorf("%w: no project gate configured", ErrStore)
	}
	if _, err := s.projects.Get(ctx, projects.Reader{UserID: actor.ID, Authenticated: true}, projectID); err != nil {
		if errors.Is(err, projects.ErrProjectNotFound) {
			return ErrProjectNotFound
		}
		return wrapStoreError(err)
	}
	membership, err := s.projects.GetMembership(ctx, actor, projectID)
	if err != nil {
		if errors.Is(err, projects.ErrMemberNotFound) {
			return ErrForbidden
		}
		return wrapStoreError(err)
	}
	if !membership.Role.AtLeast(domain.ProjectRoleMaintainer) {
		return ErrForbidden
	}
	return nil
}

// registerAudit builds the audit entry for one profile registration (the
// shared domain.AuditEntry shape; the settings surface's helper, with the
// schema-profile action and target). Via is the audit vocabulary's
// ViaSession: every registration arrives as a session-authenticated
// /api/v1 request. The correlation id is the request's own; a context
// without one gets a fresh id so the NOT NULL column always holds
// something traceable. A failure to mint the id refuses the action (fail
// closed: an audit-less governance write must not happen).
func (s *Service) registerAudit(ctx context.Context, actor domain.User, projectID string, p domain.ProjectSchemaProfile) (domain.AuditEntry, error) {
	correlationID, ok := observability.FromContext(ctx)
	if !ok {
		id, err := observability.NewCorrelationID()
		if err != nil {
			return domain.AuditEntry{}, fmt.Errorf("%w: cannot mint audit correlation id: %v", ErrStore, err)
		}
		correlationID = id
	}
	return domain.AuditEntry{
		ActorID:       actor.ID,
		Via:           domain.ViaSession,
		Action:        domain.ActionSchemaProfileRegistered,
		TargetRef:     "schema:" + p.SchemaID,
		ProjectID:     projectID,
		CorrelationID: correlationID.String(),
		AfterSummary: map[string]any{
			"schema_id":    p.SchemaID,
			"version":      p.Version,
			"base":         map[string]string{"id": p.BaseSchemaID, "version": p.BaseSchemaVersion},
			"content_hash": p.ContentHash,
		},
	}, nil
}

// wrapStoreError keeps the expected domain outcomes and turns everything
// else into ErrStore for the handler, with the cause kept for the log.
// ErrCorruption passes through too: a port that signals corruption must
// never have it downgraded into the retryable ErrStore class — the
// startup loader would retry a broken row forever instead of failing
// fast.
func wrapStoreError(err error) error {
	if err == nil ||
		errors.Is(err, ErrProfileNotFound) ||
		errors.Is(err, ErrProfileVersionExists) ||
		errors.Is(err, ErrForbidden) ||
		errors.Is(err, ErrProjectNotFound) ||
		errors.Is(err, ErrValidation) ||
		errors.Is(err, ErrCorruption) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
