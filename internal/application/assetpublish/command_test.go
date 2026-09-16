package assetpublish

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// The publish command's rules, without a database. What is pinned here is
// the ORDER (which decision is taken before which read), the two
// independent refusals of an agent, the replay/conflict split of an
// Idempotency-Key, the policy question, and the shape validation — none of
// which a store could decide for the command.
//
// The fakes record their calls, because most of what is asserted here is
// about what did NOT happen: a refusal that ran before a lookup is a
// refusal that cannot disclose what the lookup would have found.

const (
	testActorID = "11111111-1111-4111-8111-111111111111"
	testProject = "22222222-2222-4222-8222-222222222222"
	// testPID is a 26-character Crockford base32 pid (no i, l, o, u).
	testPID   = "01j9z6k3m4n5p6q7r8s9t0v1w2"
	testPID2  = "01j9z6k3m4n5p6q7r8s9t0v1w3"
	testUUIDa = "33333333-3333-4333-8333-333333333333"
)

// callLog records the order in which the command touched its ports.
type callLog struct{ calls []string }

func (l *callLog) add(name string) { l.calls = append(l.calls, name) }

func (l *callLog) has(name string) bool {
	for _, c := range l.calls {
		if c == name {
			return true
		}
	}
	return false
}

func (l *callLog) String() string { return strings.Join(l.calls, ",") }

// ---------------------------------------------------------------------------
// Fakes

type fakeMembers struct {
	log  *callLog
	role *domain.ProjectRole
	err  error
}

