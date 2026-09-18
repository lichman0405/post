package knowledgepublish

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rights"
)

// ActionKnowledgeVersionPublished is the audit action a successful publish
// appends (the audit_log.action column). It is domain's, next to
// release.created and pull_request.merged, because a publication is a
// governed write of the same class — see the constant's own comment for
// why it may spell its event's name while the asset command's may not.
const ActionKnowledgeVersionPublished = domain.ActionKnowledgeVersionPublished

// Command is the knowledge publication use case (T0805): the governance
// write that publishes one scientific object version to the network.
//
// # The order of the steps, and why it is the order
//
//  1. shape      — validate the request, before anything is read
//  2. agent      — the domain backstop, before anything is read
//  3. authorize  — membership class vs the matrix, before anything is read
//  4. replay     — an Idempotency-Key that already published, as a READ
//  5. publish    — the store's transaction: re-check the ledger, resolve
//     the facts, re-run Judge, and write
//
// Steps 2 and 3 come before every lookup on purpose: a refusal there must
// not disclose whether the project or the version exists (the
// releases.ErrForbidden rule — "resolved before any target lookup, so the
// denial never discloses whether the project exists").
//
// # The matrix row is evaluated for EVERY publication
//
// The action's name is `publish_private_to_public` and this command
// evaluates it even though a publication widens nothing (the version's
// visibility axis is untouched by publishing it). That is the literal
// reading of the row and of internal/application/contribution/doc.go,
// which requires the consuming API to map Publicize onto exactly this
// action. Its visible consequence is intended and fail-closed: with the
// matrix as it stands (maintainer = conditional, owner = allow) V1
// publications are an OWNER action, because a conditional cell whose
// condition has no specification (issue #237) resolves to a refusal —
// internal/authz's Permits() admits only `allow`.
type Command struct {
	members MembershipPort
	store   StorePort
	authz   authz.Engine
	newPID  func() (domain.PID, error)
}

// Deps carries the adapters a Command is wired over.
type Deps struct {
	// Members resolves the actor's project membership (the production
	// value is *persistence.ProjectStore).
	Members MembershipPort
	// Store is the publication store (the production value is
	// *persistence.KnowledgePublishStore). It answers the fact resolution
	// too — including the review record — so this command reads nothing
	// itself.
	Store StorePort
	// Authz is the permission-matrix engine (the production value is
	// authz.NewMatrixEngine()).
	Authz authz.Engine
	// NewPID mints the publication's persistent identifier. Tests inject a
	// deterministic sequence; production leaves it nil and gets
	// domain.NewPID.
	NewPID func() (domain.PID, error)
}

// NewCommand wires the publication command.
func NewCommand(deps Deps) *Command {
	c := &Command{
		members: deps.Members,
		store:   deps.Store,
		authz:   deps.Authz,
		newPID:  deps.NewPID,
	}
	if c.newPID == nil {
		c.newPID = func() (domain.PID, error) { return domain.NewPID() }
	}
	return c
}

// Preview resolves what the publish would be and answers the PROPOSAL:
// whether it would be admitted, and who would be able to read it once
// written. It writes nothing.
//
// It takes the actor and does NOT apply the agent backstop, and that is
// the product rule rather than an oversight: specs/mcp/tools.json lists
// knowledge.publish_preview as mode `proposal` while
// publish_private_to_public sits in forbidden_default_agent_actions — an
// agent may ask what the publication would mean and may not perform it.
// Refusing the proposal to an agent would make the one thing the tool
// exists for impossible.
//
// Who may ask is the transport's decision (the project read gate, exactly
// as the asset preview's route takes it); this command answers, it does
// not gate.
func (c *Command) Preview(ctx context.Context, in PreviewParams) (Preview, error) {
	projectID, versionID, doc, err := c.prepare(in.ProjectID, in.ObjectVersionID, in.Rights)
	if err != nil {
		return Preview{}, err
	}
	if c.store == nil {
		return Preview{}, fmt.Errorf("%w: publish command not fully wired", ErrStore)
	}
	facts, err := c.store.ResolveFacts(ctx, projectID, versionID)
	if err != nil {
		return Preview{}, mapPublishError(err)
	}
	facts.Rights = doc
	return previewOf(facts), nil
}

