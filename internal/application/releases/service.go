package releases

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/application/manifests"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Service orchestrates the release manifest build against the read
// ports. It owns input validation (the ports trust, the service
// verifies); it does NOT authorize — the release command (T0606)
// resolves visibility before calling it (package doc).
type Service struct {
	states    StatePort
	manifests StateManifestPort
	projects  ProjectPort
	branches  MainBranchPort
	policies  PolicyVersionPort
	reviews   ReleaseReviewPort
	schemas   SchemaRegistryPort
	// now is the render timestamp source; the zero value means
	// time.Now. Tests inject a fixed clock.
	now func() time.Time
}

// NewService wires the release manifest builder.
func NewService(
	states StatePort,
	manifests StateManifestPort,
	projects ProjectPort,
	branches MainBranchPort,
	policies PolicyVersionPort,
	reviews ReleaseReviewPort,
	schemas SchemaRegistryPort,
) *Service {
	return &Service{
		states:    states,
		manifests: manifests,
		projects:  projects,
		branches:  branches,
		policies:  policies,
		reviews:   reviews,
		schemas:   schemas,
	}
}

// BuildParams names one release manifest: the accepted main state being
// snapshotted, the release version string, and the policy versions to pin
// explicitly. The caller resolved "the policy in force at release time"
// (docs/12 §5) before calling — the builder never resolves "latest"
// itself, so a release rebuilt at any later time renders the identical
// document (determinism contract).
type BuildParams struct {
	ProjectID string
	StateID   string
	// Version is the release version string ([A-Za-z0-9._-], 1..64 —
	// the same bounded label the policy versions use).
	Version string
	// OrgPolicyVersionID pins the organization policy version in force
	// (the lower bound); nil when the project has no organization or no
	// org policy. When set, the version must belong to the project's
	// organization.
	OrgPolicyVersionID *string
	// ProjectPolicyVersionID pins the project policy version in force
	// (the stricter overlay); nil when the project has none.
	ProjectPolicyVersionID *string
}

// Build assembles the canonical release manifest: the state's canonical
// export embedded in full, the pinned policy versions (each with its
// full document), the pinned schema versions (ref + registry content
// hash), the review/approval record and the manifest hash. The same
// inputs render the same bytes every time — only generated_at moves, and
// it is not part of the hash.
//
// The state must be an accepted main snapshot: a state outside main is
// refused outright (ErrNotMainState) — a release of anything else is not
// a document at all (docs/11 §1).
func (s *Service) Build(ctx context.Context, in BuildParams) (*ReleaseManifest, error) {
	version, err := validateBuildParams(in)
	if err != nil {
		return nil, err
	}
	project, state, mainBranch, err := s.acceptedMainSnapshot(ctx, in.ProjectID, in.StateID)
	if err != nil {
		return nil, err
	}
	pin, err := s.resolvePolicyPin(ctx, project, in.OrgPolicyVersionID, in.ProjectPolicyVersionID)
	if err != nil {
		return nil, err
	}
	export, err := s.manifests.Export(ctx, state.ID)
	if err != nil {
		return nil, mapManifestError(err)
	}
	schemaPins, err := s.pinSchemas(export)
	if err != nil {
		return nil, err
	}
	records, err := s.reviews.ListReleaseReviews(ctx, state.ID, mainBranch.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: read review record of state %s: %v", ErrStore, state.ID, err)
	}
	m, err := Build(ManifestInput{
		ProjectID:   project.ID,
		StateID:     state.ID,
		Version:     version,
		GeneratedAt: s.renderTime(),
		State:       export,
		Policy:      pin,
		Schemas:     schemaPins,
		Reviews:     records,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	return m, nil
}

// validateBuildParams checks the input shape and returns the version
// string as stored: trimmed (the same rule the policy versions use — the
// stored string is the one validation saw).
func validateBuildParams(in BuildParams) (string, error) {
	if in.ProjectID == "" || in.StateID == "" {
		return "", fmt.Errorf("%w: project_id and state_id are required", ErrValidation)
	}
	version := strings.TrimSpace(in.Version)
	if !domain.ValidPolicyVersion(version) {
		return "", fmt.Errorf("%w: release version %q must be 1..64 characters of [A-Za-z0-9._-]", ErrValidation, in.Version)
	}
	return version, nil
}

// acceptedMainSnapshot resolves the release's premise — the project, the
// state and the main branch the state must be on. The project boundary is
// enforced before anything else: a state of another project reports "not
// found", never "forbidden" (docs/45 — no foreign entity existence
// leaks).
func (s *Service) acceptedMainSnapshot(ctx context.Context, projectID, stateID string) (domain.Project, domain.ProjectState, domain.Branch, error) {
	project, err := s.projects.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, projects.ErrProjectNotFound) {
			return domain.Project{}, domain.ProjectState{}, domain.Branch{}, ErrProjectNotFound
		}
		return domain.Project{}, domain.ProjectState{}, domain.Branch{}, fmt.Errorf("%w: read project: %v", ErrStore, err)
	}
	state, err := s.states.GetState(ctx, stateID)
	if err != nil {
		if errors.Is(err, states.ErrStateNotFound) {
			return domain.Project{}, domain.ProjectState{}, domain.Branch{}, ErrStateNotFound
		}
		return domain.Project{}, domain.ProjectState{}, domain.Branch{}, fmt.Errorf("%w: read state: %v", ErrStore, err)
	}
	if state.ProjectID != projectID {
		return domain.Project{}, domain.ProjectState{}, domain.Branch{}, ErrStateNotFound
	}
	branches, err := s.branches.ListBranches(ctx, projectID)
	if err != nil {
		return domain.Project{}, domain.ProjectState{}, domain.Branch{}, fmt.Errorf("%w: read branches: %v", ErrStore, err)
	}
	var mainBranch domain.Branch
	for _, b := range branches {
		if b.IsMain() {
			mainBranch = b
			break
		}
	}
	if mainBranch.ID == "" || state.BranchID == nil || *state.BranchID != mainBranch.ID {
		return domain.Project{}, domain.ProjectState{}, domain.Branch{}, ErrNotMainState
	}
	return project, state, mainBranch, nil
}