func (m *fakeMembers) GetMembership(_ context.Context, _, _ string) (domain.ProjectMembership, error) {
	m.log.add("members")
	if m.err != nil {
		return domain.ProjectMembership{}, m.err
	}
	if m.role == nil {
		// A caller with no membership is answered exactly as the production
		// store answers one — the same sentinel for "no membership row" and
		// for "no such project" (see the port's own doc).
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return domain.ProjectMembership{ProjectID: testProject, UserID: testActorID, Role: *m.role}, nil
}

type fakePolicies struct {
	log     *callLog
	policy  domain.Policy
	err     error
	calls   int
	lastFor domain.User
}

func (p *fakePolicies) EffectivePolicy(_ context.Context, actor domain.User, _ string) (domain.EffectivePolicy, error) {
	p.log.add("policies")
	p.calls++
	p.lastFor = actor
	if p.err != nil {
		return domain.EffectivePolicy{}, p.err
	}
	return domain.EffectivePolicy{Effective: p.policy}, nil
}

type fakeRules struct {
	log      *callLog
	decision policy.Decision
	err      error
	asked    []policy.Query
}

func (r *fakeRules) Evaluate(_ context.Context, _ domain.Policy, q policy.Query) (policy.Decision, error) {
	r.log.add("rules")
	r.asked = append(r.asked, q)
	if r.err != nil {
		return policy.Decision{}, r.err
	}
	return r.decision, nil
}

// fakeStore answers the two port calls and records what it was handed.
type fakeStore struct {
	log *callLog
	// replay is the ledger answer of LookupCreation; nil means "no entry".
	replay *PublishedVersion
	// stored is what Publish answers with.
	stored    PublishedVersion
	publishIn *PublishRequest
}

func (s *fakeStore) LookupCreation(_ context.Context, _, _ string) (*PublishedVersion, error) {
	s.log.add("lookup")
	return s.replay, nil
}

func (s *fakeStore) Publish(_ context.Context, req PublishRequest) (PublishedVersion, error) {
	s.log.add("publish")
	s.publishIn = &req
	return s.stored, nil
}

// fakeEngine is a permission matrix the test writes, so that the domain
// backstop can be observed on its own: with an engine that permits
// everything, an agent is refused by the backstop or not at all.
type fakeEngine struct {
	permits bool
	err     error
	seen    []authz.Request
}

func (e *fakeEngine) Authorize(_ context.Context, req authz.Request) (authz.Decision, error) {
	e.seen = append(e.seen, req)
	if e.err != nil {
		return authz.Decision{}, e.err
	}
	if e.permits {
		return authz.Decision{Verdict: authz.VerdictAllow}, nil
	}
	return authz.Decision{Verdict: authz.VerdictDeny, Reason: "test"}, nil
}

// ---------------------------------------------------------------------------
// Harness

type harness struct {
	cmd      *Command
	log      *callLog
	members  *fakeMembers
	policies *fakePolicies
	rules    *fakeRules
	store    *fakeStore
	engine   *fakeEngine
	minted   []assets.PID
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	log := &callLog{}
	role := domain.ProjectRoleOwner
	h := &harness{
		log:      log,
		members:  &fakeMembers{log: log, role: &role},
		policies: &fakePolicies{log: log},
		rules:    &fakeRules{log: log},
		store:    &fakeStore{log: log},
		engine:   &fakeEngine{permits: true},
	}
	h.cmd = NewCommand(Deps{
		Members:  h.members,
		Policies: h.policies,
		Rules:    h.rules,
		Store:    h.store,
		Authz:    h.engine,
		NewPID: func() (assets.PID, error) {
			next := assets.PID(testPID)
			if len(h.minted) > 0 {
				next = assets.PID(testPID2)
			}
			h.minted = append(h.minted, next)
			return next, nil
		},
	})
	return h
}

// params is a publishable request: the fields a case varies are its own.
func (h *harness) params() PublishParams {
	return PublishParams{
		ProjectID:  testProject,
		AssetPID:   testPID,
		AssetType:  "dataset",
		Version:    "1.0",
		Manifest:   json.RawMessage(`{"version":1}`),
		Rights:     json.RawMessage(`{}`),
		OriginRefs: []string{"release:" + testUUIDa},
		Visibility: "private",
	}
}

func (h *harness) actor() Actor {
	return Actor{User: domain.User{ID: testActorID, Handle: "alice"}}
}

// ---------------------------------------------------------------------------
// 1. The agent backstop, on its own

// TestPublishRefusesAgentEvenWhenTheMatrixPermits: the domain backstop is
// an INDEPENDENT line, and the only way to show that is to run it against
// an engine that would have allowed the publish. docs/23 §4 keeps
// `visibility:publish` out of an agent token's scope by default; the
// matrix denies agents too (asserted separately below), but a matrix is a
// governance document a change may edit, and this refusal is the product
// rule the edit cannot waive.
func TestPublishRefusesAgentEvenWhenTheMatrixPermits(t *testing.T) {
	h := newHarness(t)
	actor := h.actor()
	actor.IsAgent = true

	_, err := h.cmd.Publish(context.Background(), actor, h.params())

	var refused *AgentNotPermittedError
	if !errors.As(err, &refused) {
		t.Fatalf("an agent publish = %v, want *AgentNotPermittedError", err)
	}
	if !errors.Is(err, ErrAgentNotPermitted) {
		t.Errorf("errors.Is(err, ErrAgentNotPermitted) = false: %v", err)
	}
	if refused.Code() != CodeAgentPublishDenied {
		t.Errorf("code = %q, want %q", refused.Code(), CodeAgentPublishDenied)
	}
	if refused.Action != "publish" {
		t.Errorf("action = %q, want publish", refused.Action)
	}
	// The refusal is resolved from the actor value alone: nothing was read,
	// so it cannot disclose whether the project or the asset exists.
	if len(h.log.calls) != 0 {
		t.Errorf("the agent refusal touched its ports: %s", h.log)
	}
	if len(h.engine.seen) != 0 {
		t.Errorf("the agent refusal consulted the matrix: %v", h.engine.seen)
	}
}

// TestPublishAuthorizationUsesTheMatrixRow: with the agent backstop out of
// the way, the matrix is the line that decides — for the row the product
// fixes (publish_private_to_public), and for the class the membership
// resolves to.
func TestPublishAuthorizationUsesTheMatrixRow(t *testing.T) {
	cases := []struct {
		name    string
		permits bool
		role    *domain.ProjectRole
		wantErr error
	}{
		{"owner permitted", true, ptr(domain.ProjectRoleOwner), nil},
		{"maintainer refused", false, ptr(domain.ProjectRoleMaintainer), ErrForbidden},
		{"contributor refused", false, ptr(domain.ProjectRoleContributor), ErrForbidden},
		{"non-member refused", false, nil, ErrForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.members.role = c.role
			h.engine.permits = c.permits

			_, err := h.cmd.Publish(context.Background(), h.actor(), h.params())
			if c.wantErr == nil {
				if err != nil {
					t.Fatalf("publish = %v, want success", err)
				}
			} else if !errors.Is(err, c.wantErr) {
				t.Fatalf("publish = %v, want %v", err, c.wantErr)
			}
			if len(h.engine.seen) != 1 {
				t.Fatalf("the matrix was asked %d times, want once", len(h.engine.seen))
			}
			if h.engine.seen[0].Action != authz.ActionPublishPrivateToPublic {
				t.Errorf("action = %q, want %q", h.engine.seen[0].Action, authz.ActionPublishPrivateToPublic)
			}
			wantClass := authz.ClassOf(true, c.role, false)
			if h.engine.seen[0].Class != wantClass {
				t.Errorf("class = %q, want %q", h.engine.seen[0].Class, wantClass)
			}
			if c.wantErr != nil && h.store.publishIn != nil {
				t.Errorf("a refused publish reached the store: %+v", h.store.publishIn)
			}
		})
	}
}