// Publish runs the publish and returns the stored publication.
func (c *Command) Publish(ctx context.Context, actor Actor, in PublishParams) (Published, error) {
	if err := requireAgent(actor); err != nil {
		return Published{}, err
	}
	projectID, versionID, doc, err := c.prepare(in.ProjectID, in.ObjectVersionID, in.Rights)
	if err != nil {
		return Published{}, err
	}
	publicVersion, err := validatePublicVersion(in.PublicVersion)
	if err != nil {
		return Published{}, err
	}
	if err := c.requirePublish(ctx, actor.User.ID, projectID, actor.IsAgent); err != nil {
		return Published{}, err
	}
	// The pid is minted HERE, before the replay is read, for the reason
	// the asset command mints one in the same place: a replay never
	// compares an identity the caller did not send, and a request that
	// turns out to be a replay discards this one.
	pid, err := c.mintPID()
	if err != nil {
		return Published{}, err
	}
	if in.IdempotencyKey != nil {
		replayed, err := c.store.LookupCreation(ctx, projectID, *in.IdempotencyKey)
		if err != nil {
			return Published{}, mapPublishError(err)
		}
		if replayed != nil {
			if err := replayedMatches(*replayed, versionID); err != nil {
				return Published{}, err
			}
			return *replayed, nil
		}
	}
	// The facts the refusal report is rendered from. The store resolves
	// them again inside its transaction and decides on THAT resolution;
	// this one exists so a refusal can answer with the same document the
	// preview route would have answered with.
	var facts Facts
	if in.IdempotencyKey == nil {
		// Only when no replay can short-circuit the work: with a key, the
		// store's transaction re-check is what decides, and a read here
		// would be a read whose answer is thrown away in the common case.
		resolved, err := c.store.ResolveFacts(ctx, projectID, versionID)
		if err != nil {
			return Published{}, mapPublishError(err)
		}
		facts = resolved
	}
	facts.Rights = doc
	stored, err := c.store.Publish(ctx, PublishRequest{
		ProjectID:       projectID,
		ObjectVersionID: versionID,
		PID:             string(pid),
		PublicVersion:   publicVersion,
		RightsJSON:      mustMarshal(doc),
		Facts:           facts,
		Actor:           actor,
		Audit:           auditEntry(actor.User, projectID, publicVersion, doc),
		IdempotencyKey:  in.IdempotencyKey,
	})
	if err != nil {
		return Published{}, mapPublishError(err)
	}
	return stored, nil
}

// prepare validates the request shape and parses the rights document,
// before anything is read.
func (c *Command) prepare(projectID, objectVersionID string, rawRights []byte) (string, string, rights.Document, error) {
	project := strings.TrimSpace(projectID)
	if project == "" {
		return "", "", rights.Document{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	version := strings.TrimSpace(objectVersionID)
	if !validUUIDText(version) {
		return "", "", rights.Document{}, fmt.Errorf("%w: knowledge_version_ref must name a scientific object version (object_version:<uuid>)", ErrValidation)
	}
	doc, err := rights.Parse(rawRights)
	if err != nil {
		return "", "", rights.Document{}, fmt.Errorf("%w: rights must be a %d-version rights document (specs/policies/rights-template.yaml): %v", ErrValidation, rights.DocumentVersion, err)
	}
	return project, version, doc, nil
}

// validatePublicVersion applies the publisher's name rules: present, not
// blank, and bounded. It does NOT normalise — the returned string is the
// caller's, byte for byte (owner ruling L3-20260916-1 #2: 服务端原样存,
// 不做推导、不做格式化).
//
// Blank (empty, or nothing but space) is refused rather than trimmed: a
// publication whose name is whitespace is a publication with no name, and
// trimming one would be the normalisation this rule forbids. A name that
// merely STARTS or ENDS with space is a legal, odd name and is stored
// exactly as sent — a test pins one.
func validatePublicVersion(v string) (string, error) {
	if v == "" {
		return "", fmt.Errorf("%w: public_version is required — it is the name the publication is shown under, and this server does not invent one", ErrValidation)
	}
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("%w: public_version must not be blank", ErrValidation)
	}
	if utf8.RuneCountInString(v) > MaxPublicVersionLen {
		return "", fmt.Errorf("%w: public_version must be 1..%d characters", ErrValidation, MaxPublicVersionLen)
	}
	return v, nil
}

// mintPID mints the publication's persistent identifier and checks the
// generator's output against the vocabulary the database CHECK enforces —
// a generator that produced something else would be refused by PostgreSQL
// with a constraint error that says nothing about which component is
// wrong.
func (c *Command) mintPID() (domain.PID, error) {
	pid, err := c.newPID()
	if err != nil {
		return "", fmt.Errorf("%w: mint a pid: %v", ErrStore, err)
	}
	if !pid.Valid() {
		return "", fmt.Errorf("%w: the pid generator produced %q, which is not a persistent identifier (26 lowercase Crockford base32 characters)", ErrStore, pid)
	}
	return pid, nil
}

// requirePublish authorizes the publish: resolve the actor's membership
// role and evaluate authz.ActionPublishPrivateToPublic for the class.
//
// The denial precedes every lookup, and that is the point rather than an
// artifact of the ordering: the membership resolution answers
// projects.ErrMemberNotFound for a project that does not exist exactly as
// it does for one the caller does not belong to, so a caller probing for
// projects with this route is answered "not permitted" either way. Only
// projects.ErrProjectNotFound — the deliberate disclosure the project
// surface makes for an invisible project — is relayed as a not-found.
//
// A nil or erroring engine is a wiring failure, never a permission: the
// refusal is ErrStore. Same shape as assetpublish.Command.requirePublish.
func (c *Command) requirePublish(ctx context.Context, actorID, projectID string, isAgent bool) error {
	if c.members == nil || c.authz == nil {
		return fmt.Errorf("%w: publish command not fully wired", ErrStore)
	}
	membership, err := c.members.GetMembership(ctx, projectID, actorID)
	var role *domain.ProjectRole
	switch {
	case err == nil:
		r := membership.Role
		role = &r
	case errors.Is(err, projects.ErrMemberNotFound):
		role = nil
	case errors.Is(err, projects.ErrProjectNotFound):
		return ErrProjectNotFound
	default:
		return mapPublishError(err)
	}
	decision, err := c.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionPublishPrivateToPublic,
		Class:  authz.ClassOf(true, role, isAgent),
	})
	if err != nil {
		return fmt.Errorf("%w: authorize publish: %v", ErrStore, err)
	}
	if !decision.Permits() {
		return ErrForbidden
	}
	return nil
}

