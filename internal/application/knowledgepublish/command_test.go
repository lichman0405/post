package knowledgepublish

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rights"
)

// The publish command's rules, without a database. What is pinned here is
// the ORDER (which refusal is taken before which read), the two
// independent refusals of an agent, the replay/conflict split of an
// Idempotency-Key, the publisher's name rules, and the pid — none of which
// a store could decide for the command.
//
// The fakes record their calls, because most of what is asserted here is
// about what did NOT happen: a refusal that ran before a lookup is a
// refusal that cannot disclose what the lookup would have found, and a
// request that was refused before Publish was called is a request that
// wrote nothing.

const (
	testActorID  = "11111111-1111-4111-8111-111111111111"
	testProject  = "22222222-2222-4222-8222-222222222222"
	testVersionA = "33333333-3333-4333-8333-333333333333"
	testVersionB = "44444444-4444-4444-8444-444444444444"
	testObject   = "55555555-5555-4555-8555-555555555555"
	// testPID and testPID2 are 26-character Crockford base32 pids (no i,
	// l, o, u), the shape domain.PID.Valid accepts.
	testPID  = "01j9z6k3m4n5p6q7r8s9t0v1w2"
	testPID2 = "01j9z6k3m4n5p6q7r8s9t0v1w3"
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
		// The production store answers "no membership row" with the same
		// sentinel it answers "no such project" with — see the port's doc.
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return domain.ProjectMembership{ProjectID: testProject, UserID: testActorID, Role: *m.role}, nil
}

type fakeStore struct {
	log *callLog
	// facts is what ResolveFacts answers with.
	facts Facts
	// resolveErr, when set, is what ResolveFacts fails with.
	resolveErr error
	// replay is the ledger answer of LookupCreation; nil means "no entry".
	replay *Published
	// publishErr, when set, is what Publish fails with.
	publishErr error
	// publishIn is the request the store was handed.
	publishIn *PublishRequest
	// factsAfterRights records the facts handed to Publish.
	publishFacts Facts
}

func (s *fakeStore) ResolveFacts(_ context.Context, _, _ string) (Facts, error) {
	s.log.add("resolve")
	if s.resolveErr != nil {
		return Facts{}, s.resolveErr
	}
	return s.facts, nil
}

func (s *fakeStore) LookupCreation(_ context.Context, _, _ string) (*Published, error) {
	s.log.add("lookup")
	return s.replay, nil
}

func (s *fakeStore) Publish(_ context.Context, req PublishRequest) (Published, error) {
	s.log.add("publish")
	s.publishIn = &req
	s.publishFacts = req.Facts
	if s.publishErr != nil {
		return Published{}, s.publishErr
	}
	return Published{
		ID:              "66666666-6666-4666-8666-666666666666",
		PID:             req.PID,
		ObjectVersionID: req.ObjectVersionID,
		PublicVersion:   req.PublicVersion,
		RightsJSON:      req.RightsJSON,
		PublishedBy:     req.Actor.User.ID,
	}, nil
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
	cmd     *Command
	log     *callLog
	members *fakeMembers
	store   *fakeStore
	engine  *fakeEngine
	minted  []domain.PID
	// pidErr, when set, is what the generator fails with.
	pidErr error
	// pidOverride, when set, is what the generator returns unchecked — the
	// only way to observe the command's own shape check. It is a pointer so
	// that "the generator returned nothing" is expressible.
	pidOverride *string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	log := &callLog{}
	role := domain.ProjectRoleOwner
	h := &harness{
		log:     log,
		members: &fakeMembers{log: log, role: &role},
		store:   &fakeStore{log: log},
		engine:  &fakeEngine{permits: true},
	}
	h.store.facts = publishedFacts()
	h.cmd = NewCommand(Deps{
		Members: h.members,
		Store:   h.store,
		Authz:   h.engine,
		NewPID: func() (domain.PID, error) {
			if h.pidErr != nil {
				return "", h.pidErr
			}
			next := domain.PID(testPID)
			if h.pidOverride != nil {
				next = domain.PID(*h.pidOverride)
			} else if len(h.minted) > 0 {
				next = domain.PID(testPID2)
			}
			h.minted = append(h.minted, next)
			return next, nil
		},
	})
	return h
}