// resolvePolicyPin reads the two named policy versions and renders their
// canonical documents. A named version that does not exist — or does not
// belong to this release's scope (the project's organization / the
// project itself) — is refused as not found: never reveal a foreign
// entity's policy existence (docs/45).
func (s *Service) resolvePolicyPin(ctx context.Context, project domain.Project, orgVersionID, projectVersionID *string) (*PolicyPin, error) {
	var pin PolicyPin
	if orgVersionID != nil {
		v, err := s.policies.GetVersion(ctx, *orgVersionID)
		if err != nil {
			if errors.Is(err, policy.ErrPolicyNotFound) {
				return nil, ErrPolicyNotFound
			}
			return nil, fmt.Errorf("%w: read org policy version: %v", ErrStore, err)
		}
		if project.OrganizationID == nil || v.Scope.OrganizationID != *project.OrganizationID {
			return nil, ErrPolicyNotFound
		}
		doc, err := v.Policy.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("%w: render org policy %s: %v", ErrStore, v.ID, err)
		}
		pin.Organization = &PinnedPolicyVersion{
			ID:        v.ID,
			Version:   v.Version,
			Document:  doc,
			CreatedBy: v.CreatedBy,
			CreatedAt: v.CreatedAt,
		}
	}
	if projectVersionID != nil {
		v, err := s.policies.GetVersion(ctx, *projectVersionID)
		if err != nil {
			if errors.Is(err, policy.ErrPolicyNotFound) {
				return nil, ErrPolicyNotFound
			}
			return nil, fmt.Errorf("%w: read project policy version: %v", ErrStore, err)
		}
		if v.Scope.ProjectID != project.ID {
			return nil, ErrPolicyNotFound
		}
		doc, err := v.Policy.MarshalJSON()
		if err != nil {
			return nil, fmt.Errorf("%w: render project policy %s: %v", ErrStore, v.ID, err)
		}
		pin.Project = &PinnedPolicyVersion{
			ID:        v.ID,
			Version:   v.Version,
			Document:  doc,
			CreatedBy: v.CreatedBy,
			CreatedAt: v.CreatedAt,
		}
	}
	if pin.Organization == nil && pin.Project == nil {
		return nil, nil
	}
	return &pin, nil
}

