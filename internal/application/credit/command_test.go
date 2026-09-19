package credit

import (
	"context"
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/domain"
)

// The unit half of T0809's tests: no database, no HTTP. What this file is
// for is the ORDER the command resolves things in and the AUTHORIZATION it
// derives, both of which are properties of this package alone —
//   - the agent backstop and the shape check precede every read, so a
//     refused request never touches a port;
//   - the authorization precedes the store's write, so a denial never
//     reaches a lookup;
//   - the `conditional` cell of the re-used matrix row is resolved per
//     operation against a least role, and every other verdict refuses.
//
// The store and the membership port are stubs that RECORD what was asked of
// them: "nothing was read" is asserted by inspecting the recording, not by
// trusting an ordering comment.

const (
	testProject = "11111111-1111-4111-8111-111111111111"
	testUser    = "22222222-2222-4222-8222-222222222222"
	testParty   = "33333333-3333-4333-8333-333333333333"
	testAsset   = "0123456789abcdefghjkmnpqrs"
)

// recorderStore is a StorePort that answers with a fixed value and records
// every call, so a test can assert that a refusal did not reach it.
type recorderStore struct {
	calls    []string
	declare  contribution.CreditAttribution
	dispute  contribution.CreditDispute
	declErr  error
	openErr  error
	closeErr error
}

func (s *recorderStore) DeclareAttribution(_ context.Context, _ DeclareRequest) (contribution.CreditAttribution, error) {
	s.calls = append(s.calls, "declare")
	return s.declare, s.declErr
}

func (s *recorderStore) OpenDispute(_ context.Context, _ OpenDisputeRequest) (contribution.CreditDispute, error) {
	s.calls = append(s.calls, "open")
	return s.dispute, s.openErr
}

func (s *recorderStore) CloseDispute(_ context.Context, _ CloseDisputeRequest) (contribution.CreditDispute, error) {
	s.calls = append(s.calls, "close")
	return s.dispute, s.closeErr
}

// recorderMembers answers with a fixed role (or error) and records the ask.
type recorderMembers struct {
	calls []string
	role  domain.ProjectRole
	err   error
}

func (m *recorderMembers) GetMembership(_ context.Context, projectID, userID string) (domain.ProjectMembership, error) {
	m.calls = append(m.calls, projectID+"/"+userID)
	if m.err != nil {
		return domain.ProjectMembership{}, m.err
	}
	return domain.ProjectMembership{ProjectID: projectID, UserID: userID, Role: m.role}, nil
}

func newTestCommand(role domain.ProjectRole) (*Command, *recorderStore, *recorderMembers) {
	store := &recorderStore{
		declare: contribution.CreditAttribution{ID: "stmt", Ordinal: 1},
		dispute: contribution.CreditDispute{ID: "dispute", State: contribution.DisputeStateOpen},
	}
	members := &recorderMembers{role: role}
	return NewCommand(Deps{Members: members, Store: store, Authz: authz.NewMatrixEngine()}), store, members
}

func actorFor(userID string) Actor { return Actor{User: domain.User{ID: userID}} }

// declareParams is a well-formed declaration of the fixture's asset.
func declareParams() DeclareParams {
	return DeclareParams{
		ProjectID: testProject,
		TargetRef: "asset:" + testAsset,
		Parties: []PartyInput{
			{Kind: domain.PartyUser, ID: testParty, Role: contribution.CreditRoleCreator},
		},
	}
}

func openParams() OpenDisputeParams {
	return OpenDisputeParams{
		ProjectID: testProject,
		TargetRef: "asset:" + testAsset,
		Claim:     "The creator list is wrong.",
	}
}

func closeParams() CloseDisputeParams {
	return CloseDisputeParams{
		ProjectID:  testProject,
		DisputeID:  testUser,
		Outcome:    contribution.DisputeStateResolved,
		Resolution: "Corrected.",
	}
}