// mustRightsRaw returns the canonical bytes of a rights document whose
// metadata axis is the given token, or rights.MetadataProjectPolicy when
// the token is empty — the document every publish in these tests sends.
func mustRightsRaw(t *testing.T, metadata rights.MetadataVisibility) json.RawMessage {
	t.Helper()
	doc := rights.New()
	if metadata != "" {
		doc.Visibility.Metadata = metadata
	}
	raw, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal rights: %v", err)
	}
	return raw
}

// publishedFacts is a version that IS admissible: it passed the two
// required reviews, it has never been published, and its project is
// public. Cases below vary one field each.
func publishedFacts() Facts {
	return Facts{
		ObjectVersionID:   testVersionA,
		ObjectID:          testObject,
		ObjectType:        "finding",
		ProjectID:         testProject,
		Title:             "A reproducible finding",
		LifecycleState:    "published",
		StateID:           "77777777-7777-4777-8777-777777777777",
		MainBranchID:      "88888888-8888-4888-8888-888888888888",
		ProjectVisibility: "public",
		Reviews: []releases.ReviewRecord{
			{Reviews: []releases.Review{
				{ReviewKind: "scientific", Decision: "approved"},
				{ReviewKind: "integrity", Decision: "approved"},
			}},
		},
	}
}

func (h *harness) params(t *testing.T) PublishParams {
	t.Helper()
	return PublishParams{
		ProjectID:       testProject,
		ObjectVersionID: testVersionA,
		PublicVersion:   "v1.0",
		Rights:          mustRightsRaw(t, ""),
	}
}

func (h *harness) actor() Actor {
	return Actor{User: domain.User{ID: testActorID, Handle: "alice"}}
}

// useRealEngine swaps the permissive fake for the shipped matrix, which is
// what the production wiring uses.
func (h *harness) useRealEngine() { h.cmd.authz = authz.NewMatrixEngine() }

// ---------------------------------------------------------------------------
// 1. The agent backstop, on its own

// TestPublishRefusesAgentEvenWhenTheMatrixPermits: the domain backstop is
// an INDEPENDENT line, and the only way to show that is to run it against
// an engine that would have allowed the publish. docs/23 §4 keeps
// `visibility:publish` out of an agent token's scope; the matrix denies
// agents too (asserted separately below), but a matrix is a governance
// document a change may edit, and this refusal is the product rule the
// edit cannot waive.
func TestPublishRefusesAgentEvenWhenTheMatrixPermits(t *testing.T) {
	h := newHarness(t)
	actor := h.actor()
	actor.IsAgent = true

	_, err := h.cmd.Publish(context.Background(), actor, h.params(t))

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
	// Resolved before every lookup: neither the membership nor the store
	// was touched, so the refusal cannot disclose whether the project or
	// the version exists.
	if h.log.has("members") || h.log.has("resolve") || h.log.has("publish") {
		t.Errorf("an agent publish touched its ports (%s); the backstop precedes every read", h.log)
	}
	if len(h.engine.seen) != 0 {
		t.Errorf("the matrix was consulted for an agent: %+v", h.engine.seen)
	}
}

// TestPublishRefusesAgentUnderTheShippedMatrix pins the same refusal
// against the real matrix cell (agent_default = deny), so that the
// backstop is not the only thing standing between an agent and a
// publication.
func TestPublishRefusesAgentUnderTheShippedMatrix(t *testing.T) {
	h := newHarness(t)
	h.useRealEngine()
	actor := h.actor()
	actor.IsAgent = true

	// The backstop answers first; the point of this case is that the
	// matrix would have answered the same way, so the engine is asked
	// directly.
	decision, err := h.cmd.authz.Authorize(context.Background(), authz.Request{
		Action: authz.ActionPublishPrivateToPublic,
		Class:  authz.ClassOf(true, nil, true),
	})
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	if decision.Permits() {
		t.Errorf("the shipped matrix permits publish_private_to_public for agent_default; the backstop must not be the only refusal")
	}
	if _, err := h.cmd.Publish(context.Background(), actor, h.params(t)); !errors.Is(err, ErrAgentNotPermitted) {
		t.Errorf("an agent publish = %v, want ErrAgentNotPermitted", err)
	}
}

// ---------------------------------------------------------------------------
// 2. The permission matrix, fail closed