// requireAgent is the domain backstop: an agent never publishes, whatever
// the matrix says. It reads the actor value alone, so it is resolved
// before any lookup.
func requireAgent(actor Actor) error {
	if actor.IsAgent {
		return &AgentNotPermittedError{Action: "publish"}
	}
	return nil
}

// previewOf renders the preview of a decision input: the audience the
// publication would have and every entry that blocks it. It is the same
// rendering the store produces for a refusal, so what a caller previewed
// and what a refusal reports cannot be two different documents.
func previewOf(f Facts) Preview {
	approved := f.ReviewKinds()
	blocking := Judge(f)
	return Preview{
		ProjectID:           f.ProjectID,
		ObjectVersionID:     f.ObjectVersionID,
		ObjectID:            f.ObjectID,
		ObjectType:          f.ObjectType,
		Audience:            AudienceFor(f.ProjectVisibility, f.VisibilityPolicyID, f.Rights),
		ProjectVisibility:   f.ProjectVisibility,
		VisibilityPolicyID:  f.VisibilityPolicyID,
		ReviewApproved:      f.ReviewApproved(),
		ApprovedReviewKinds: approved,
		RequiredReviewKinds: RequiredReviewKinds(),
		Existing:            f.Published,
		Blocking:            blocking,
		Publishable:         len(blocking) == 0,
	}
}

// PreviewOf renders the preview of a resolved decision input. The store
// uses it for the refusal it reports, so a refusal and the preview that
// preceded it are one document.
func PreviewOf(f Facts) Preview { return previewOf(f) }

// replayedMatches decides whether an Idempotency-Key's stored publication
// is the SAME publication the caller is repeating (replay) or a different
// one (conflict, docs/45).
//
// What is compared is the version the key published: a key that published
// version A may not answer for a request that asks for version B — the
// caller would receive a row it did not ask for and could not tell. The
// public_version is NOT compared, and that is deliberate rather than
// lenient: a replay is a RETRY, and a caller retrying after a lost
// response may well have re-sent a request whose name field it edited
// while waiting. The row the first call wrote is the row it wrote; the
// caller is told which one that was.
func replayedMatches(stored Published, objectVersionID string) error {
	if stored.ObjectVersionID != objectVersionID {
		return fmt.Errorf("%w: this Idempotency-Key published knowledge object version %q, and this request is for version %q",
			ErrIdempotencyConflict, stored.ObjectVersionID, objectVersionID)
	}
	return nil
}

// auditEntry renders the audit row the store appends inside the publish
// transaction. TargetRef is left empty: the store fills it with
// "knowledge_publication:<id>" once the insert has assigned the id.
func auditEntry(actor domain.User, projectID, publicVersion string, doc rights.Document) domain.AuditEntry {
	return domain.AuditEntry{
		ActorID:   actor.ID,
		Via:       domain.ViaSession,
		Action:    ActionKnowledgeVersionPublished,
		ProjectID: projectID,
		AfterSummary: map[string]any{
			"public_version":      publicVersion,
			"rights_document":     rights.DocumentVersion,
			"metadata_visibility": string(doc.Visibility.Metadata),
			"data_access":         string(doc.Visibility.DataAccess),
		},
	}
}

// mustMarshal renders the canonical bytes of a document that has already
// been parsed and validated. The only failure is an unrepresentable
// document, which Parse cannot produce; a nil result is impossible and is
// mapped to an empty document rather than panicking.
func mustMarshal(doc rights.Document) []byte {
	b, err := doc.Marshal()
	if err != nil {
		return nil
	}
	return b
}

// mapPublishError keeps the sentinels of this package and of its ports
// (the port contracts) and turns everything else into ErrStore.
func mapPublishError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, ErrValidation),
		errors.Is(err, ErrAgentNotPermitted),
		errors.Is(err, ErrForbidden),
		errors.Is(err, ErrProjectNotFound),
		errors.Is(err, ErrVersionNotFound),
		errors.Is(err, ErrAlreadyPublished),
		errors.Is(err, ErrReviewRequired),
		errors.Is(err, ErrIdempotencyConflict),
		errors.Is(err, ErrRefused),
		errors.Is(err, ErrStore):
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}

// validUUIDText reports whether s is a uuid in canonical text form. It is
// the shape check the database performs on the column's cast, done here so
// that a reference that is not a version id is a validation refusal rather
// than a storage error.
func validUUIDText(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
			continue
		}
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
