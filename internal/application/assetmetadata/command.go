package assetmetadata

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// Command is the asset metadata revision use case (T0706).
//
// It owns the order of the steps, and the order is the contract:
//
//  1. shape  — validate the request, before anything is read
//  2. role   — the default-deny maintainer gate, before any asset lookup
//  3. revise — the store's transaction: lock, read the before values,
//     write, audit
//
// Step 1 precedes every read because it needs none. Step 2 precedes the
// asset lookup because a denial must not disclose whether the pid exists:
// an unknown pid and an asset the caller may not govern are answered the
// same (the rule internal/application/assetrights.requireOwner states).
type Command struct {
	members MembershipPort
	store   StorePort
}

// Deps carries the adapters a Command is wired over.
type Deps struct {
	// Members resolves the actor's project membership (the production
	// value is *persistence.ProjectStore).
	Members MembershipPort
	// Store is the metadata store (the production value is
	// *persistence.AssetMetadataStore).
	Store StorePort
}

// NewCommand wires the revision command.
func NewCommand(deps Deps) *Command {
	return &Command{members: deps.Members, store: deps.Store}
}

// Revise revises one asset's metadata and returns the stored result.
//
// docs/11 §4 in one method: the fields change, an audit row records the
// change, and no scientific version is produced or touched. The command
// has no version parameter to pass and the store has no version table to
// write, so the guarantee is structural rather than a promise —
// tests/integration pins it over real rows.
func (c *Command) Revise(ctx context.Context, actor domain.User, in ReviseParams) (Revision, error) {
	req, err := c.prepare(ctx, actor, in)
	if err != nil {
		return Revision{}, err
	}
	if err := c.requireManager(ctx, in.ProjectID, actor.ID); err != nil {
		return Revision{}, err
	}
	revision, err := c.store.ReviseMetadata(ctx, req)
	if err != nil {
		return Revision{}, mapReviseError(err)
	}
	return revision, nil
}

// ReviseParams is one metadata revision request.
type ReviseParams struct {
	// ProjectID is the project the asset belongs to. It is required
	// rather than derived, and that is the authorization's shape: the
	// actor's membership is resolved for THIS project before the asset is
	// looked up at all, so the denial precedes the lookup (the
	// assetrights.ChangeParams rule). The store refuses a revision whose
	// asset's origin project is not this one, so the pair cannot be used
	// to authorize in one project and write in another.
	ProjectID string
	// AssetPID is the asset's persistent identifier. Metadata is revised
	// BY pid: the pid is what every later reader and every URL resolves
	// through, and it is the one thing about the asset a revision cannot
	// move.
	AssetPID string
	// Changes are the fields to write; nil means unchanged.
	Changes Changes
}