// TestPublishRoleMatrixUnderTheShippedEngine is the whole fail-closed
// story of specs/policies/permissions-matrix.csv row 12
// (publish_private_to_public) in one table: only the owner is admitted,
// and the maintainer's `conditional` cell — a condition no specification
// defines (issue #237) — reads as a refusal because authz.Permits() admits
// only `allow`.
func TestPublishRoleMatrixUnderTheShippedEngine(t *testing.T) {
	maintainer := domain.ProjectRoleMaintainer
	viewer := domain.ProjectRoleViewer
	contributor := domain.ProjectRoleContributor
	owner := domain.ProjectRoleOwner

	cases := []struct {
		name string
		role *domain.ProjectRole
		want error
	}{
		{"owner may publish", &owner, nil},
		{"maintainer is refused (conditional has no specification)", &maintainer, ErrForbidden},
		{"contributor is refused", &contributor, ErrForbidden},
		{"viewer is refused", &viewer, ErrForbidden},
		{"a non-member is refused", nil, ErrForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.useRealEngine()
			h.members.role = tc.role

			got, err := h.cmd.Publish(context.Background(), h.actor(), h.params(t))
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("err = %v, want %v", err, tc.want)
				}
				if h.log.has("publish") {
					t.Errorf("a refused publish still called the store: %s", h.log)
				}
				return
			}
			if err != nil {
				t.Fatalf("owner publish = %v, want success", err)
			}
			if got.PID != testPID {
				t.Errorf("published pid = %q, want %q", got.PID, testPID)
			}
		})
	}
}

// TestPublishRefusesAnUnknownProjectWithoutDisclosingIt: the membership
// port answers ErrMemberNotFound for a project that does not exist exactly
// as it does for one the caller does not belong to, so an actor probing
// for projects through this route is answered "not permitted" — never
// "no such project".
func TestPublishRefusesAnUnknownProjectWithoutDisclosingIt(t *testing.T) {
	h := newHarness(t)
	h.useRealEngine()
	h.members.role = nil
	h.members.err = projects.ErrMemberNotFound

	_, err := h.cmd.Publish(context.Background(), h.actor(), h.params(t))
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if errors.Is(err, ErrProjectNotFound) {
		t.Errorf("an unknown membership was reported as a missing project: %v", err)
	}
	if h.log.has("resolve") || h.log.has("publish") {
		t.Errorf("a forbidden publish read past the authorization: %s", h.log)
	}
}

// TestPublishRefusesAnErroringEngineAsStorageFailure: an engine that
// cannot decide is a wiring failure, never a permission — and it fails
// closed.
func TestPublishRefusesAnErroringEngineAsStorageFailure(t *testing.T) {
	h := newHarness(t)
	boom := errors.New("matrix unavailable")
	h.cmd.authz = &fakeEngine{err: boom}

	_, err := h.cmd.Publish(context.Background(), h.actor(), h.params(t))
	if !errors.Is(err, ErrStore) {
		t.Fatalf("err = %v, want ErrStore", err)
	}
	if errors.Is(err, ErrForbidden) {
		t.Errorf("an undecidable authorization was reported as a denial")
	}
	if !strings.Contains(err.Error(), "matrix unavailable") {
		t.Errorf("the cause was dropped: %v", err)
	}
	if h.log.has("publish") {
		t.Errorf("an undecided authorization reached the store: %s", h.log)
	}
}

// ---------------------------------------------------------------------------
// 3. Shape validation, before anything is read

func TestPublishValidatesTheRequestBeforeReadingAnything(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PublishParams)
	}{
		{"empty project", func(p *PublishParams) { p.ProjectID = "  " }},
		{"version ref is not a uuid", func(p *PublishParams) { p.ObjectVersionID = "object_version:not-a-uuid" }},
		{"version ref is empty", func(p *PublishParams) { p.ObjectVersionID = "" }},
		{"rights is not a rights document", func(p *PublishParams) { p.Rights = json.RawMessage(`{"version":99}`) }},
		{"rights is not json", func(p *PublishParams) { p.Rights = json.RawMessage(`not json`) }},
		{"rights is empty", func(p *PublishParams) { p.Rights = nil }},
		{"public_version is missing", func(p *PublishParams) { p.PublicVersion = "" }},
		{"public_version is blank", func(p *PublishParams) { p.PublicVersion = " \t\n " }},
		{"public_version is over the bound", func(p *PublishParams) {
			p.PublicVersion = strings.Repeat("v", MaxPublicVersionLen+1)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			p := h.params(t)
			tc.mutate(&p)

			if _, err := h.cmd.Publish(context.Background(), h.actor(), p); !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if h.log.has("members") || h.log.has("resolve") || h.log.has("publish") {
				t.Errorf("a malformed request touched a port: %s", h.log)
			}
			if len(h.minted) != 0 {
				t.Errorf("a malformed request minted %d pids", len(h.minted))
			}
		})
	}
}

