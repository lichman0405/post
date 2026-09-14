package rsg

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/relations"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	"github.com/lichman0405/post/internal/rsg/semantics"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// manifestVersion is the manifest format version every scientific-state
// write commits under (the V1 manifest).
const manifestVersion = "v1"

// Service orchestrates the RSG write commands: research branches, first
// object versions, next object versions and typed relations — every
// scientific-state write as one state commit (gate draft, via api), and
// every write command behind the project membership/role authorization
// the consuming API task owns. Reads run the project's visibility gate
// (T0106) and answer the same existence-hidden outcomes as the project
// surface.
type Service struct {
	projects  ProjectGate
	branches  BranchPort
	states    StatePort
	latest    LatestStatePort
	objects   ObjectPort
	relations RelationPort
	authz     authz.Engine
	schemas   *schemareg.Registry
}

// Deps wires the service. Schemas (the canonical registry) and Authz (the
// matrix engine) are required: an unwired service refuses at call time
// rather than guessing (fail closed, docs/12).
type Deps struct {
	Projects  ProjectGate
	Branches  BranchPort
	States    StatePort
	Latest    LatestStatePort
	Objects   ObjectPort
	Relations RelationPort
	Authz     authz.Engine
	Schemas   *schemareg.Registry
}

// NewService builds the service on the ports.
func NewService(deps Deps) *Service {
	return &Service{
		projects:  deps.Projects,
		branches:  deps.Branches,
		states:    deps.States,
		latest:    deps.Latest,
		objects:   deps.Objects,
		relations: deps.Relations,
		authz:     deps.Authz,
		schemas:   deps.Schemas,
	}
}

// ObjectResult is one object read/write outcome: the container row, its
// (new or current) version, and the advisory semantic hints (never
// failures — see internal/rsg/semantics).
type ObjectResult struct {
	Object  domain.ScientificObject
	Version domain.ScientificObjectVersion
	Hints   []semantics.Hint
}

// ObjectVersionResult is one next-version outcome. The container row
// travels with the new version so the wire answers a complete shape
// without a second read.
type ObjectVersionResult struct {
	Object  domain.ScientificObject
	Version domain.ScientificObjectVersion
	Hints   []semantics.Hint
}

// RelationResult is one relation creation outcome.
type RelationResult struct {
	Relation domain.Relation
	Version  domain.RelationVersion
}