// TestAuthorizationIsResolvedPerOperation is the matrix reuse, spelled out:
// the same row (submit_scientific_review) answers three operations
// differently, and the conditional cell is resolved against the least role
// each operation requires — contributor to raise a dispute (docs/13 §3's
// "Contributor 可发起 dispute"), maintainer to declare a credit or decide
// one (same sentence: "Maintainer/Organization governance 处理", and the
// existing creator-writing path, which is gated on a maintainer-level
// action).
func TestAuthorizationIsResolvedPerOperation(t *testing.T) {
	cases := []struct {
		role      domain.ProjectRole
		declareOK bool
		openOK    bool
		closeOK   bool
	}{
		{domain.ProjectRoleOwner, true, true, true},
		{domain.ProjectRoleMaintainer, true, true, true},
		{domain.ProjectRoleContributor, false, true, false},
		{domain.ProjectRoleViewer, false, false, false},
	}
	for _, c := range cases {
		t.Run(string(c.role), func(t *testing.T) {
			cmd, store, members := newTestCommand(c.role)
			ctx := context.Background()

			_, declareErr := cmd.DeclareAttribution(ctx, actorFor(testUser), declareParams())
			assertAllowed(t, "declare", c.declareOK, declareErr)
			_, openErr := cmd.OpenDispute(ctx, actorFor(testUser), openParams())
			assertAllowed(t, "open", c.openOK, openErr)
			_, closeErr := cmd.CloseDispute(ctx, actorFor(testUser), closeParams())
			assertAllowed(t, "close", c.closeOK, closeErr)

			want := 0
			for _, ok := range []bool{c.declareOK, c.openOK, c.closeOK} {
				if ok {
					want++
				}
			}
			if len(store.calls) != want {
				t.Fatalf("the store was called %v, want %d call(s) (%d permitted operations)",
					store.calls, want, want)
			}
			if len(members.calls) != 3 {
				t.Fatalf("the membership port was asked %d times, want 3 — the authorization runs for every request, permitted or not",
					len(members.calls))
			}
		})
	}

	// A non-member is authenticated with no role: the row's
	// authenticated_nonmember cell is `conditional` and the condition does
	// not hold for anybody.
	t.Run("non-member", func(t *testing.T) {
		cmd, store, members := newTestCommand("")
		members.err = projects.ErrMemberNotFound
		ctx := context.Background()
		if _, err := cmd.OpenDispute(ctx, actorFor(testUser), openParams()); !errors.Is(err, ErrForbidden) {
			t.Fatalf("a non-member opening a dispute = %v, want ErrForbidden", err)
		}
		if _, err := cmd.DeclareAttribution(ctx, actorFor(testUser), declareParams()); !errors.Is(err, ErrForbidden) {
			t.Fatalf("a non-member declaring credit = %v, want ErrForbidden", err)
		}
		if len(store.calls) != 0 {
			t.Fatalf("a refusal reached the store: %v", store.calls)
		}
	})
}

// assertAllowed checks one operation's outcome against what the matrix
// says, and that a refusal is one of the two refusal sentinels rather than
// a store failure.
func assertAllowed(t *testing.T, op string, wantOK bool, err error) {
	t.Helper()
	if wantOK {
		if err != nil {
			t.Fatalf("%s = %v, want permitted", op, err)
		}
		return
	}
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("%s = %v, want ErrForbidden", op, err)
	}
}

// TestAgentBackstopPrecedesEveryRead: docs/60 §2 lists credit dispute
// resolution among the actions that must be human-governed, so an agent is
// refused from the actor value alone — before the membership port is asked
// and before the store is reached. It is a second line of defence in front
// of the matrix, so an edit to the CSV cannot waive it.
func TestAgentBackstopPrecedesEveryRead(t *testing.T) {
	cmd, store, members := newTestCommand(domain.ProjectRoleOwner)
	ctx := context.Background()
	agent := Actor{User: domain.User{ID: testUser}, IsAgent: true}

	type call struct {
		op string
		fn func() error
	}
	calls := []call{
		{"declare", func() error { _, err := cmd.DeclareAttribution(ctx, agent, declareParams()); return err }},
		{"open", func() error { _, err := cmd.OpenDispute(ctx, agent, openParams()); return err }},
		{"close", func() error { _, err := cmd.CloseDispute(ctx, agent, closeParams()); return err }},
	}
	for _, c := range calls {
		err := c.fn()
		if !errors.Is(err, ErrAgentNotPermitted) {
			t.Fatalf("an agent's %s = %v, want ErrAgentNotPermitted", c.op, err)
		}
		var typed *AgentNotPermittedError
		if !errors.As(err, &typed) || typed.Operation == "" {
			t.Fatalf("the refusal for %s does not carry the operation: %v", c.op, err)
		}
	}
	if len(members.calls) != 0 || len(store.calls) != 0 {
		t.Fatalf("the agent backstop did not precede the reads: members %v, store %v", members.calls, store.calls)
	}
}