// prepare validates the request shape and renders the store request.
//
// Every refusal here is a statement about the REQUEST, made before
// anything is read: a project must be named, the pid must be a pid, at
// least one field must be present, and each present field must be
// storable. Whether the asset EXISTS is the store's read, because a
// command that cannot look anything up cannot decide it.
//
// The values are trimmed where trimming cannot lose content (a title, a
// slug, a keyword, a contact, a documentation reference — a leading or
// trailing space is not part of any of them) and left alone where it can
// (the description: whitespace inside prose is content, and the write
// stores what the caller declared).
func (c *Command) prepare(ctx context.Context, actor domain.User, in ReviseParams) (RevisionRequest, error) {
	projectID := strings.TrimSpace(in.ProjectID)
	if projectID == "" {
		return RevisionRequest{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	pid := strings.TrimSpace(in.AssetPID)
	if !assets.ValidPID(pid) {
		return RevisionRequest{}, fmt.Errorf("%w: asset_pid %q is not a persistent identifier (26 Crockford base32 characters)", ErrValidation, in.AssetPID)
	}
	if in.Changes.Cover != nil {
		// Refused BY NAME, before anything is read and before the
		// unchanged-fields check below — a caller that sent only a cover
		// must be told why it was refused, not told that it sent nothing.
		// See ErrCoverNotSupported for what the build is missing.
		return RevisionRequest{}, &CoverNotSupportedError{}
	}
	if in.Changes.Empty() {
		return RevisionRequest{}, fmt.Errorf("%w: nothing to revise — at least one metadata field must be present", ErrValidation)
	}

	changes := in.Changes
	if changes.Title != nil {
		title := strings.TrimSpace(*changes.Title)
		if !assets.ValidMetadataTitle(title) {
			return RevisionRequest{}, fmt.Errorf("%w: title must be 1..%d characters", ErrValidation, assets.TitleMaxLen)
		}
		changes.Title = &title
	}
	if changes.Slug != nil {
		slug := strings.TrimSpace(*changes.Slug)
		if !assets.ValidMetadataSlug(slug) {
			return RevisionRequest{}, fmt.Errorf("%w: slug must be 1..%d characters", ErrValidation, assets.SlugMaxLen)
		}
		changes.Slug = &slug
	}
	if changes.Description != nil && !assets.ValidMetadataDescription(*changes.Description) {
		return RevisionRequest{}, fmt.Errorf("%w: description must be at most %d characters", ErrValidation, assets.MaxDescriptionLen)
	}
	if changes.Keywords != nil {
		items := trimAll(*changes.Keywords)
		if !assets.ValidMetadataList(items, assets.MaxKeywords, assets.MaxKeywordLen) {
			return RevisionRequest{}, fmt.Errorf("%w: keywords must be at most %d non-blank entries of at most %d characters", ErrValidation, assets.MaxKeywords, assets.MaxKeywordLen)
		}
		changes.Keywords = &items
	}
	if changes.Contact != nil {
		items := trimAll(*changes.Contact)
		if !assets.ValidMetadataList(items, assets.MaxContacts, assets.MaxContactLen) {
			return RevisionRequest{}, fmt.Errorf("%w: contact must be at most %d non-blank entries of at most %d characters", ErrValidation, assets.MaxContacts, assets.MaxContactLen)
		}
		changes.Contact = &items
	}
	if changes.Documentation != nil {
		items := trimAll(*changes.Documentation)
		if !assets.ValidMetadataList(items, assets.MaxDocumentation, assets.MaxDocumentationLen) {
			return RevisionRequest{}, fmt.Errorf("%w: documentation must be at most %d non-blank entries of at most %d characters", ErrValidation, assets.MaxDocumentation, assets.MaxDocumentationLen)
		}
		changes.Documentation = &items
	}

	// Fail closed on the audit id before anything is looked up: a revision
	// whose audit row could not be traced must not happen at all.
	correlationID, err := correlationID(ctx)
	if err != nil {
		return RevisionRequest{}, err
	}
	return RevisionRequest{
		ProjectID: projectID,
		AssetPID:  pid,
		Changes:   changes,
		ActorID:   actor.ID,
		Audit:     auditEntry(actor, projectID, pid, correlationID),
	}, nil
}

// requireManager resolves the actor's membership and enforces the
// maintainer-or-above gate — the default-deny server-side role rule
// projects.requireManager set for the settings surface
// (internal/application/projects/settings.go:176-188), applied here for
// the same reason and in the same words.
//
// Why a role gate rather than a matrix action: the permission matrix has
// no asset-metadata row, and specs/policies/permissions-matrix.csv is the
// policy source of truth, outside this task's scope. Rather than invent a
// policy cell, the surface takes the conservative rule the settings
// surface took under the identical situation — maintainer or above — and
// the missing row is a recorded follow-up for the Supervisor. T0109 was
// accepted on exactly this footing.
//
// "Not a member" and "role too low" answer the SAME ErrForbidden
// (nothing is disclosed either way), and both are resolved before the
// asset is looked up, so the answer does not disclose whether the pid
// exists either. A store failure answers ErrStore — fail closed, never
// guess. A project the caller cannot resolve a membership in is answered
// ErrProjectNotFound, which is the existence-hiding answer the project
// surface itself gives.
func (c *Command) requireManager(ctx context.Context, projectID, userID string) error {
	if c.members == nil {
		return fmt.Errorf("%w: revise command not fully wired", ErrStore)
	}
	membership, err := c.members.GetMembership(ctx, projectID, userID)
	switch {
	case err == nil:
	case errors.Is(err, projects.ErrMemberNotFound):
		return ErrForbidden
	case errors.Is(err, projects.ErrProjectNotFound):
		return ErrProjectNotFound
	default:
		return mapReviseError(err)
	}
	if !membership.Role.AtLeast(domain.ProjectRoleMaintainer) {
		return ErrForbidden
	}
	return nil
}

// auditEntry renders the audit row skeleton the store appends inside the
// revision transaction: who acted, how it arrived, what happened, which
// asset, which project, and the correlation id of the request that caused
// it (docs/26).
//
// BeforeSummary and AfterSummary are left EMPTY here and filled by the
// store, from the row it has locked. That is the one place this surface
// departs from projects.settingsAudit (which fills them from a pre-lock
// read): the before values of a metadata revision are the values the same
// transaction is about to overwrite, and reading them anywhere else would
// record a before that another writer could have moved. The audit row
// still commits in the same transaction as the write either way —
// settings.go:35-43 is the shape, and the shape is about WHERE the row is
// written.
//
// Via is the audit vocabulary's ViaSession: every metadata revision
// arrives as a session-authenticated /api/v1 request (the v1 guard
// challenges every state-changing method under the subtree).
// TargetRef names the asset by its persistent identifier — the identity
// every later reader resolves through — using the "<kind>:<value>" shape
// assetrights uses for the same asset.
//
// correlationID is the request's own (the observability middleware
// attaches it); a context without one — a direct service call — gets a
// fresh id, so the NOT NULL column always holds something traceable. The
// error is returned rather than swallowed so prepare can fail closed: a
// revision must not commit without a traceable audit row.
func auditEntry(actor domain.User, projectID, assetPID, correlationID string) domain.AuditEntry {
	return domain.AuditEntry{
		ActorID:       actor.ID,
		Via:           domain.ViaSession,
		Action:        domain.ActionAssetMetadataRevised,
		TargetRef:     "asset:" + assetPID,
		ProjectID:     projectID,
		CorrelationID: correlationID,
	}
}

// correlationID resolves the audit row's correlation id. It is the rule
// projects.settingsAudit states, spelled once.
func correlationID(ctx context.Context) (string, error) {
	if id, ok := observability.FromContext(ctx); ok {
		return id.String(), nil
	}
	id, err := observability.NewCorrelationID()
	if err != nil {
		return "", fmt.Errorf("%w: cannot mint audit correlation id: %v", ErrStore, err)
	}
	return id.String(), nil
}

// trimAll trims every entry of a list-shaped field. The trimmed list is
// what is validated AND what is stored — one pass, so a value a validator
// accepted cannot be a value the store trims into something else.
func trimAll(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, strings.TrimSpace(item))
	}
	return out
}

// mapReviseError keeps the sentinels of this package and of its ports, and
// turns everything else into ErrStore.
func mapReviseError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, ErrValidation),
		errors.Is(err, ErrForbidden),
		errors.Is(err, ErrProjectNotFound),
		errors.Is(err, ErrAssetNotFound),
		errors.Is(err, ErrCoverNotSupported),
		errors.Is(err, ErrStore):
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