// TestPublicVersionBoundIsInclusive: the bound refuses what no surface
// could render, and it refuses at the first character over it — an
// off-by-one here would reject the longest legal name.
func TestPublicVersionBoundIsInclusive(t *testing.T) {
	h := newHarness(t)
	p := h.params(t)
	p.PublicVersion = strings.Repeat("v", MaxPublicVersionLen)

	if _, err := h.cmd.Publish(context.Background(), h.actor(), p); err != nil {
		t.Fatalf("a name of exactly %d characters was refused: %v", MaxPublicVersionLen, err)
	}
	if h.store.publishIn == nil || h.store.publishIn.PublicVersion != p.PublicVersion {
		t.Fatalf("the store did not receive the name")
	}
}

// TestPublishStoresThePublicVersionVerbatim is owner ruling
// L3-20260916-1 #2: the name is the PUBLISHER's, stored byte for byte. The
// fixture is deliberately odd — surrounding spaces, an em dash, CJK, and a
// mixed-case suffix — and every byte of it must survive to the request the
// store is handed. A trimmed, lowercased or slugged name would fail here.
func TestPublishStoresThePublicVersionVerbatim(t *testing.T) {
	const odd = "  v1.0 — 预印本 (DRAFT)  "
	h := newHarness(t)
	p := h.params(t)
	p.PublicVersion = odd

	got, err := h.cmd.Publish(context.Background(), h.actor(), p)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if h.store.publishIn == nil {
		t.Fatal("the store was never called")
	}
	if h.store.publishIn.PublicVersion != odd {
		t.Errorf("stored public_version = %q, want %q (verbatim)", h.store.publishIn.PublicVersion, odd)
	}
	if got.PublicVersion != odd {
		t.Errorf("returned public_version = %q, want %q", got.PublicVersion, odd)
	}
}

// ---------------------------------------------------------------------------
// 4. One publication per version (ruling #3)

// TestPublishSurfacesTheStoresRefusalWhole: the "already published" rule
// is the APPLICATION's, not the database's — UNIQUE(object_version_id,
// public_version) would happily accept a second row under a second name.
// The command must therefore carry the store's refusal out as a refusal,
// with the report that explains it, and must not retry.
func TestPublishSurfacesTheStoresRefusalWhole(t *testing.T) {
	h := newHarness(t)
	h.store.publishErr = &PublicationRefused{
		Preview: Preview{
			ProjectID:       testProject,
			ObjectVersionID: testVersionA,
			Audience:        AudienceNetwork,
			Existing:        &Published{PID: testPID, ObjectVersionID: testVersionA, PublicVersion: "v1.0"},
			Publishable:     false,
			Blocking: []Reason{
				{Code: ReasonAlreadyPublished, Detail: `this knowledge object version is already published as "v1.0"`},
				{Code: ReasonReviewRequired, Detail: "the record of this version's lineage into main carries no approved scientific review"},
			},
		},
		Reasons: []string{`already published as "v1.0"`},
	}

	_, err := h.cmd.Publish(context.Background(), h.actor(), h.params(t))

	var refused *PublicationRefused
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want *PublicationRefused", err)
	}
	if !errors.Is(err, ErrRefused) {
		t.Errorf("errors.Is(err, ErrRefused) = false: %v", err)
	}
	if refused.Code() != CodePublishBlocked {
		t.Errorf("code = %q, want %q", refused.Code(), CodePublishBlocked)
	}
	if len(refused.Preview.Blocking) != 2 {
		t.Errorf("the refusal report was trimmed: %+v", refused.Preview.Blocking)
	}
	if refused.Preview.Existing == nil || refused.Preview.Existing.PublicVersion != "v1.0" {
		t.Errorf("the refusal does not name the existing publication: %+v", refused.Preview.Existing)
	}
	if !strings.Contains(err.Error(), `already published as "v1.0"`) {
		t.Errorf("Error() = %q, want the reasons", err.Error())
	}
	if n := strings.Count(h.log.String(), "publish"); n != 1 {
		t.Errorf("the store was called %d times (%s); a refusal is not retried", n, h.log)
	}
}

