package authz_test

import (
	"context"
	"testing"

	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// TestEngineDefaultDeny: the engine's contract is default deny — an
// unknown action, an unknown actor class or an empty request is denied,
// never allowed (docs/12, docs/50).
func TestEngineDefaultDeny(t *testing.T) {
	engine := authz.NewMatrixEngine()
	ctx := context.Background()
	cases := []authz.Request{
		{Action: "not_an_action", Class: authz.ActorOwner},
		{Action: authz.ActionMergeMain, Class: "not_a_class"},
		{Action: "", Class: authz.ActorOwner},
		{Action: authz.ActionMergeMain, Class: ""},
		{},
	}
	for _, req := range cases {
		decision, err := engine.Authorize(ctx, req)
		if err != nil {
			t.Fatalf("Authorize(%+v) error: %v (the matrix engine never fails)", req, err)
		}
		if !decision.Denies() {
			t.Errorf("Authorize(%+v) = %q, want deny", req, decision.Verdict)
		}
		if decision.Permits() {
			t.Errorf("Authorize(%+v) permits, default deny violated", req)
		}
	}
}

// TestEngineMatrixCells spot-checks representative cells end to end
// through the engine, including the conditional forms and the columns
// that exist only to refuse (docs/10: the Files Web page is strictly
// read-only — deny for everyone).
func TestEngineMatrixCells(t *testing.T) {
	engine := authz.NewMatrixEngine()
	ctx := context.Background()
	cases := []struct {
		req  authz.Request
		want authz.Verdict
	}{
		{authz.Request{authz.ActionMergeMain, authz.ActorOwner}, authz.VerdictAllow},
		{authz.Request{authz.ActionMergeMain, authz.ActorMaintainer}, authz.VerdictAllow},
		{authz.Request{authz.ActionMergeMain, authz.ActorContributor}, authz.VerdictDeny},
		{authz.Request{authz.ActionMergeMain, authz.ActorAgent}, authz.VerdictDeny},
		{authz.Request{authz.ActionReadPrivateProject, authz.ActorViewer}, authz.VerdictAllow},
		{authz.Request{authz.ActionReadPrivateProject, authz.ActorAuthenticatedNonMember}, authz.VerdictDeny},
		{authz.Request{authz.ActionReadPrivateProject, authz.ActorPublicAnonymous}, authz.VerdictDeny},
		{authz.Request{authz.ActionReadPrivateProject, authz.ActorAgent}, authz.VerdictScoped},
		{authz.Request{authz.ActionCreateBranch, authz.ActorAuthenticatedNonMember}, authz.VerdictExternalForkOnly},
		{authz.Request{authz.ActionWriteScientificState, authz.ActorAuthenticatedNonMember}, authz.VerdictOwnForkOnly},
		{authz.Request{authz.ActionSubmitScientificReview, authz.ActorContributor}, authz.VerdictConditional},
		{authz.Request{authz.ActionPublishPrivateToPublic, authz.ActorMaintainer}, authz.VerdictConditional},
		{authz.Request{authz.ActionPublishPrivateToPublic, authz.ActorOwner}, authz.VerdictAllow},
		{authz.Request{authz.ActionPublishPrivateToPublic, authz.ActorAgent}, authz.VerdictDeny},
		{authz.Request{authz.ActionAbortMainObject, authz.ActorMaintainer}, authz.VerdictViaPR},
		{authz.Request{authz.ActionCreateRelease, authz.ActorAgent}, authz.VerdictProposalOnly},
		{authz.Request{authz.ActionReadFiles, authz.ActorPublicAnonymous}, authz.VerdictPublicPolicy},
		{authz.Request{authz.ActionReadFiles, authz.ActorViewer}, authz.VerdictAuthorized},
		{authz.Request{authz.ActionReadFiles, authz.ActorAgent}, authz.VerdictAllowIfAuthorized},
		{authz.Request{authz.ActionMutateFilesWeb, authz.ActorOwner}, authz.VerdictDeny},
		{authz.Request{authz.ActionMutateFilesWeb, authz.ActorAgent}, authz.VerdictDeny},
		{authz.Request{authz.ActionChangeRightsHolder, authz.ActorOwner}, authz.VerdictAllow},
		{authz.Request{authz.ActionChangeRightsHolder, authz.ActorMaintainer}, authz.VerdictDeny},
		{authz.Request{authz.ActionCreateProject, authz.ActorAuthenticatedNonMember}, authz.VerdictAllow},
		{authz.Request{authz.ActionCreateProject, authz.ActorPublicAnonymous}, authz.VerdictDeny},
		{authz.Request{authz.ActionCreateProject, authz.ActorAgent}, authz.VerdictScoped},
	}
	for _, c := range cases {
		decision, err := engine.Authorize(ctx, c.req)
		if err != nil {
			t.Fatalf("Authorize(%+v) error: %v", c.req, err)
		}
		if decision.Verdict != c.want {
			t.Errorf("Authorize(%+v) = %q, want %q", c.req, decision.Verdict, c.want)
		}
	}
}

// TestClassOf: the actor's matrix column is resolved from facts the
// service verified — the session, the membership row, the agent flag —
// and an unknown role yields no class at all (the engine then denies).
func TestClassOf(t *testing.T) {
	owner := domain.ProjectRoleOwner
	maintainer := domain.ProjectRoleMaintainer
	contributor := domain.ProjectRoleContributor
	viewer := domain.ProjectRoleViewer
	bogus := domain.ProjectRole("admin")
	cases := []struct {
		name          string
		authenticated bool
		role          *domain.ProjectRole
		isAgent       bool
		want          authz.ActorClass
	}{
		{"anonymous", false, nil, false, authz.ActorPublicAnonymous},
		{"authenticated non-member", true, nil, false, authz.ActorAuthenticatedNonMember},
		{"viewer", true, &viewer, false, authz.ActorViewer},
		{"contributor", true, &contributor, false, authz.ActorContributor},
		{"maintainer", true, &maintainer, false, authz.ActorMaintainer},
		{"owner", true, &owner, false, authz.ActorOwner},
		{"agent wins over membership", true, &owner, true, authz.ActorAgent},
		{"agent wins over anonymity", false, nil, true, authz.ActorAgent},
		{"unknown role has no class", true, &bogus, false, ""},
	}
	for _, c := range cases {
		if got := authz.ClassOf(c.authenticated, c.role, c.isAgent); got != c.want {
			t.Errorf("ClassOf(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestClassOfUnknownRoleDenied: a membership row with a role outside the
// four canonical ones (impossible through the DB CHECK, but defense in
// depth) must deny, not escalate to anonymous or owner.
func TestClassOfUnknownRoleDenied(t *testing.T) {
	bogus := domain.ProjectRole("admin")
	engine := authz.NewMatrixEngine()
	decision, err := engine.Authorize(context.Background(), authz.Request{
		Action: authz.ActionReadPrivateProject,
		Class:  authz.ClassOf(true, &bogus, false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Denies() {
		t.Fatalf("unknown role read = %q, want deny", decision.Verdict)
	}
}

// TestMatrixCopyIsolation: the exported Matrix is a copy — mutating it
// must not corrupt the engine's decisions.
func TestMatrixCopyIsolation(t *testing.T) {
	engine := authz.NewMatrixEngine()
	m := authz.Matrix()
	m[authz.ActionMergeMain][authz.ActorContributor] = authz.VerdictAllow
	decision, err := engine.Authorize(context.Background(), authz.Request{
		Action: authz.ActionMergeMain,
		Class:  authz.ActorContributor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.Verdict != authz.VerdictDeny {
		t.Errorf("merge_main for contributor = %q after mutating the exported copy, want deny", decision.Verdict)
	}
}

// TestVerdictSemantics: only allow permits; deny denies; conditional
// forms are neither — an enforcement site that does not resolve the
// condition must refuse.
func TestVerdictSemantics(t *testing.T) {
	if !authz.VerdictAllow.Permits() || authz.VerdictAllow.Denies() {
		t.Error("allow must permit and not deny")
	}
	if authz.VerdictDeny.Permits() || !authz.VerdictDeny.Denies() {
		t.Error("deny must deny and not permit")
	}
	conditional := []authz.Verdict{
		authz.VerdictScoped, authz.VerdictConditional,
		authz.VerdictExternalForkOnly, authz.VerdictOwnForkOnly,
		authz.VerdictAllowFromFork, authz.VerdictProposalOrScoped,
		authz.VerdictProposalOnly, authz.VerdictViaPR,
		authz.VerdictPublicPolicy, authz.VerdictAuthorized,
		authz.VerdictAllowIfAuthorized,
	}
	for _, v := range conditional {
		if v.Permits() {
			t.Errorf("%q must not permit unconditionally", v)
		}
		if v.Denies() {
			t.Errorf("%q is conditional, not an outright deny", v)
		}
	}
}