// TestShapeIsCheckedBeforeAnyRead: a malformed request is refused without
// asking the membership port or the store anything — a command that cannot
// parse its input has nothing to look up, and a refusal must not disclose
// whether the project or the target exists.
func TestShapeIsCheckedBeforeAnyRead(t *testing.T) {
	cmd, store, members := newTestCommand(domain.ProjectRoleOwner)
	ctx := context.Background()
	who := actorFor(testUser)

	bad := []struct {
		name string
		err  error
	}{
		{"declare: no project", errOf(func() error {
			p := declareParams()
			p.ProjectID = " "
			_, e := cmd.DeclareAttribution(ctx, who, p)
			return e
		})},
		{"declare: no target", errOf(func() error {
			p := declareParams()
			p.TargetRef = "asset:"
			_, e := cmd.DeclareAttribution(ctx, who, p)
			return e
		})},
		{"declare: unknown kind", errOf(func() error {
			p := declareParams()
			p.TargetRef = "project:x"
			_, e := cmd.DeclareAttribution(ctx, who, p)
			return e
		})},
		{"declare: no parties", errOf(func() error {
			p := declareParams()
			p.Parties = nil
			_, e := cmd.DeclareAttribution(ctx, who, p)
			return e
		})},
		{"declare: unknown role", errOf(func() error {
			p := declareParams()
			p.Parties = []PartyInput{{Kind: domain.PartyUser, ID: testParty, Role: "method_designer"}}
			_, e := cmd.DeclareAttribution(ctx, who, p)
			return e
		})},
		{"declare: project as party", errOf(func() error {
			p := declareParams()
			p.Parties = []PartyInput{{Kind: domain.PartyProject, ID: testParty, Role: contribution.CreditRoleCreator}}
			_, e := cmd.DeclareAttribution(ctx, who, p)
			return e
		})},
		{"declare: party id not a uuid", errOf(func() error {
			p := declareParams()
			p.Parties = []PartyInput{{Kind: domain.PartyUser, ID: "alice", Role: contribution.CreditRoleCreator}}
			_, e := cmd.DeclareAttribution(ctx, who, p)
			return e
		})},
		{"declare: the same party twice under one role", errOf(func() error {
			p := declareParams()
			p.Parties = []PartyInput{
				{Kind: domain.PartyUser, ID: testParty, Role: contribution.CreditRoleCreator},
				{Kind: domain.PartyUser, ID: testParty, Role: contribution.CreditRoleCreator},
			}
			_, e := cmd.DeclareAttribution(ctx, who, p)
			return e
		})},
		{"open: empty claim", errOf(func() error { p := openParams(); p.Claim = "  "; _, e := cmd.OpenDispute(ctx, who, p); return e })},
		{"open: evidence that is not a ledger ref", errOf(func() error {
			p := openParams()
			p.EvidenceRefs = []string{"credit_title:invented"}
			_, e := cmd.OpenDispute(ctx, who, p)
			return e
		})},
		{"close: dispute id not a uuid", errOf(func() error {
			p := closeParams()
			p.DisputeID = "dispute"
			_, e := cmd.CloseDispute(ctx, who, p)
			return e
		})},
		{"close: outcome open", errOf(func() error {
			p := closeParams()
			p.Outcome = contribution.DisputeStateOpen
			_, e := cmd.CloseDispute(ctx, who, p)
			return e
		})},
		{"close: outcome invented", errOf(func() error {
			p := closeParams()
			p.Outcome = "closed"
			_, e := cmd.CloseDispute(ctx, who, p)
			return e
		})},
		{"close: empty resolution", errOf(func() error { p := closeParams(); p.Resolution = " "; _, e := cmd.CloseDispute(ctx, who, p); return e })},
	}
	for _, c := range bad {
		if !errors.Is(c.err, ErrValidation) {
			t.Errorf("%s = %v, want ErrValidation", c.name, c.err)
		}
	}
	if len(members.calls) != 0 || len(store.calls) != 0 {
		t.Fatalf("a malformed request was looked up: members %v, store %v", members.calls, store.calls)
	}
}

