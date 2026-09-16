package mainfreeze_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/mainfreeze"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// The unit surface of the freeze command: the order of its steps, the two
// independent refusals an agent meets, and the shape of each refusal. What
// this file deliberately does NOT do is exercise the freeze itself — the
// transaction that sets the flag with its audit row and its event is the
// integration surface (tests/integration/freeze_main_e2e_test.go), because
// a fake store cannot show that the three commit together.

const (
	testProject = "11111111-1111-4111-8111-111111111111"
	testActor   = "22222222-2222-4222-8222-222222222222"
	testKey     = "freeze-key-0001"
)

// fakeMembers answers a fixed membership, recording that it was asked.
// seenNobody is the "not a member or no such project" answer
// persistence.ProjectStore gives for both.
type fakeMembers struct {
	role  *domain.ProjectRole
	err   error
	calls int
}

func (f *fakeMembers) GetMembership(_ context.Context, _, _ string) (domain.ProjectMembership, error) {
	f.calls++
	if f.err != nil {
		return domain.ProjectMembership{}, f.err
	}
	if f.role == nil {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return domain.ProjectMembership{ProjectID: testProject, UserID: testActor, Role: *f.role}, nil
}

// fakePolicies answers a fixed effective policy.
type fakePolicies struct {
	effective domain.EffectivePolicy
	err       error
	calls     int
}

func (f *fakePolicies) EffectivePolicy(_ context.Context, _ domain.User, _ string) (domain.EffectivePolicy, error) {
	f.calls++
	return f.effective, f.err
}

// fakeRules answers one fixed decision for every query.
type fakeRules struct {
	decision policy.Decision
	err      error
	calls    int
}

func (f *fakeRules) Evaluate(_ context.Context, _ domain.Policy, _ policy.Query) (policy.Decision, error) {
	f.calls++
	return f.decision, f.err
}

// fakeStore records whether it was reached and answers one fixed outcome.
type fakeStore struct {
	result mainfreeze.Result
	err    error
	calls  int
	req    mainfreeze.FreezeRequest
}

func (f *fakeStore) Freeze(_ context.Context, req mainfreeze.FreezeRequest) (mainfreeze.Result, error) {
	f.calls++
	f.req = req
	return f.result, f.err
}

// harness is the wired command plus its fakes, so a test can assert not
// only what the caller saw but which steps ran on the way there.
type harness struct {
	cmd   *mainfreeze.Command
	mem   *fakeMembers
	pol   *fakePolicies
	rules *fakeRules
	store *fakeStore
}

func newHarness(t *testing.T, role *domain.ProjectRole) *harness {
	t.Helper()
	h := &harness{
		mem:   &fakeMembers{role: role},
		pol:   &fakePolicies{},
		rules: &fakeRules{decision: policy.Decision{Found: true, Bool: true}},
		store: &fakeStore{result: mainfreeze.Result{ProjectID: testProject, MainFrozen: true}},
	}
	h.cmd = mainfreeze.NewCommand(mainfreeze.Deps{
		Members:  h.mem,
		Policies: h.pol,
		Rules:    h.rules,
		Store:    h.store,
		Authz:    authz.NewMatrixEngine(),
	})
	return h
}

func (h *harness) freeze(actor mainfreeze.Actor, in mainfreeze.Input) (mainfreeze.Result, error) {
	return h.cmd.Freeze(context.Background(), actor, in)
}

func owner() mainfreeze.Actor {
	return mainfreeze.Actor{User: domain.User{ID: testActor, Handle: "owner"}}
}

func validInput() mainfreeze.Input {
	return mainfreeze.Input{ProjectID: testProject, IdempotencyKey: testKey}
}

func rolePtr(r domain.ProjectRole) *domain.ProjectRole { return &r }

// TestFreezeAdmitsOwnerAndMaintainer: the matrix's freeze_main row allows
// exactly these two classes (specs/policies/permissions-matrix.csv:10), and
// the command must reach the store for both.
func TestFreezeAdmitsOwnerAndMaintainer(t *testing.T) {
	for _, role := range []domain.ProjectRole{domain.ProjectRoleOwner, domain.ProjectRoleMaintainer} {
		t.Run(string(role), func(t *testing.T) {
			h := newHarness(t, rolePtr(role))
			res, err := h.freeze(owner(), validInput())
			if err != nil {
				t.Fatalf("freeze refused for %s: %v", role, err)
			}
			if !res.MainFrozen {
				t.Fatalf("result does not report a frozen main: %+v", res)
			}
			if h.store.calls != 1 {
				t.Fatalf("store calls = %d, want 1", h.store.calls)
			}
		})
	}
}

// TestFreezeRefusesClassesTheMatrixDenies: viewer, contributor and a
// non-member are all AUTH_FORBIDDEN, and none of them reaches the store —
// the refusal is a decision about the actor, not a failed read.
func TestFreezeRefusesClassesTheMatrixDenies(t *testing.T) {
	cases := map[string]*domain.ProjectRole{
		"viewer":      rolePtr(domain.ProjectRoleViewer),
		"contributor": rolePtr(domain.ProjectRoleContributor),
		"non-member":  nil,
	}
	for name, role := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, role)
			_, err := h.freeze(owner(), validInput())
			if !errors.Is(err, mainfreeze.ErrForbidden) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
			if h.store.calls != 0 {
				t.Fatalf("store reached %d times on a refusal", h.store.calls)
			}
			if h.pol.calls != 0 {
				t.Fatalf("policy read %d times on a refusal", h.pol.calls)
			}
		})
	}
}