// TestPublishAgentDeniedByTheMatrixToo: the second line. With the backstop
// bypassed (the matrix is asked directly, as the command asks it), the
// agent column of the publish row denies on its own — and it denies an
// agent that is the project's OWNER, which is the cell a human owner is
// allowed by.
func TestPublishAgentDeniedByTheMatrixToo(t *testing.T) {
	engine := authz.NewMatrixEngine()
	owner := domain.ProjectRoleOwner

	human, err := engine.Authorize(context.Background(), authz.Request{
		Action: authz.ActionPublishPrivateToPublic,
		Class:  authz.ClassOf(true, &owner, false),
	})
	if err != nil {
		t.Fatalf("authorize human owner: %v", err)
	}
	if !human.Permits() {
		t.Fatalf("the human owner is not permitted to publish: %+v", human)
	}

	agent, err := engine.Authorize(context.Background(), authz.Request{
		Action: authz.ActionPublishPrivateToPublic,
		Class:  authz.ClassOf(true, &owner, true),
	})
	if err != nil {
		t.Fatalf("authorize agent owner: %v", err)
	}
	if agent.Permits() {
		t.Fatalf("an agent owner is permitted to publish: %+v", agent)
	}

	maintainer := domain.ProjectRoleMaintainer
	cond, err := engine.Authorize(context.Background(), authz.Request{
		Action: authz.ActionPublishPrivateToPublic,
		Class:  authz.ClassOf(true, &maintainer, false),
	})
	if err != nil {
		t.Fatalf("authorize maintainer: %v", err)
	}
	if cond.Permits() {
		t.Errorf("a maintainer is permitted to publish: %+v (a conditional cell with no resolver must fail closed)", cond)
	}
}

// ---------------------------------------------------------------------------
// 2. The order: authorization precedes every lookup

// TestPublishDenialPrecedesEveryLookup: a refusal that resolves before the
// target is read cannot disclose whether the target exists (the
// releases.ErrForbidden rule). The project id is opaque to the client, so
// an unknown one and a forbidden one must be the same answer.
func TestPublishDenialPrecedesEveryLookup(t *testing.T) {
	h := newHarness(t)
	h.engine.permits = false

	_, err := h.cmd.Publish(context.Background(), h.actor(), h.params())
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("publish = %v, want ErrForbidden", err)
	}
	if h.log.has("policies") || h.log.has("lookup") || h.log.has("publish") {
		t.Errorf("the denial ran after a lookup: %s", h.log)
	}
	if len(h.log.calls) != 1 || h.log.calls[0] != "members" {
		t.Errorf("calls = %s, want the membership read alone", h.log)
	}
}