func errOf(fn func() error) error { return fn() }

// TestEngineAndWiringFailClosed: an engine error means the decision is
// unknowable, and a missing engine is not "everyone allowed" — both refuse,
// and neither reaches the store. A missing membership port or store is a
// wiring failure (ErrStore), not a permission.
func TestEngineAndWiringFailClosed(t *testing.T) {
	ctx := context.Background()

	broken := NewCommand(Deps{
		Members: &recorderMembers{role: domain.ProjectRoleOwner},
		Store:   &recorderStore{},
		Authz:   stubEngine{err: errors.New("engine down")},
	})
	if _, err := broken.OpenDispute(ctx, actorFor(testUser), openParams()); !errors.Is(err, ErrStore) {
		t.Errorf("an engine error = %v, want a wrapped ErrStore", err)
	}

	deny := NewCommand(Deps{
		Members: &recorderMembers{role: domain.ProjectRoleOwner},
		Store:   &recorderStore{},
		Authz:   stubEngine{decision: authz.Decision{Verdict: authz.VerdictDeny}},
	})
	if _, err := deny.OpenDispute(ctx, actorFor(testUser), openParams()); !errors.Is(err, ErrForbidden) {
		t.Errorf("an explicit deny = %v, want ErrForbidden", err)
	}

	// The agent column of the re-used row is proposal_or_scoped, which no
	// verdict this site resolves — an engine that answered it for a HUMAN
	// class must still refuse, because a scoped credit operation is a
	// concept nothing defines.
	scoped := NewCommand(Deps{
		Members: &recorderMembers{role: domain.ProjectRoleOwner},
		Store:   &recorderStore{},
		Authz:   stubEngine{decision: authz.Decision{Verdict: authz.VerdictProposalOrScoped}},
	})
	if _, err := scoped.OpenDispute(ctx, actorFor(testUser), openParams()); !errors.Is(err, ErrForbidden) {
		t.Errorf("a proposal_or_scoped verdict = %v, want ErrForbidden", err)
	}

	unwired := NewCommand(Deps{})
	if _, err := unwired.OpenDispute(ctx, actorFor(testUser), openParams()); !errors.Is(err, ErrStore) {
		t.Errorf("a command without ports = %v, want ErrStore", err)
	}

	// The project the membership port cannot see is relayed as its own
	// outcome: projects.ErrProjectNotFound is the deliberate disclosure the
	// project surface makes for an invisible project.
	hidden := NewCommand(Deps{
		Members: &recorderMembers{err: projects.ErrProjectNotFound},
		Store:   &recorderStore{},
		Authz:   authz.NewMatrixEngine(),
	})
	if _, err := hidden.OpenDispute(ctx, actorFor(testUser), openParams()); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("an invisible project = %v, want ErrProjectNotFound", err)
	}
}

// stubEngine answers one fixed decision (or error).
type stubEngine struct {
	decision authz.Decision
	err      error
}

func (e stubEngine) Authorize(context.Context, authz.Request) (authz.Decision, error) {
	return e.decision, e.err
}