// CreateBranch forks a research branch from a project state. The caller
// must be permitted by ActionCreateBranch: a project member (contributor
// and above) may fork; everyone else is refused before anything is looked
// up. BaseRef "" resolves to the project's latest state (the genesis root
// is created when the project has no state yet — a first branch has
// nothing to fork).
func (s *Service) CreateBranch(ctx context.Context, actor domain.User, projectID string, in CreateBranchInput) (domain.Branch, error) {
	if actor.ID == "" {
		return domain.Branch{}, fmt.Errorf("%w: actor is required", ErrValidation)
	}
	if projectID == "" {
		return domain.Branch{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	role, err := s.projectRole(ctx, actor, projectID)
	if err != nil {
		return domain.Branch{}, err
	}
	if err := s.require(ctx, authz.Request{
		Action: authz.ActionCreateBranch,
		Class:  authz.ClassOf(true, role, false),
	}); err != nil {
		return domain.Branch{}, err
	}
	baseStateID, err := s.resolveBaseState(ctx, projectID, in.BaseRef)
	if err != nil {
		return domain.Branch{}, err
	}
	branch, err := s.branches.Create(ctx, branches.CreateBranchParams{
		ProjectID:   projectID,
		Name:        in.Name,
		Visibility:  in.Visibility,
		Purpose:     in.Purpose,
		BaseStateID: baseStateID,
		CreatedBy:   actor.ID,
	})
	if err != nil {
		return domain.Branch{}, wrapError(err)
	}
	return branch, nil
}

// resolveBaseState turns the caller's base_ref into a state id of the
// project. An explicit ref passes through (the branch adapter's guarded
// insert re-checks the state's project inside the transaction); an empty
// ref — the OpenAPI contract names base_ref required, but a first branch
// has no state to fork — resolves to the project's latest state, creating
// the genesis root when the project has none.
func (s *Service) resolveBaseState(ctx context.Context, projectID, baseRef string) (string, error) {
	if baseRef != "" {
		return baseRef, nil
	}
	if s.latest == nil {
		return "", fmt.Errorf("%w: no latest-state gate configured", ErrStore)
	}
	latest, err := s.latest.GetLatestState(ctx, projectID)
	if err == nil {
		return latest.ID, nil
	}
	if !errors.Is(err, states.ErrStateNotFound) {
		return "", wrapError(err)
	}
	genesis, err := s.states.CreateInitialState(ctx, states.CreateInitialStateParams{
		ProjectID:       projectID,
		ManifestVersion: manifestVersion,
	})
	if err == nil {
		return genesis.ID, nil
	}
	if errors.Is(err, states.ErrStateExists) {
		// A concurrent first branch seeded the genesis between the two
		// reads; fork the state that now exists.
		latest, lerr := s.latest.GetLatestState(ctx, projectID)
		if lerr != nil {
			return "", wrapError(lerr)
		}
		return latest.ID, nil
	}
	return "", wrapError(err)
}

// CreateObject creates an object and its version 1 as one state commit.
// The caller must be permitted by ActionWriteScientificState (a denied
// caller is refused before the object id is even parsed — the refusal is
// identical for an existing and a nonexistent object, docs/45). The
// object id is pre-generated so the commit's operation summary names the
// real entity (the pr gate's commit_linkage check).
func (s *Service) CreateObject(ctx context.Context, actor domain.User, projectID, branchID string, in CreateObjectInput) (ObjectResult, error) {
	if actor.ID == "" {
		return ObjectResult{}, fmt.Errorf("%w: actor is required", ErrValidation)
	}
	if err := s.requireWrite(ctx, actor, projectID); err != nil {
		return ObjectResult{}, err
	}
	if _, err := s.branches.Get(ctx, projectID, branchID); err != nil {
		return ObjectResult{}, wrapError(err)
	}
	ref, ok := s.schemaFor(in.ObjectType)
	if !ok {
		return ObjectResult{}, fmt.Errorf("%w: object_type %q is not a canonical V1 type", ErrValidation, in.ObjectType)
	}
	if in.SchemaRef != "" && in.SchemaRef != ref.ID {
		return ObjectResult{}, fmt.Errorf("%w: V1 pins the canonical schema per object type (schema_ref %q is not %q)", ErrValidation, in.SchemaRef, ref.ID)
	}
	payload, err := payloadObject(in.Payload)
	if err != nil {
		return ObjectResult{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	objectID, err := newID()
	if err != nil {
		return ObjectResult{}, fmt.Errorf("%w: generating object id: %v", ErrStore, err)
	}
	errs, hints := semantics.Check(in.ObjectType, objectID, payload)
	if len(errs) > 0 {
		return ObjectResult{}, fmt.Errorf("%w: %v", ErrValidation, errors.Join(errs...))
	}
	head, err := s.states.GetBranchHead(ctx, branchID)
	if err != nil {
		return ObjectResult{}, wrapError(err)
	}
	title := titleFromPayload(payload, in.ObjectType)
	var obj domain.ScientificObject
	var v domain.ScientificObjectVersion
	write := func(ctx context.Context, tx states.Transaction, stateID string) error {
		var werr error
		obj, v, werr = s.objects.CreateObjectInTx(ctx, tx, CreateObjectInTxParams{
			ObjectID:   objectID,
			ProjectID:  projectID,
			ObjectType: in.ObjectType,
			CreatedBy:  actor.ID,
			Version: sciobjects.VersionParams{
				StateID:        stateID,
				BranchID:       &branchID,
				SchemaID:       ref.ID,
				SchemaVersion:  ref.Version,
				Title:          title,
				LifecycleState: domain.LifecycleActive,
				Payload:        in.Payload,
				CreatedBy:      actor.ID,
			},
		})
		return werr
	}
	_, _, err = s.states.Commit(ctx, states.CommitParams{
		ProjectID:       projectID,
		BranchID:        branchID,
		ActorID:         actor.ID,
		Via:             domain.ViaAPI,
		Message:         fmt.Sprintf("create %s %s", in.ObjectType, title),
		Operations:      []domain.StateOperation{objectOperation(objectID, in.ObjectType, 1)},
		BaseStateID:     &head.ID,
		ManifestVersion: manifestVersion,
		Gate:            rsgvalidation.GateDraft,
	}, write)
	if err != nil {
		return ObjectResult{}, wrapError(err)
	}
	return ObjectResult{Object: obj, Version: v, Hints: hints}, nil
}

// CreateObjectVersion appends version expected+1 to the object's log as
// one state commit. The patch is a shallow merge over the current payload
// (L1: top-level keys only — an object value is replaced whole, never
// spliced). The authorization and the version compare-and-swap are the
// same as CreateObject's: a denied caller is refused before the object is
// looked up, and a lost expectation answers the one stable conflict.
func (s *Service) CreateObjectVersion(ctx context.Context, actor domain.User, projectID, branchID, objectID string, in CreateObjectVersionInput) (ObjectVersionResult, error) {
	if actor.ID == "" {
		return ObjectVersionResult{}, fmt.Errorf("%w: actor is required", ErrValidation)
	}
	if err := s.requireWrite(ctx, actor, projectID); err != nil {
		return ObjectVersionResult{}, err
	}
	if _, err := s.branches.Get(ctx, projectID, branchID); err != nil {
		return ObjectVersionResult{}, wrapError(err)
	}
	if in.ExpectedVersion < 1 {
		return ObjectVersionResult{}, fmt.Errorf("%w: expected_version must be >= 1", ErrValidation)
	}
	patch, err := payloadObject(in.Patch)
	if err != nil {
		return ObjectVersionResult{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	obj, err := s.objects.GetObject(ctx, objectID)
	if err != nil {
		return ObjectVersionResult{}, wrapError(err)
	}
	if obj.ProjectID != projectID {
		// An object of another project reports the same outcome as an
		// unknown one (never leak a foreign project's state).
		return ObjectVersionResult{}, sciobjects.ErrObjectNotFound
	}
	latest, err := s.objects.GetLatestVersion(ctx, objectID)
	if err != nil {
		return ObjectVersionResult{}, wrapError(err)
	}
	merged := shallowMerge(latest.Payload, patch)
	errs, hints := semantics.Check(obj.ObjectType, objectID, merged)
	if len(errs) > 0 {
		return ObjectVersionResult{}, fmt.Errorf("%w: %v", ErrValidation, errors.Join(errs...))
	}
	head, err := s.states.GetBranchHead(ctx, branchID)
	if err != nil {
		return ObjectVersionResult{}, wrapError(err)
	}
	title := titleFromPayload(merged, obj.ObjectType)
	if title == "" {
		title = latest.Title // keep the identity a v1 established
	}
	mergedRaw, err := json.Marshal(merged)
	if err != nil {
		return ObjectVersionResult{}, fmt.Errorf("%w: re-encoding merged payload: %v", ErrStore, err)
	}
	versionNo := in.ExpectedVersion + 1
	var v domain.ScientificObjectVersion
	write := func(ctx context.Context, tx states.Transaction, stateID string) error {
		var werr error
		v, werr = s.objects.CreateVersionInTx(ctx, tx, objectID, in.ExpectedVersion, sciobjects.VersionParams{
			StateID:        stateID,
			BranchID:       &branchID,
			SchemaID:       latest.SchemaID,
			SchemaVersion:  latest.SchemaVersion,
			Title:          title,
			LifecycleState: domain.LifecycleActive,
			Payload:        mergedRaw,
			CreatedBy:      actor.ID,
		})
		return werr
	}
	_, _, err = s.states.Commit(ctx, states.CommitParams{
		ProjectID:       projectID,
		BranchID:        branchID,
		ActorID:         actor.ID,
		Via:             domain.ViaAPI,
		Message:         fmt.Sprintf("update %s %s to version %d", obj.ObjectType, title, versionNo),
		Operations:      []domain.StateOperation{objectOperation(objectID, obj.ObjectType, versionNo)},
		BaseStateID:     &head.ID,
		ManifestVersion: manifestVersion,
		Gate:            rsgvalidation.GateDraft,
	}, write)
	if err != nil {
		return ObjectVersionResult{}, wrapError(err)
	}
	return ObjectVersionResult{Object: obj, Version: v, Hints: hints}, nil
}

// CreateRelation creates a typed relation and its version 1 as one state
// commit. The relation type must be in the V1 catalog (docs/44) and both
// endpoints must name existing object versions of the SAME project — a
// missing endpoint and a foreign-project endpoint report the same
// ReferencedVersionNotFoundError outcome (no leak). Authorization runs
// first, before any endpoint is looked up.
func (s *Service) CreateRelation(ctx context.Context, actor domain.User, projectID, branchID string, in CreateRelationInput) (RelationResult, error) {
	if actor.ID == "" {
		return RelationResult{}, fmt.Errorf("%w: actor is required", ErrValidation)
	}
	if err := s.requireWrite(ctx, actor, projectID); err != nil {
		return RelationResult{}, err
	}
	if _, err := s.branches.Get(ctx, projectID, branchID); err != nil {
		return RelationResult{}, wrapError(err)
	}
	if _, ok := relationcatalog.Lookup(in.RelationType); !ok {
		return RelationResult{}, &relations.UnknownRelationTypeError{Type: in.RelationType}
	}
	if in.SourceObjectVersionID == "" || in.TargetObjectVersionID == "" {
		return RelationResult{}, fmt.Errorf("%w: source_object_version_id and target_object_version_id are required (edges are version-pinned)", ErrValidation)
	}
	if err := s.requireEndpoint(ctx, projectID, "source", in.SourceObjectVersionID); err != nil {
		return RelationResult{}, err
	}
	if err := s.requireEndpoint(ctx, projectID, "target", in.TargetObjectVersionID); err != nil {
		return RelationResult{}, err
	}
	payload := in.Payload
	if len(payload) == 0 || string(payload) == "null" {
		payload = json.RawMessage("{}")
	}
	if _, err := payloadObject(payload); err != nil {
		return RelationResult{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}
	relationID, err := newID()
	if err != nil {
		return RelationResult{}, fmt.Errorf("%w: generating relation id: %v", ErrStore, err)
	}
	head, err := s.states.GetBranchHead(ctx, branchID)
	if err != nil {
		return RelationResult{}, wrapError(err)
	}
	var rel domain.Relation
	var v domain.RelationVersion
	write := func(ctx context.Context, tx states.Transaction, stateID string) error {
		var werr error
		rel, v, werr = s.relations.CreateRelationInTx(ctx, tx, CreateRelationInTxParams{
			RelationID: relationID,
			ProjectID:  projectID,
			Version: relations.VersionParams{
				StateID:               stateID,
				RelationType:          in.RelationType,
				SourceObjectVersionID: in.SourceObjectVersionID,
				TargetObjectVersionID: in.TargetObjectVersionID,
				Payload:               payload,
				CreatedBy:             actor.ID,
			},
		})
		return werr
	}
	_, _, err = s.states.Commit(ctx, states.CommitParams{
		ProjectID:       projectID,
		BranchID:        branchID,
		ActorID:         actor.ID,
		Via:             domain.ViaAPI,
		Message:         fmt.Sprintf("create %s relation", in.RelationType),
		Operations:      []domain.StateOperation{relationOperation(relationID, in.RelationType)},
		BaseStateID:     &head.ID,
		ManifestVersion: manifestVersion,
		Gate:            rsgvalidation.GateDraft,
	}, write)
	if err != nil {
		return RelationResult{}, wrapError(err)
	}
	return RelationResult{Relation: rel, Version: v}, nil
}

// requireEndpoint verifies one relation endpoint: the version must exist
// and belong to an object of the project. A missing version and a
// foreign-project version answer the same outcome, with the side named.
func (s *Service) requireEndpoint(ctx context.Context, projectID, side, versionID string) error {
	v, err := s.objects.GetVersionByID(ctx, versionID)
	if err != nil {
		if errors.Is(err, sciobjects.ErrVersionNotFound) {
			return &relations.ReferencedVersionNotFoundError{Side: side, VersionID: versionID}
		}
		return wrapError(err)
	}
	obj, err := s.objects.GetObject(ctx, v.ObjectID)
	if err != nil {
		return wrapError(err)
	}
	if obj.ProjectID != projectID {
		return &relations.ReferencedVersionNotFoundError{Side: side, VersionID: versionID}
	}
	return nil
}

// GetObject reads one object with its current version. The project's
// visibility gate runs first (T0106): a denied read answers the same
// existence-hidden outcome as an unknown project, and an object of
// another project answers the same outcome as an unknown object.
func (s *Service) GetObject(ctx context.Context, r projects.Reader, projectID, branchID, objectID string) (ObjectResult, error) {
	if _, err := s.projects.Get(ctx, r, projectID); err != nil {
		return ObjectResult{}, wrapError(err)
	}
	if _, err := s.branches.Get(ctx, projectID, branchID); err != nil {
		return ObjectResult{}, wrapError(err)
	}
	obj, err := s.objects.GetObject(ctx, objectID)
	if err != nil {
		return ObjectResult{}, wrapError(err)
	}
	if obj.ProjectID != projectID {
		return ObjectResult{}, sciobjects.ErrObjectNotFound
	}
	latest, err := s.objects.GetLatestVersion(ctx, objectID)
	if err != nil {
		return ObjectResult{}, wrapError(err)
	}
	return ObjectResult{Object: obj, Version: latest}, nil
}

// projectRole resolves the actor's membership role in the project — the
// authz class input. The resolution itself runs the project read gate
// (projects.Service.GetMembership): a caller who may not read the project
// gets ErrProjectNotFound (the existence-hiding 404) before any role is
// decided; a readable project without a membership answers "no role",
// which ClassOf resolves to the authenticated_nonmember matrix column.
func (s *Service) projectRole(ctx context.Context, actor domain.User, projectID string) (*domain.ProjectRole, error) {
	if s.projects == nil {
		return nil, fmt.Errorf("%w: no project gate configured", ErrStore)
	}
	membership, err := s.projects.GetMembership(ctx, actor, projectID)
	switch {
	case err == nil:
		role := membership.Role
		return &role, nil
	case errors.Is(err, projects.ErrMemberNotFound):
		return nil, nil
	default:
		return nil, wrapError(err)
	}
}

// requireWrite authorizes one scientific-state write: resolve the caller's
// membership/role, then evaluate ActionWriteScientificState for the class
// (the task's rule verbatim — authz.ClassOf(authenticated, role, false),
// same require shape as internal/application/projects). The denial happens
// before any object or relation lookup, so it never discloses whether the
// target exists.
func (s *Service) requireWrite(ctx context.Context, actor domain.User, projectID string) error {
	role, err := s.projectRole(ctx, actor, projectID)
	if err != nil {
		return err
	}
	return s.require(ctx, authz.Request{
		Action: authz.ActionWriteScientificState,
		Class:  authz.ClassOf(true, role, false),
	})
}

// require evaluates one authorization request and fails closed. A
// permitting verdict passes; every other verdict — an outright deny, or a
// conditional form this site does not resolve — answers ErrForbidden; an
// engine failure (or a service wired without an engine) answers ErrStore:
// state is unknowable, so the action is refused rather than guessed
// (default deny, docs/12). The same shape as projects.Service.require.
func (s *Service) require(ctx context.Context, req authz.Request) error {
	if s.authz == nil {
		return fmt.Errorf("%w: no policy engine configured", ErrStore)
	}
	decision, err := s.authz.Authorize(ctx, req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
	if !decision.Permits() {
		return ErrForbidden
	}
	return nil
}

// schemaFor resolves the canonical V1 schema reference of an object type.
// The type token itself is shape-checked first (a path or URI never
// reaches the registry); the registry is then the one authority on which
// types exist.
func (s *Service) schemaFor(objectType string) (schemareg.Ref, bool) {
	if !typeTokenRe.MatchString(objectType) || s.schemas == nil {
		return schemareg.Ref{}, false
	}
	id := schemareg.CanonicalNamespace + objectType + ".schema.json"
	ref := schemareg.Ref{ID: id, Version: schemareg.CanonicalV1}
	if _, err := s.schemas.Lookup(ref); err != nil {
		return schemareg.Ref{}, false
	}
	return ref, true
}

// typeTokenRe bounds the object-type token (canonical type names are
// lowercase snake_case; anything else can never name a canonical schema).
var typeTokenRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// objectOperation builds the commit operation summary for one object
// version: the entity is the pre-generated object id and the version
// number the log position, exactly what the pr gate's commit_linkage
// check matches against the rows as written.
func objectOperation(objectID, objectType string, versionNo int) domain.StateOperation {
	detail, _ := json.Marshal(map[string]string{"object_type": objectType})
	return domain.StateOperation{
		Kind:      domain.OperationObjectVersionCreated,
		EntityID:  objectID,
		VersionNo: versionNo,
		Detail:    detail,
	}
}

// relationOperation builds the commit operation summary for one relation
// version (same commit_linkage contract as objectOperation).
func relationOperation(relationID, relationType string) domain.StateOperation {
	detail, _ := json.Marshal(map[string]string{"relation_type": relationType})
	return domain.StateOperation{
		Kind:      domain.OperationRelationVersionCreated,
		EntityID:  relationID,
		VersionNo: 1,
		Detail:    detail,
	}
}

// payloadObject verifies a payload is a JSON object and returns its
// decoded form (the semantic checks and the title derivation run on it).
func payloadObject(raw json.RawMessage) (map[string]any, error) {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("payload must be a JSON object: %v", err)
	}
	return payload, nil
}

// shallowMerge merges a version patch over the current payload at the top
// level only (L1, recorded for the Supervisor): the new version replaces
// the keys the patch names and keeps the rest. No deep merge — an array or
// object value is replaced whole, never spliced.
func shallowMerge(current json.RawMessage, patch map[string]any) map[string]any {
	merged := map[string]any{}
	var cur map[string]any
	if json.Unmarshal(current, &cur) == nil {
		merged = cur
	}
	for k, v := range patch {
		merged[k] = v
	}
	return merged
}

// titleFields are the per-type payload fields the derived title prefers,
// in order — the first non-empty string wins.
var titleFields = []string{"name", "sample_code", "statement", "objective", "purpose", "canonical_url", "external_identifier"}

// maxTitle bounds the derived title (the identity_fields check requires a
// title; the payload is caller-sized, the stored title is not).
const maxTitle = 200

// titleFromPayload derives the version title from the payload's natural
// title field, falling back to the humanized object type. Identity facts
// are server-derived (docs/23 §3): the client never supplies the title.
func titleFromPayload(payload map[string]any, objectType string) string {
	for _, field := range titleFields {
		if s, ok := payload[field].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				if len(s) > maxTitle {
					s = s[:maxTitle]
				}
				return s
			}
		}
	}
	return strings.ReplaceAll(objectType, "_", " ")
}

// newID generates a UUID v4 with crypto/rand (the same shape as
// domain.NewUserID, which is user-specific by name). Object and relation
// ids are pre-generated server-side so the state commit's operation
// summary names the real entities (commit_linkage).
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// wrapError keeps the expected domain outcomes — the owning packages'
// sentinels and typed errors (a lost version compare-and-swap, a branch
// state conflict, a blocked validation gate, ...) — and turns everything
// else into ErrStore for the handler, with the cause kept for the log.
func wrapError(err error) error {
	if err == nil ||
		errors.Is(err, ErrForbidden) ||
		errors.Is(err, ErrValidation) ||
		errors.Is(err, ErrStore) ||
		errors.Is(err, projects.ErrProjectNotFound) ||
		errors.Is(err, projects.ErrMemberNotFound) ||
		errors.Is(err, branches.ErrBranchNotFound) ||
		errors.Is(err, branches.ErrBranchNameTaken) ||
		errors.Is(err, branches.ErrBaseStateNotFound) ||
		errors.Is(err, branches.ErrBranchNotActive) ||
		errors.Is(err, branches.ErrMainProtected) ||
		errors.Is(err, branches.ErrPublicBranchInPrivateProject) ||
		errors.Is(err, branches.ErrStateNotFound) ||
		errors.Is(err, branches.ErrValidation) ||
		errors.Is(err, states.ErrBranchNotFound) ||
		errors.Is(err, states.ErrStateNotFound) ||
		errors.Is(err, states.ErrStateExists) ||
		errors.Is(err, states.ErrValidation) ||
		errors.Is(err, sciobjects.ErrObjectNotFound) ||
		errors.Is(err, sciobjects.ErrVersionNotFound) ||
		errors.Is(err, sciobjects.ErrValidation) ||
		errors.Is(err, relations.ErrRelationNotFound) ||
		errors.Is(err, relations.ErrRelationVersionNotFound) ||
		errors.Is(err, relations.ErrReferencedVersionNotFound) ||
		errors.Is(err, relations.ErrValidation) ||
		errors.As(err, new(*sciobjects.VersionConflictError)) ||
		errors.As(err, new(*relations.VersionConflictError)) ||
		errors.As(err, new(*relations.UnknownRelationTypeError)) ||
		errors.As(err, new(*relations.ReferencedVersionNotFoundError)) ||
		errors.As(err, new(*states.StateConflictError)) ||
		errors.As(err, new(*states.BranchNotActiveError)) ||
		errors.As(err, new(*branches.NotActiveError)) ||
		errors.As(err, new(*rsgvalidation.GateBlockedError)) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