// TestPublishUnknownProjectIsNotDisclosed: the membership port answers
// "not a member" for a project that does not exist, and the command must
// relay that as a denial rather than as a not-found. A not-found here
// would answer a caller probing for projects with this route — and it
// would answer it with the one fact the denial is meant to withhold.
func TestPublishUnknownProjectIsNotDisclosed(t *testing.T) {
	h := newHarness(t)
	h.members.err = projects.ErrMemberNotFound
	h.engine.permits = false

	in := h.params()
	in.ProjectID = "44444444-4444-4444-8444-444444444444"
	_, err := h.cmd.Publish(context.Background(), h.actor(), in)
	if errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("an unknown project answered a not-found: %v", err)
	}
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("publish = %v, want ErrForbidden", err)
	}
}

// TestPublishRelaysTheProjectSurfacesNotFound: the one disclosure the
// project surface DOES make deliberately (its own gate answers a hidden
// project with a not-found) is relayed as itself, and only it.
func TestPublishRelaysTheProjectSurfacesNotFound(t *testing.T) {
	h := newHarness(t)
	h.members.err = projects.ErrProjectNotFound

	_, err := h.cmd.Publish(context.Background(), h.actor(), h.params())
	if !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("publish = %v, want ErrProjectNotFound", err)
	}
	if h.log.has("lookup") || h.log.has("publish") {
		t.Errorf("the refusal ran after a lookup: %s", h.log)
	}
}

// TestPublishNotPermittedWhenTheMembershipReadFails: an unreadable
// membership is not a permission and not a denial — it is a failure, and
// it must not be answered as either.
func TestPublishNotPermittedWhenTheMembershipReadFails(t *testing.T) {
	h := newHarness(t)
	h.members.err = errors.New("connection reset")

	_, err := h.cmd.Publish(context.Background(), h.actor(), h.params())
	if !errors.Is(err, ErrStore) {
		t.Fatalf("publish = %v, want ErrStore", err)
	}
	if h.store.publishIn != nil {
		t.Errorf("a store failure reached the publish: %+v", h.store.publishIn)
	}
}

// TestPublishFailsClosedWithoutAnEngine: a command that was never wired an
// authorization engine refuses rather than publishing unchecked.
func TestPublishFailsClosedWithoutAnEngine(t *testing.T) {
	h := newHarness(t)
	h.cmd.authz = nil

	_, err := h.cmd.Publish(context.Background(), h.actor(), h.params())
	if !errors.Is(err, ErrStore) {
		t.Fatalf("publish = %v, want ErrStore (a missing engine is never permission)", err)
	}
}

// ---------------------------------------------------------------------------
// 3. Idempotency

// TestPublishReplaysTheKeyItAlreadyPublished: a replay is a READ: the key
// returns the version the first call stored, and nothing is written,
// gated, or asked of the policy.
func TestPublishReplaysTheKeyItAlreadyPublished(t *testing.T) {
	h := newHarness(t)
	stored := PublishedVersion{ID: testUUIDa, AssetPID: testPID, Version: "1.0"}
	h.store.replay = &stored
	key := "key-1"

	in := h.params()
	in.IdempotencyKey = &key
	got, err := h.cmd.Publish(context.Background(), h.actor(), in)
	if err != nil {
		t.Fatalf("replay = %v, want the stored version", err)
	}
	if got.ID != stored.ID || got.Version != stored.Version {
		t.Errorf("replay = %+v, want %+v", got, stored)
	}
	if h.log.has("publish") {
		t.Errorf("a replay published: %s", h.log)
	}
	if h.log.has("policies") || h.log.has("rules") {
		t.Errorf("a replay consulted the policy: %s", h.log)
	}
	if len(h.engine.seen) != 1 {
		t.Errorf("a replay ran %d authorization checks, want the one that authorizes the request", len(h.engine.seen))
	}
}