// TestFreezeRefusesUnknownProjectAsPermissionOutcome is acceptance
// criterion 7 at the command's own boundary: the membership lookup answers
// ErrMemberNotFound for both "you are not a member" and "there is no such
// project", so an unknown id is refused by the SAME step, with the same
// code, as a stranger — the caller cannot tell the two apart. It also pins
// the case where the store answers the other sentinel: a project that
// disappeared still resolves to the non-member class rather than to a
// "project not found" disclosure.
func TestFreezeRefusesUnknownProjectAsPermissionOutcome(t *testing.T) {
	for _, sentinel := range []error{projects.ErrMemberNotFound, projects.ErrProjectNotFound} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			h := newHarness(t, nil)
			h.mem.err = sentinel
			_, err := h.freeze(owner(), validInput())
			if !errors.Is(err, mainfreeze.ErrForbidden) {
				t.Fatalf("err = %v, want ErrForbidden", err)
			}
			if errors.Is(err, mainfreeze.ErrProjectNotFound) {
				t.Fatalf("an unknown project was disclosed as PROJECT_NOT_FOUND: %v", err)
			}
			if h.store.calls != 0 {
				t.Fatalf("store reached %d times for an unknown project", h.store.calls)
			}
		})
	}
}

// TestFreezeRefusesAgentBeforeAnyLookup is the domain backstop, and it
// asserts BOTH halves of what makes it a second line of defence: it fires
// before the membership read and before the store, and it answers a code
// of its own (MAIN_FREEZE_AGENT_DENIED), distinct from the matrix's
// AUTH_FORBIDDEN. An agent that got through the matrix would still be
// refused here; a test that could not tell the two refusals apart could not
// show that.
func TestFreezeRefusesAgentBeforeAnyLookup(t *testing.T) {
	// The membership role is the OWNER on purpose: even an actor the matrix
	// would admit is refused as an agent, which is what "independent of
	// everything else" means.
	h := newHarness(t, rolePtr(domain.ProjectRoleOwner))
	_, err := h.freeze(
		mainfreeze.Actor{User: domain.User{ID: testActor}, IsAgent: true},
		validInput(),
	)
	if !errors.Is(err, mainfreeze.ErrAgentNotPermitted) {
		t.Fatalf("err = %v, want ErrAgentNotPermitted", err)
	}
	var refused *mainfreeze.AgentNotPermittedError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want *AgentNotPermittedError", err)
	}
	if got := refused.Code(); got != mainfreeze.CodeAgentFreezeDenied {
		t.Fatalf("code = %q, want %q", got, mainfreeze.CodeAgentFreezeDenied)
	}
	if refused.Code() == mainfreeze.CodeForbidden {
		t.Fatal("the agent backstop reports the matrix's code: the two lines of defence are indistinguishable")
	}
	if h.mem.calls != 0 {
		t.Fatalf("membership read %d times before the agent backstop", h.mem.calls)
	}
	if h.store.calls != 0 {
		t.Fatalf("store reached %d times for an agent", h.store.calls)
	}
}