// pinSchemas derives the unique schema refs of the export's object
// versions and pins each by its registry content hash. A ref the registry
// does not know cannot be pinned — the registry is the one source of
// schema content, and a pin without content is no pin.
func (s *Service) pinSchemas(export *manifest.Manifest) ([]SchemaPin, error) {
	seen := make(map[schemareg.Ref]bool, len(export.ObjectVersions))
	for _, ov := range export.ObjectVersions {
		seen[schemareg.Ref{ID: ov.SchemaRef.ID, Version: ov.SchemaRef.Version}] = true
	}
	refs := make([]schemareg.Ref, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ID != refs[j].ID {
			return refs[i].ID < refs[j].ID
		}
		return refs[i].Version < refs[j].Version
	})
	pins := make([]SchemaPin, 0, len(refs))
	for _, ref := range refs {
		sch, ok := s.schemas.Get(ref)
		if !ok {
			return nil, fmt.Errorf("%w: schema %s is not registered — the release cannot pin it", ErrStore, ref)
		}
		pins = append(pins, SchemaPin{ID: ref.ID, Version: ref.Version, ContentHash: sch.ContentHash})
	}
	return pins, nil
}

// FactsParams names the release-gate facts to assemble: the project and
// state being released, and the policy versions the caller intends to pin
// (nil = none — the rights fact is then false and the release gate
// refuses, as it should).
type FactsParams struct {
	ProjectID              string
	StateID                string
	OrgPolicyVersionID     *string
	ProjectPolicyVersionID *string
}

// ReleaseFacts assembles the three release-gate facts
// (rsgvalidation.ReleaseFacts) the validation ladder demands before an
// immutable release snapshot exists: the review/approval record
// (scientific and integrity reviews recorded and approved), the
// rights/policy snapshot (the named policy versions exist in scope), and
// the accepted main-branch origin. The facts are reported, never
// enforced here — the release gate refuses (docs/11 §1, internal/rsg/
// validation's GateRelease).
func (s *Service) ReleaseFacts(ctx context.Context, in FactsParams) (rsgvalidation.ReleaseFacts, error) {
	if in.ProjectID == "" || in.StateID == "" {
		return rsgvalidation.ReleaseFacts{}, fmt.Errorf("%w: project_id and state_id are required", ErrValidation)
	}
	project, state, mainBranch, err := s.acceptedMainSnapshot(ctx, in.ProjectID, in.StateID)
	if err != nil {
		if errors.Is(err, ErrNotMainState) {
			return rsgvalidation.ReleaseFacts{}, nil // the fact is false, not an error
		}
		return rsgvalidation.ReleaseFacts{}, err
	}
	pin, err := s.resolvePolicyPin(ctx, project, in.OrgPolicyVersionID, in.ProjectPolicyVersionID)
	if err != nil {
		return rsgvalidation.ReleaseFacts{}, err
	}
	records, err := s.reviews.ListReleaseReviews(ctx, state.ID, mainBranch.ID)
	if err != nil {
		return rsgvalidation.ReleaseFacts{}, fmt.Errorf("%w: read review record of state %s: %v", ErrStore, state.ID, err)
	}
	facts := rsgvalidation.ReleaseFacts{
		ReviewApproved: approvedKinds(records, "scientific", "integrity"),
		RightsSnapshot: pin != nil,
		FromMainBranch: true,
	}
	return facts, nil
}

// approvedKinds reports whether the record carries an approved review for
// every kind — the release gate's review/approval requirement
// (docs/09 §5: the two dimensions are recorded separately, never one
// aggregate approval).
func approvedKinds(records []ReviewRecord, kinds ...string) bool {
	have := make(map[string]bool, len(kinds))
	for _, rec := range records {
		for _, r := range rec.Reviews {
			if r.Decision == "approved" {
				have[r.ReviewKind] = true
			}
		}
	}
	for _, k := range kinds {
		if !have[k] {
			return false
		}
	}
	return true
}

// mapManifestError translates the manifests service's sentinels into this
// package's vocabulary (docs/45): the export read is one store read of
// the release flow.
func mapManifestError(err error) error {
	switch {
	case errors.Is(err, manifests.ErrStateNotFound):
		return ErrStateNotFound
	case errors.Is(err, manifests.ErrValidation):
		return ErrValidation
	default:
		return fmt.Errorf("%w: export state manifest: %v", ErrStore, err)
	}
}

// renderTime is the generated_at source: the injected clock, or the wall
// clock.
func (s *Service) renderTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}