// TestPublishReplayOfACreateDoesNotCompareTheMintedPID: a create request's
// pid is minted per request, so comparing it would turn every repeat of a
// create into a conflict — which is exactly the case the key exists for.
func TestPublishReplayOfACreateDoesNotCompareTheMintedPID(t *testing.T) {
	h := newHarness(t)
	// The key published an asset under the pid the FIRST call minted; this
	// request mints testPID (the harness's first value) and the stored row
	// carries testPID2.
	stored := PublishedVersion{ID: testUUIDa, AssetPID: testPID2, Version: "1.0"}
	h.store.replay = &stored
	key := "key-create"

	in := h.params()
	in.AssetPID = ""
	in.Title = "New asset"
	in.Slug = "new-asset"
	in.IdempotencyKey = &key

	got, err := h.cmd.Publish(context.Background(), h.actor(), in)
	if err != nil {
		t.Fatalf("replay of a create = %v, want the stored version", err)
	}
	if got.AssetPID != testPID2 {
		t.Errorf("replay = %+v, want the version the key published", got)
	}
	if h.log.has("publish") {
		t.Errorf("a replay published: %s", h.log)
	}
}

// TestPublishIdempotencyConflictOnADifferentTarget: a key reused for a
// different publication is a client error, not a replay — answering it
// with the other publication's row would hand back a version the caller
// did not ask for (docs/45).
func TestPublishIdempotencyConflictOnADifferentTarget(t *testing.T) {
	key := "key-1"
	cases := []struct {
		name   string
		stored PublishedVersion
		mutate func(*PublishParams)
	}{
		{
			name:   "another version of the same asset",
			stored: PublishedVersion{ID: testUUIDa, AssetPID: testPID, Version: "2.0"},
		},
		{
			name:   "another asset",
			stored: PublishedVersion{ID: testUUIDa, AssetPID: testPID2, Version: "1.0"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			stored := c.stored
			h.store.replay = &stored

			in := h.params()
			in.IdempotencyKey = &key
			if c.mutate != nil {
				c.mutate(&in)
			}
			_, err := h.cmd.Publish(context.Background(), h.actor(), in)
			if !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("publish = %v, want ErrIdempotencyConflict", err)
			}
			if h.log.has("publish") {
				t.Errorf("a conflict published: %s", h.log)
			}
		})
	}
}

// TestPublishWithoutAKeyNeverLooksUpTheLedger: a request with no
// Idempotency-Key has nothing to replay, and must not be answered from
// another caller's ledger row.
func TestPublishWithoutAKeyNeverLooksUpTheLedger(t *testing.T) {
	h := newHarness(t)
	stored := PublishedVersion{ID: testUUIDa, AssetPID: testPID, Version: "1.0"}
	h.store.replay = &stored // would be returned if it were consulted

	got, err := h.cmd.Publish(context.Background(), h.actor(), h.params())
	if err != nil {
		t.Fatalf("publish = %v", err)
	}
	if got.ID == stored.ID {
		t.Errorf("a request with no key was answered from the ledger: %+v", got)
	}
	if h.log.has("lookup") {
		t.Errorf("a request with no key looked the ledger up: %s", h.log)
	}
}

// ---------------------------------------------------------------------------
// 4. The policy question

// TestPublishPolicyIsAskedOnlyForAPublicVersion: the rule is domain.
// RulePublicAssetIPReview — "requires an IP review before a PUBLIC asset
// is published" — so a private publication widens nothing and the policy
// is not read at all.
func TestPublishPolicyIsAskedOnlyForAPublicVersion(t *testing.T) {
	h := newHarness(t)
	in := h.params()
	in.Visibility = "private"

	if _, err := h.cmd.Publish(context.Background(), h.actor(), in); err != nil {
		t.Fatalf("private publish = %v", err)
	}
	if h.policies.calls != 0 || len(h.rules.asked) != 0 {
		t.Errorf("a private publish read the policy: policies=%d rules=%d", h.policies.calls, len(h.rules.asked))
	}

	h2 := newHarness(t)
	in2 := h2.params()
	in2.Visibility = "public"
	if _, err := h2.cmd.Publish(context.Background(), h2.actor(), in2); err != nil {
		t.Fatalf("public publish = %v", err)
	}
	if h2.policies.calls != 1 || len(h2.rules.asked) != 1 {
		t.Fatalf("a public publish read the policy %d/%d times, want once each", h2.policies.calls, len(h2.rules.asked))
	}
	if h2.rules.asked[0].Rule != domain.RulePublicAssetIPReview {
		t.Errorf("rule = %q, want %q", h2.rules.asked[0].Rule, domain.RulePublicAssetIPReview)
	}
	if h2.policies.lastFor.ID != testActorID {
		t.Errorf("the policy was read for %q, want the publishing actor", h2.policies.lastFor.ID)
	}
	// The policy read happens AFTER the authorization: the effective policy
	// resolves the project, and a denied caller must not reach it.
	if i, j := indexOf(h2.log.calls, "members"), indexOf(h2.log.calls, "policies"); i > j {
		t.Errorf("the policy was read before the authorization: %s", h2.log)
	}
}