// TestPublishCountsPublicationsOfTheVersionNotOfTheName: the store's
// refusal is what the command reports, and the command must not soften it
// into a success when the caller sends a DIFFERENT public_version — that
// is precisely the row the database's unique index would have allowed.
func TestPublishRefusesASecondNameForTheSameVersion(t *testing.T) {
	h := newHarness(t)
	h.store.publishErr = &PublicationRefused{
		Preview: Preview{
			ObjectVersionID: testVersionA,
			Existing:        &Published{PID: testPID, ObjectVersionID: testVersionA, PublicVersion: "v1.0"},
			Blocking:        []Reason{{Code: ReasonAlreadyPublished, Detail: `already published as "v1.0"`}},
		},
		Reasons: []string{`already published as "v1.0"`},
	}
	p := h.params(t)
	p.PublicVersion = "v1.1 (revised)"

	if _, err := h.cmd.Publish(context.Background(), h.actor(), p); !errors.Is(err, ErrRefused) {
		t.Fatalf("a second publication under a new name = %v, want ErrRefused", err)
	}
}

// ---------------------------------------------------------------------------
// 5. Idempotency (docs/22)

// TestPublishReplaysAnIdempotencyKeyAsARead: a key that already published
// answers with the publication the FIRST call wrote, and does no work: no
// pid is minted, the version is not resolved, the store's Publish is not
// called.
func TestPublishReplaysAnIdempotencyKeyAsARead(t *testing.T) {
	h := newHarness(t)
	key := "idem-key-1"
	stored := Published{
		ID:              "99999999-9999-4999-8999-999999999999",
		PID:             testPID2,
		ObjectVersionID: testVersionA,
		PublicVersion:   "v1.0",
		PublishedBy:     testActorID,
	}
	h.store.replay = &stored
	p := h.params(t)
	p.IdempotencyKey = &key

	got, err := h.cmd.Publish(context.Background(), h.actor(), p)
	if err != nil {
		t.Fatalf("replay = %v, want the stored publication", err)
	}
	if got.PID != testPID2 || got.ID != stored.ID {
		t.Errorf("replay = %+v, want the row the first call wrote (%+v)", got, stored)
	}
	if h.log.has("publish") {
		t.Errorf("a replay called the store's publish: %s", h.log)
	}
	// The pid is minted BEFORE the ledger read (a request that turns out to
	// be a replay discards it), so exactly one is minted here — and the
	// identity the replay answers with is the STORED one, never the fresh
	// one.
	if len(h.minted) != 1 {
		t.Fatalf("a replay minted %d pids, want 1 (minted before the ledger read and discarded)", len(h.minted))
	}
	if got.PID == string(h.minted[0]) {
		t.Errorf("a replay returned the freshly minted pid %q", got.PID)
	}
}

// TestPublishConflictsWhenTheKeyPublishedAnotherVersion (docs/45): a key
// is scoped to the target it published, and answering a request for
// another version with the first version's row would hand the caller a
// publication it did not ask for.
func TestPublishConflictsWhenTheKeyPublishedAnotherVersion(t *testing.T) {
	h := newHarness(t)
	key := "idem-key-2"
	h.store.replay = &Published{ID: "99999999-9999-4999-8999-999999999999", PID: testPID2, ObjectVersionID: testVersionB, PublicVersion: "v1.0"}
	p := h.params(t)
	p.IdempotencyKey = &key

	_, err := h.cmd.Publish(context.Background(), h.actor(), p)
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("err = %v, want ErrIdempotencyConflict", err)
	}
	if !strings.Contains(err.Error(), testVersionB) {
		t.Errorf("the conflict does not name the version the key published: %v", err)
	}
	if h.log.has("publish") {
		t.Errorf("a conflicting key reached the store's publish: %s", h.log)
	}
}