// TestStoreFailuresAreMappedNotSwallowed: an adapter's sentinel arrives at
// the caller unchanged, and anything the package does not know becomes
// ErrStore — a transport must be able to branch on the outcome without
// parsing a message.
func TestStoreFailuresAreMappedNotSwallowed(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		in   error
		want error
	}{
		{"target not found", ErrTargetNotFound, ErrTargetNotFound},
		{"party not found", ErrPartyNotFound, ErrPartyNotFound},
		{"dispute not found", ErrDisputeNotFound, ErrDisputeNotFound},
		{"dispute closed", ErrDisputeClosed, ErrDisputeClosed},
		{"validation relayed", ErrValidation, ErrValidation},
		{"unknown failure", errors.New("connection reset"), ErrStore},
	} {
		t.Run(c.name, func(t *testing.T) {
			cmd, store, _ := newTestCommand(domain.ProjectRoleOwner)
			store.declErr = c.in
			store.openErr = c.in
			store.closeErr = c.in
			who := actorFor(testUser)
			if _, err := cmd.DeclareAttribution(ctx, who, declareParams()); !errors.Is(err, c.want) {
				t.Errorf("declare = %v, want %v", err, c.want)
			}
			if _, err := cmd.OpenDispute(ctx, who, openParams()); !errors.Is(err, c.want) {
				t.Errorf("open = %v, want %v", err, c.want)
			}
			if _, err := cmd.CloseDispute(ctx, who, closeParams()); !errors.Is(err, c.want) {
				t.Errorf("close = %v, want %v", err, c.want)
			}
		})
	}
}

// TestPositionsAreAssignedPerRoleInTheCallersOrder: a declaration is a
// list, and the order the caller sent it in is what the row stores — one
// running position per role, so two roles do not interleave each other's
// numbering.
func TestPositionsAreAssignedPerRoleInTheCallersOrder(t *testing.T) {
	cmd, store, _ := newTestCommand(domain.ProjectRoleOwner)
	var captured DeclareRequest
	store.declErr = nil
	cmd.store = &captureStore{inner: store, onDeclare: func(r DeclareRequest) { captured = r }}

	other := "44444444-4444-4444-8444-444444444444"
	_, err := cmd.DeclareAttribution(context.Background(), actorFor(testUser), DeclareParams{
		ProjectID: testProject,
		TargetRef: "asset:" + testAsset,
		Parties: []PartyInput{
			{Kind: domain.PartyUser, ID: testParty, Role: contribution.CreditRoleCreator},
			{Kind: domain.PartyUser, ID: other, Role: contribution.CreditRoleMajorContributor},
			{Kind: domain.PartyUser, ID: testUser, Role: contribution.CreditRoleCreator},
			{Kind: domain.PartyUser, ID: other, Role: contribution.CreditRoleCreator},
		},
	})
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	want := []struct {
		id       string
		role     contribution.CreditRole
		position int
	}{
		{testParty, contribution.CreditRoleCreator, 0},
		{other, contribution.CreditRoleMajorContributor, 0},
		{testUser, contribution.CreditRoleCreator, 1},
		{other, contribution.CreditRoleCreator, 2},
	}
	if len(captured.Parties) != len(want) {
		t.Fatalf("captured %d parties, want %d", len(captured.Parties), len(want))
	}
	for i, w := range want {
		got := captured.Parties[i]
		if got.Party.ID != w.id || got.Role != w.role || got.Position != w.position {
			t.Errorf("parties[%d] = %s/%s/%d, want %s/%s/%d",
				i, got.Party.ID, got.Role, got.Position, w.id, w.role, w.position)
		}
	}
	// The audit row the command renders names the actor and the target, and
	// never restates the request's content.
	if captured.Audit.Action != contribution.AuditActionCreditAttributionDeclared {
		t.Errorf("audit action = %q", captured.Audit.Action)
	}
	if captured.Audit.TargetRef != "asset:"+testAsset || captured.Audit.ProjectID != testProject {
		t.Errorf("audit scope = %q / %q", captured.Audit.TargetRef, captured.Audit.ProjectID)
	}
}

// captureStore records the request the command rendered, so a test can
// assert the shape the store would have written.
type captureStore struct {
	inner     *recorderStore
	onDeclare func(DeclareRequest)
}

func (s *captureStore) DeclareAttribution(ctx context.Context, req DeclareRequest) (contribution.CreditAttribution, error) {
	s.onDeclare(req)
	return s.inner.DeclareAttribution(ctx, req)
}

func (s *captureStore) OpenDispute(ctx context.Context, req OpenDisputeRequest) (contribution.CreditDispute, error) {
	return s.inner.OpenDispute(ctx, req)
}

func (s *captureStore) CloseDispute(ctx context.Context, req CloseDisputeRequest) (contribution.CreditDispute, error) {
	return s.inner.CloseDispute(ctx, req)
}