// TestPublishPolicyRefusesOnlyWhenItRequiresAReview: three of the four
// answers are refusals and one is not. An ABSENT rule is not a refusal —
// docs/23 §4 lists the org policy approvals as optional, so a policy that
// does not require an IP review does not require one. A rule a policy
// CANNOT be read or evaluated for is a refusal: an unreadable policy is
// not a permissive one.
func TestPublishPolicyRefusesOnlyWhenItRequiresAReview(t *testing.T) {
	cases := []struct {
		name     string
		decision policy.Decision
		readErr  error
		evalErr  error
		want     error
	}{
		{"absent rule permits", policy.Decision{Found: false}, nil, nil, nil},
		{"rule false permits", policy.Decision{Found: true, Bool: false}, nil, nil, nil},
		{"rule true refuses", policy.Decision{Found: true, Bool: true}, nil, nil, ErrPolicyRefused},
		{"unreadable policy refuses", policy.Decision{}, errors.New("policy store down"), nil, ErrPolicyRefused},
		{"unevaluable policy refuses", policy.Decision{}, nil, errors.New("bad policy json"), ErrPolicyRefused},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.policies.err = c.readErr
			h.rules.decision = c.decision
			h.rules.err = c.evalErr

			in := h.params()
			in.Visibility = "public"
			_, err := h.cmd.Publish(context.Background(), h.actor(), in)
			if c.want == nil {
				if err != nil {
					t.Fatalf("publish = %v, want success", err)
				}
				if h.store.publishIn == nil {
					t.Fatalf("the publish never reached the store")
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("publish = %v, want %v", err, c.want)
			}
			var refused *PolicyRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("publish = %v, want *PolicyRefusedError", err)
			}
			if refused.Code() != CodePolicyRefused {
				t.Errorf("code = %q, want %q", refused.Code(), CodePolicyRefused)
			}
			if refused.Rule != domain.RulePublicAssetIPReview {
				t.Errorf("rule = %q, want %q", refused.Rule, domain.RulePublicAssetIPReview)
			}
			if h.store.publishIn != nil {
				t.Errorf("a policy-refused publish reached the store: %+v", h.store.publishIn)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 5. The request shape

// TestPublishValidationRefusesTheShapeBeforeAnyRead: the shape is validated
// before anything is read — a body that is not a candidate costs no query.
func TestPublishValidationRefusesTheShapeBeforeAnyRead(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PublishParams)
		want   string
	}{
		{"no project", func(p *PublishParams) { p.ProjectID = "  " }, "project_id is required"},
		{"unknown asset type", func(p *PublishParams) { p.AssetType = "sourdough" }, "asset_type"},
		{"empty version", func(p *PublishParams) { p.Version = "" }, "version"},
		{"version too long", func(p *PublishParams) { p.Version = strings.Repeat("v", assets.MaxVersionLen+1) }, "version"},
		{"unknown visibility", func(p *PublishParams) { p.Visibility = "unlisted" }, "visibility"},
		{"pid that is not a pid", func(p *PublishParams) { p.AssetPID = "not-a-pid" }, "persistent identifier"},
		{"title on a continuation", func(p *PublishParams) { p.Title = "Renamed" }, "title is not accepted"},
		{"slug on a continuation", func(p *PublishParams) { p.Slug = "renamed" }, "slug is not accepted"},
		{"title too long", func(p *PublishParams) {
			p.AssetPID, p.Title, p.Slug = "", strings.Repeat("t", MaxTitleLen+1), "s"
		}, "title must be 1.."},
		{"slug too long", func(p *PublishParams) {
			p.AssetPID, p.Title, p.Slug = "", "t", strings.Repeat("s", MaxSlugLen+1)
		}, "slug must be 1.."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			in := h.params()
			c.mutate(&in)

			_, err := h.cmd.Publish(context.Background(), h.actor(), in)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("publish = %v, want ErrValidation", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name the field (%q)", err, c.want)
			}
			if len(h.log.calls) != 0 {
				t.Errorf("a malformed request was read for: %s", h.log)
			}
		})
	}
}