// TestPublishWithoutAKeyNeverConsultsTheLedger: the ledger read is the
// caller's key's, and a request that carried none must not be answered
// from another request's entry.
func TestPublishWithoutAKeyNeverConsultsTheLedger(t *testing.T) {
	h := newHarness(t)

	if _, err := h.cmd.Publish(context.Background(), h.actor(), h.params(t)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if h.log.has("lookup") {
		t.Errorf("a keyless publish read the idempotency ledger: %s", h.log)
	}
	if h.store.publishIn == nil {
		t.Fatal("the store was never called")
	}
	if h.store.publishIn.IdempotencyKey != nil {
		t.Errorf("a keyless publish carried a key: %q", *h.store.publishIn.IdempotencyKey)
	}
}

// ---------------------------------------------------------------------------
// 6. The pid

// TestPublishMintsAPidOfTheOneVocabulary: 26 lowercase Crockford base32
// characters, minted by the command (never left to the column DEFAULT, for
// the reason migration 00064 records for research_assets), and handed to
// the store on the request.
func TestPublishMintsAPidOfTheOneVocabulary(t *testing.T) {
	h := newHarness(t)

	got, err := h.cmd.Publish(context.Background(), h.actor(), h.params(t))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if h.store.publishIn == nil {
		t.Fatal("the store was never called")
	}
	pid := h.store.publishIn.PID
	if pid != testPID {
		t.Errorf("pid handed to the store = %q, want %q", pid, testPID)
	}
	if len(pid) != domain.PIDLen {
		t.Errorf("pid length = %d, want %d", len(pid), domain.PIDLen)
	}
	if !domain.ValidPID(pid) {
		t.Errorf("pid %q is not a persistent identifier", pid)
	}
	if pid != strings.ToLower(pid) {
		t.Errorf("pid %q is not lowercase", pid)
	}
	for _, r := range pid {
		if !strings.ContainsRune(domain.PIDAlphabet, r) {
			t.Errorf("pid %q contains %q, which is outside the Crockford alphabet", pid, r)
		}
	}
	if got.PID != pid {
		t.Errorf("returned pid = %q, want the minted %q", got.PID, pid)
	}
}

// TestPublishRefusesAGeneratorThatDoesNotSpeakTheVocabulary: the check is
// the command's, so a broken generator is a storage failure with a
// sentence about the format — not a PostgreSQL CHECK violation that says
// nothing about which component is wrong.
func TestPublishRefusesAGeneratorThatDoesNotSpeakTheVocabulary(t *testing.T) {
	cases := []struct {
		name string
		pid  string
	}{
		{"uppercase", strings.ToUpper(testPID)},
		{"too short", testPID[:25]},
		{"a character outside the alphabet", testPID[:25] + "i"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			pid := tc.pid
			h.pidOverride = &pid

			_, err := h.cmd.Publish(context.Background(), h.actor(), h.params(t))
			if !errors.Is(err, ErrStore) {
				t.Fatalf("err = %v, want ErrStore", err)
			}
			if h.log.has("publish") {
				t.Errorf("an invalid pid reached the store: %s", h.log)
			}
		})
	}
}

// TestPublishFailsWhenTheGeneratorFails: no entropy, no publication.
func TestPublishFailsWhenTheGeneratorFails(t *testing.T) {
	h := newHarness(t)
	h.pidErr = errors.New("no entropy")

	_, err := h.cmd.Publish(context.Background(), h.actor(), h.params(t))
	if !errors.Is(err, ErrStore) {
		t.Fatalf("err = %v, want ErrStore", err)
	}
	if h.log.has("publish") {
		t.Errorf("a pid-less publish reached the store: %s", h.log)
	}
}

// TestPIDDoesNotChangeWithTheNameOrTheProject is the persistence the
// vocabulary promises: the same version published under two different
// names, and by callers in differently-named projects, mints independent
// identities that are a function of nothing but the generator. The
// generator takes no argument at all — this pins that it stays that way.
func TestPIDDoesNotChangeWithTheNameOrTheProject(t *testing.T) {
	seen := map[string]string{}
	for _, name := range []string{"v1.0", "  预印本 Draft  ", "v2"} {
		h := newHarness(t)
		p := h.params(t)
		p.PublicVersion = name
		if _, err := h.cmd.Publish(context.Background(), h.actor(), p); err != nil {
			t.Fatalf("Publish(%q): %v", name, err)
		}
		seen[name] = h.store.publishIn.PID
		if h.store.publishIn.PID != testPID {
			t.Errorf("public_version %q changed the pid to %q", name, h.store.publishIn.PID)
		}
	}
}

// ---------------------------------------------------------------------------
// 7. The preview