// TestFreezeValidatesShapeFirst: the contract's Idempotency-Key is
// required and at least 8 characters (specs/api/openapi.yaml), and a
// request that is not a freeze request is refused before anything is read.
func TestFreezeValidatesShapeFirst(t *testing.T) {
	cases := map[string]mainfreeze.Input{
		"no key":          {ProjectID: testProject},
		"empty key":       {ProjectID: testProject, IdempotencyKey: ""},
		"short key":       {ProjectID: testProject, IdempotencyKey: "1234567"},
		"no project id":   {IdempotencyKey: testKey},
		"blank project":   {ProjectID: "   ", IdempotencyKey: testKey},
		"key of 8 is ok?": {ProjectID: testProject, IdempotencyKey: "12345678"}, // admitted — asserted below
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, rolePtr(domain.ProjectRoleOwner))
			_, err := h.freeze(owner(), in)
			admitted := name == "key of 8 is ok?"
			switch {
			case admitted && err != nil:
				t.Fatalf("a key at the contract's minimum was refused: %v", err)
			case !admitted && !errors.Is(err, mainfreeze.ErrValidation):
				t.Fatalf("err = %v, want ErrValidation", err)
			case !admitted && h.store.calls != 0:
				t.Fatalf("store reached %d times on a shape refusal", h.store.calls)
			}
		})
	}
	if got := len("12345678"); got != mainfreeze.MinIdempotencyKeyLen {
		t.Fatalf("the minLength this test covers (%d) drifted from the contract's (%d)", got, mainfreeze.MinIdempotencyKeyLen)
	}
}

// TestFreezePolicyRefusals pins the policy step docs/22 §28 requires. The
// asymmetry with the merge is deliberate and is asserted here rather than
// left in a comment: an unreadable policy, an unevaluable policy and a
// policy that DISABLES main_protected all refuse; a policy that sets it
// true, or that is silent about it, proceeds — because the freeze protects
// main, so silence is not a denial (see the command's note).
func TestFreezePolicyRefusals(t *testing.T) {
	cases := []struct {
		name     string
		readErr  error
		evalErr  error
		decision policy.Decision
		refused  bool
	}{
		{name: "main_protected true proceeds", decision: policy.Decision{Found: true, Bool: true}},
		{name: "rule absent proceeds", decision: policy.Decision{Found: false}},
		{name: "main_protected false refuses", decision: policy.Decision{Found: true, Bool: false}, refused: true},
		{name: "unreadable policy refuses", readErr: errors.New("db is down"), refused: true},
		{name: "unevaluable policy refuses", evalErr: errors.New("unknown rule"), decision: policy.Decision{Found: true, Bool: true}, refused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, rolePtr(domain.ProjectRoleOwner))
			h.pol.err = tc.readErr
			h.rules.err = tc.evalErr
			h.rules.decision = tc.decision
			_, err := h.freeze(owner(), validInput())
			if tc.refused {
				if !errors.Is(err, mainfreeze.ErrPolicyRefused) {
					t.Fatalf("err = %v, want ErrPolicyRefused", err)
				}
				var refused *mainfreeze.PolicyRefusedError
				if !errors.As(err, &refused) {
					t.Fatalf("err = %v, want *PolicyRefusedError", err)
				}
				if refused.Code() != mainfreeze.CodePolicyRefused {
					t.Fatalf("code = %q, want %q", refused.Code(), mainfreeze.CodePolicyRefused)
				}
				if h.store.calls != 0 {
					t.Fatalf("store reached %d times on a policy refusal", h.store.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("freeze refused: %v", err)
			}
			if h.store.calls != 1 {
				t.Fatalf("store calls = %d, want 1", h.store.calls)
			}
		})
	}
}