// TestPublishCreateRequiresTheDisplayFields: a publish that names no asset
// CREATES one, and an asset with no name is not an asset.
func TestPublishCreateRequiresTheDisplayFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PublishParams)
		want   string
	}{
		{"no title", func(p *PublishParams) { p.AssetPID = ""; p.Title = ""; p.Slug = "s" }, "title is required"},
		{"no slug", func(p *PublishParams) { p.AssetPID = ""; p.Title = "T"; p.Slug = "" }, "slug is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			in := h.params()
			c.mutate(&in)
			_, err := h.cmd.Publish(context.Background(), h.actor(), in)
			if !errors.Is(err, ErrValidation) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("publish = %v, want a validation refusal naming %q", err, c.want)
			}
		})
	}
}

// TestPublishMintsThePIDForACreate: the pid comes from the application
// (assets.NewPID), not from the column DEFAULT (migration 00064's note).
// The command mints it through the injected generator, so a test can see
// the value that reaches the store.
func TestPublishMintsThePIDForACreate(t *testing.T) {
	h := newHarness(t)
	in := h.params()
	in.AssetPID = ""
	in.Title = "Fresh asset"
	in.Slug = "fresh-asset"

	if _, err := h.cmd.Publish(context.Background(), h.actor(), in); err != nil {
		t.Fatalf("create = %v", err)
	}
	if len(h.minted) != 1 {
		t.Fatalf("minted %d pids, want one", len(h.minted))
	}
	if h.store.publishIn == nil {
		t.Fatalf("the create never reached the store")
	}
	if got := string(h.store.publishIn.Candidate.AssetPID); got != string(h.minted[0]) {
		t.Errorf("the store was handed pid %q, want the minted %q", got, h.minted[0])
	}
	if !h.store.publishIn.NewAsset {
		t.Errorf("NewAsset = false for a publish that named no asset")
	}
	if h.store.publishIn.Slug != "fresh-asset" || h.store.publishIn.Title != "Fresh asset" {
		t.Errorf("display fields = %q/%q, want the request's", h.store.publishIn.Title, h.store.publishIn.Slug)
	}
	if !ValidPID(assets.PID(testPID)) {
		t.Fatalf("the fixture pid is not a pid")
	}
}

// TestPublishContinuationIsNotACreate: a request that names an asset
// continues it, and the store is told so — a pid that names nothing is a
// refusal the store raises, never a silent create under the caller's
// identity.
func TestPublishContinuationIsNotACreate(t *testing.T) {
	h := newHarness(t)
	if _, err := h.cmd.Publish(context.Background(), h.actor(), h.params()); err != nil {
		t.Fatalf("publish = %v", err)
	}
	if h.store.publishIn.NewAsset {
		t.Errorf("NewAsset = true for a request that named an existing asset")
	}
	if len(h.minted) != 0 {
		t.Errorf("a continuation minted a pid: %v", h.minted)
	}
}