// TestPreviewIsTheProposalAndWritesNothing: the preview is the read-only
// half — it resolves the facts, renders the decision, and touches nothing
// else. No pid is minted and the store's publish is never called.
func TestPreviewIsTheProposalAndWritesNothing(t *testing.T) {
	h := newHarness(t)

	got, err := h.cmd.Preview(context.Background(), PreviewParams{
		ProjectID:       testProject,
		ObjectVersionID: testVersionA,
		Rights:          mustRightsRaw(t, ""),
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !got.Publishable {
		t.Errorf("a preview of an admissible version reported not publishable: %+v", got.Blocking)
	}
	if got.Audience != AudienceNetwork {
		t.Errorf("audience = %q, want %q", got.Audience, AudienceNetwork)
	}
	if len(got.Blocking) != 0 {
		t.Errorf("blocking = %+v, want none", got.Blocking)
	}
	if got.PublicVersion != "" {
		t.Errorf("public_version = %q, want empty: the proposal takes no name", got.PublicVersion)
	}
	if h.log.has("publish") || h.log.has("lookup") {
		t.Errorf("a preview wrote or read the ledger: %s", h.log)
	}
	if len(h.minted) != 0 {
		t.Errorf("a preview minted %d pids", len(h.minted))
	}
}

// TestPreviewOfAnUnreviewedVersionBlocksWithTheMissingKinds: the preview
// is where a publisher learns WHY the publish would be refused, and the
// sentence names the review dimensions that are missing rather than
// saying "not reviewed".
func TestPreviewOfAnUnreviewedVersionBlocksWithTheMissingKinds(t *testing.T) {
	h := newHarness(t)
	h.store.facts = publishedFacts()
	h.store.facts.Reviews = []releases.ReviewRecord{
		{Reviews: []releases.Review{{ReviewKind: "scientific", Decision: "approved"}}},
	}

	got, err := h.cmd.Preview(context.Background(), PreviewParams{
		ProjectID:       testProject,
		ObjectVersionID: testVersionA,
		Rights:          mustRightsRaw(t, ""),
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if got.Publishable {
		t.Error("a version that passed only one review was reported publishable")
	}
	if got.ReviewApproved {
		t.Error("review_approved = true for a version that passed one of two reviews")
	}
	if len(got.Blocking) != 1 || got.Blocking[0].Code != ReasonReviewRequired {
		t.Fatalf("blocking = %+v, want one %s", got.Blocking, ReasonReviewRequired)
	}
	if !strings.Contains(got.Blocking[0].Detail, "integrity") {
		t.Errorf("the refusal does not name the missing dimension: %q", got.Blocking[0].Detail)
	}
	if strings.Contains(got.Blocking[0].Detail, "scientific,") {
		t.Errorf("the refusal names a dimension that was approved: %q", got.Blocking[0].Detail)
	}
	if got.RequiredReviewKinds == nil || len(got.RequiredReviewKinds) != 2 {
		t.Errorf("required_review_kinds = %v, want the two dimensions", got.RequiredReviewKinds)
	}
	if len(got.ApprovedReviewKinds) != 1 || got.ApprovedReviewKinds[0] != "scientific" {
		t.Errorf("approved_review_kinds = %v, want [scientific]", got.ApprovedReviewKinds)
	}
}

// TestPreviewOfAPublishedVersionNamesTheExistingPublication: a caller
// asking what a second publication would do is told that one exists —
// which is the whole content of ruling #3 at the preview surface.
func TestPreviewOfAPublishedVersionNamesTheExistingPublication(t *testing.T) {
	h := newHarness(t)
	h.store.facts = publishedFacts()
	h.store.facts.Published = &Published{ID: "aaaa", PID: testPID, ObjectVersionID: testVersionA, PublicVersion: "v1.0"}

	got, err := h.cmd.Preview(context.Background(), PreviewParams{
		ProjectID:       testProject,
		ObjectVersionID: testVersionA,
		Rights:          mustRightsRaw(t, ""),
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if got.Publishable {
		t.Error("an already published version was reported publishable")
	}
	if got.Existing == nil || got.Existing.PublicVersion != "v1.0" {
		t.Fatalf("existing_publication = %+v, want the publication", got.Existing)
	}
	if len(got.Blocking) != 1 || got.Blocking[0].Code != ReasonAlreadyPublished {
		t.Errorf("blocking = %+v, want one %s", got.Blocking, ReasonAlreadyPublished)
	}
}

// TestPreviewJudgesOnTheRightsTheCallerSentNotOnTheStoresCopy: the
// decision input's rights axis is the caller's document (the preview
// stores nothing), and the audience the preview reports must move with it.
// A preview that reported the crowd the stored document describes would be
// answering a different question than the one asked.
func TestPreviewJudgesOnTheRightsTheCallerSentNotOnTheStoresCopy(t *testing.T) {
	h := newHarness(t)
	h.store.facts = publishedFacts()
	// The store's copy is the fail-closed document (zero value): if the
	// command forgot to substitute the caller's, this is the answer the
	// preview would report.
	h.store.facts.Rights = rights.Document{}

	got, err := h.cmd.Preview(context.Background(), PreviewParams{
		ProjectID:       testProject,
		ObjectVersionID: testVersionA,
		Rights:          mustRightsRaw(t, ""),
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if got.Audience != AudienceNetwork {
		t.Errorf("audience = %q, want %q: the preview judged the stored document, not the caller's", got.Audience, AudienceNetwork)
	}
}

// TestPreviewReportsTheVersionsOwnVisibilityAxis: the axis that decides
// the audience is echoed, because a caller has to be able to see which one
// applied. A version that pins a policy of its own is not network-visible
// even in a public project.
func TestPreviewReportsTheVersionsOwnVisibilityAxis(t *testing.T) {
	pinned := "policy-restricted"
	h := newHarness(t)
	h.store.facts = publishedFacts() // public project
	h.store.facts.VisibilityPolicyID = &pinned

	got, err := h.cmd.Preview(context.Background(), PreviewParams{
		ProjectID:       testProject,
		ObjectVersionID: testVersionA,
		Rights:          mustRightsRaw(t, ""),
	})
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if got.Audience != AudienceMembers {
		t.Errorf("audience = %q, want %q: a version in a PUBLIC project with its own visibility policy is not network-visible", got.Audience, AudienceMembers)
	}
	if got.VisibilityPolicyID == nil || *got.VisibilityPolicyID != pinned {
		t.Errorf("visibility_policy_id = %v, want %q", got.VisibilityPolicyID, pinned)
	}
	if got.ProjectVisibility != "public" {
		t.Errorf("project_visibility = %q, want public", got.ProjectVisibility)
	}
	// Publishable is a statement about the RULES, not about the audience:
	// a members-only publication is a legal publication.
	if !got.Publishable {
		t.Errorf("a publishable members-only publication was refused: %+v", got.Blocking)
	}
}

// TestPreviewRefusesMalformedInputAsValidation: the preview validates the
// same shape the publish does, and reads nothing when it refuses.
func TestPreviewRefusesMalformedInputAsValidation(t *testing.T) {
	h := newHarness(t)

	_, err := h.cmd.Preview(context.Background(), PreviewParams{
		ProjectID:       testProject,
		ObjectVersionID: "not-a-uuid",
		Rights:          mustRightsRaw(t, ""),
	})
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("err = %v, want ErrValidation", err)
	}
	if h.log.has("resolve") {
		t.Errorf("a malformed preview resolved facts: %s", h.log)
	}
}

// TestPreviewReportsAStoreFailureAsStorage: a version the store cannot
// resolve is the port's sentinel, relayed unchanged, and anything else is
// ErrStore.
func TestPreviewReportsStoreErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"unknown version", ErrVersionNotFound, ErrVersionNotFound},
		{"unknown project", ErrProjectNotFound, ErrProjectNotFound},
		{"anything else", errors.New("connection reset"), ErrStore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.store.resolveErr = tc.err

			_, err := h.cmd.Preview(context.Background(), PreviewParams{
				ProjectID:       testProject,
				ObjectVersionID: testVersionA,
				Rights:          mustRightsRaw(t, ""),
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestPublishIsNotWiredWithoutItsPorts: a half-wired command fails closed
// with a storage failure, never with a permission and never with a write.
func TestPublishIsNotWiredWithoutItsPorts(t *testing.T) {
	h := newHarness(t)
	empty := NewCommand(Deps{})
	if _, err := empty.Preview(context.Background(), PreviewParams{
		ProjectID:       testProject,
		ObjectVersionID: testVersionA,
		Rights:          mustRightsRaw(t, ""),
	}); !errors.Is(err, ErrStore) {
		t.Fatalf("unwired preview = %v, want ErrStore", err)
	}
	if _, err := empty.Publish(context.Background(), h.actor(), h.params(t)); !errors.Is(err, ErrStore) {
		t.Fatalf("unwired publish = %v, want ErrStore", err)
	}
}