// TestFreezeRecordsTheRequestOnTheAuditRow: the row the winning freeze
// appends names the action, the project, the actor, and the transition it
// makes, and carries the Idempotency-Key of the request that caused it.
func TestFreezeRecordsTheRequestOnTheAuditRow(t *testing.T) {
	h := newHarness(t, rolePtr(domain.ProjectRoleOwner))
	in := validInput()
	in.IdempotencyKey = "freeze-key-records"
	if _, err := h.freeze(owner(), in); err != nil {
		t.Fatalf("freeze refused: %v", err)
	}
	audit := h.store.req.Audit
	if audit.Action != domain.ActionProjectMainFrozen {
		t.Fatalf("action = %q, want %q", audit.Action, domain.ActionProjectMainFrozen)
	}
	if audit.ActorID != testActor || audit.ProjectID != testProject {
		t.Fatalf("audit is not scoped to the actor and project: %+v", audit)
	}
	if audit.TargetRef != "project:"+testProject {
		t.Fatalf("target_ref = %q", audit.TargetRef)
	}
	// The summaries are `any` (jsonb), so they are read the way the store
	// will read them: as the maps the command built.
	before, ok := audit.BeforeSummary.(map[string]any)
	if !ok {
		t.Fatalf("before summary = %#v, want the state before the freeze", audit.BeforeSummary)
	}
	after, ok := audit.AfterSummary.(map[string]any)
	if !ok {
		t.Fatalf("after summary = %#v, want the state after the freeze", audit.AfterSummary)
	}
	if before["main_frozen"] != false || after["main_frozen"] != true {
		t.Fatalf("the row does not state the transition: before=%v after=%v",
			before["main_frozen"], after["main_frozen"])
	}
	meta, ok := audit.Metadata.(map[string]any)
	if !ok {
		t.Fatalf("metadata = %#v, want the request's own facts", audit.Metadata)
	}
	if meta["idempotency_key"] != "freeze-key-records" {
		t.Fatalf("the row does not carry the request's key: %v", meta)
	}
	if meta["actor_is_agent"] != false {
		t.Fatalf("the row does not record how the action arrived: %v", meta)
	}
	if h.store.req.IdempotencyKey != "freeze-key-records" {
		t.Fatalf("request key = %q", h.store.req.IdempotencyKey)
	}
}

// TestFreezeReportsAlreadyFrozenAsTheSameState: the store answers the
// zero-row compare-and-swap with the state a previous freeze produced, and
// the command passes that through instead of inventing a second outcome.
func TestFreezeReportsAlreadyFrozenAsTheSameState(t *testing.T) {
	h := newHarness(t, rolePtr(domain.ProjectRoleOwner))
	h.store.result = mainfreeze.Result{ProjectID: testProject, MainFrozen: true, AlreadyFrozen: true}
	res, err := h.freeze(owner(), validInput())
	if err != nil {
		t.Fatalf("a repeated freeze was refused: %v", err)
	}
	if !res.MainFrozen || !res.AlreadyFrozen {
		t.Fatalf("result = %+v, want a frozen project reported as already frozen", res)
	}
}

// TestFreezeMapsUnknownStoreFailuresToServiceUnavailable: a store failure
// is ErrStore with the cause kept for the log (the same shape merge, rsg
// and assetpublish use). The cause stays in the error VALUE and is kept out
// of the MESSAGE the transport prints — the two are different things, and
// the second is asserted here by rendering the one refusal whose text the
// handler does put on the wire.
func TestFreezeMapsUnknownStoreFailuresToServiceUnavailable(t *testing.T) {
	h := newHarness(t, rolePtr(domain.ProjectRoleOwner))
	cause := errors.New("pq: connection refused to 10.0.0.7:5432")
	h.store.err = cause
	_, err := h.freeze(owner(), validInput())
	if !errors.Is(err, mainfreeze.ErrStore) {
		t.Fatalf("err = %v, want ErrStore", err)
	}

	// A policy that could not be READ is the refusal whose message reaches
	// the caller verbatim (the handler writes err.Error() for it), so the
	// driver's text must not be part of it — only the fixed reason is.
	pol := newHarness(t, rolePtr(domain.ProjectRoleOwner))
	pol.pol.err = cause
	_, perr := pol.freeze(owner(), validInput())
	if !errors.Is(perr, mainfreeze.ErrPolicyRefused) {
		t.Fatalf("err = %v, want ErrPolicyRefused", perr)
	}
	if strings.Contains(perr.Error(), "10.0.0.7") {
		t.Fatalf("the store's own text is in the refusal the caller reads: %v", perr)
	}
	if !errors.Is(perr, cause) {
		t.Fatalf("the failed read's cause is not in the chain for the log: %v", perr)
	}
}

// TestFreezePropagatesAPreviouslyFrozenProject: the store's own
// "no such project" outcome is the only one that survives as itself, and
// it is reachable only after the authorization passed (a member whose
// project vanished underneath the request).
func TestFreezePropagatesAPreviouslyFrozenProject(t *testing.T) {
	h := newHarness(t, rolePtr(domain.ProjectRoleMaintainer))
	h.store.err = mainfreeze.ErrProjectNotFound
	_, err := h.freeze(owner(), validInput())
	if !errors.Is(err, mainfreeze.ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
}