// TestPublishCanonicalisesOriginRefsThroughNewOriginRef: the refs the row
// stores are the canonical kind:value form. A ref that is not canonical is
// passed through untouched, because the gate is the component that refuses
// it and it must refuse the spelling the caller actually sent.
func TestPublishCanonicalisesOriginRefsThroughNewOriginRef(t *testing.T) {
	h := newHarness(t)
	in := h.params()
	in.OriginRefs = []string{
		"release:" + testUUIDa,
		"  release:" + testUUIDa + "  ", // not canonical: the gate refuses it by name
		"nonsense",
		"release:" + strings.ToUpper(testUUIDa),
	}
	if _, err := h.cmd.Publish(context.Background(), h.actor(), in); err != nil {
		t.Fatalf("publish = %v", err)
	}
	got := h.store.publishIn.Candidate.OriginRefs
	if len(got) != 4 {
		t.Fatalf("refs = %v, want all four passed through", got)
	}
	if got[0] != "release:"+testUUIDa {
		t.Errorf("refs[0] = %q, want the canonical form", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i] != in.OriginRefs[i] {
			t.Errorf("refs[%d] = %q, want it passed through unchanged as %q", i, got[i], in.OriginRefs[i])
		}
	}
	// And the canonical form is exactly what NewOriginRef renders.
	ref, ok := assets.NewOriginRef(assets.KindRelease, testUUIDa)
	if !ok || string(ref) != got[0] {
		t.Errorf("the stored ref %q is not assets.NewOriginRef's form %q", got[0], ref)
	}
}

// TestPublishAuditRowNamesThePublisher: the audit row is rendered by the
// command and appended by the store inside its transaction, so its shape
// is pinned here — including the target ref, which the store fills once
// the insert has assigned the id.
func TestPublishAuditRowNamesThePublisher(t *testing.T) {
	h := newHarness(t)
	in := h.params()
	in.Visibility = "public"
	if _, err := h.cmd.Publish(context.Background(), h.actor(), in); err != nil {
		t.Fatalf("publish = %v", err)
	}
	audit := h.store.publishIn.Audit
	if audit.ActorID != testActorID {
		t.Errorf("audit actor = %q, want the publishing user", audit.ActorID)
	}
	if audit.Action != ActionAssetVersionPublished {
		t.Errorf("audit action = %q, want %q", audit.Action, ActionAssetVersionPublished)
	}
	if audit.ProjectID != testProject {
		t.Errorf("audit project = %q, want the publish's", audit.ProjectID)
	}
	if audit.Via != domain.ViaSession {
		t.Errorf("audit via = %q, want a session", audit.Via)
	}
	if audit.TargetRef != "" {
		t.Errorf("audit target = %q, want it left for the store to fill with the assigned id", audit.TargetRef)
	}
	summary, ok := audit.AfterSummary.(map[string]any)
	if !ok {
		t.Fatalf("audit summary = %T, want a JSON object", audit.AfterSummary)
	}
	if summary["version"] != "1.0" || summary["visibility"] != "public" {
		t.Errorf("audit summary = %v, want the published version and visibility", summary)
	}
	if summary["asset_id"] != testPID {
		t.Errorf("audit summary asset_id = %v, want the asset's pid", summary["asset_id"])
	}
}

// TestPublishStoreFailureIsNotAPermission: the store's failures are
// relayed as failures. A caller must not be able to read "the database was
// down" as "you may not publish" (or the reverse).
func TestPublishStoreFailureIsNotAPermission(t *testing.T) {
	h := newHarness(t)
	h.store.replay = nil
	h.cmd.store = &failingStore{err: errors.New("connection refused")}

	_, err := h.cmd.Publish(context.Background(), h.actor(), h.params())
	if !errors.Is(err, ErrStore) {
		t.Fatalf("publish = %v, want ErrStore", err)
	}
	if errors.Is(err, ErrForbidden) || errors.Is(err, ErrRefused) {
		t.Errorf("a store failure was answered as a permission or a refusal: %v", err)
	}
}

type failingStore struct{ err error }

func (s *failingStore) LookupCreation(context.Context, string, string) (*PublishedVersion, error) {
	return nil, nil
}

func (s *failingStore) Publish(context.Context, PublishRequest) (PublishedVersion, error) {
	return PublishedVersion{}, s.err
}

// ---------------------------------------------------------------------------

func ptr[T any](v T) *T { return &v }

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}
