package attestations_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/attestations"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// The unit half of T0812. It carries what the e2e cannot: the refusal
// VOCABULARY (every reason code, one case each, so a code with no producer
// or a producer with no code shows up here), the ORDER of the command's
// steps (a refusal must precede every lookup, and that is only observable
// against a store that counts its calls), and the two-gate attribution rule
// read over all four combinations of its inputs.

// A uuid-shaped string, so the shape checks pass and the assertions below
// are about the rule under test rather than about the fixture.
const (
	projectID  = "11111111-1111-4111-8111-111111111111"
	otherID    = "22222222-2222-4222-8222-222222222222"
	versionID  = "33333333-3333-4333-8333-333333333333"
	stateID    = "44444444-4444-4444-8444-444444444444"
	reviewID   = "55555555-5555-4555-8555-555555555555"
	orgID      = "66666666-6666-4666-8666-666666666666"
	objectID   = "77777777-7777-4777-8777-777777777777"
	assetID    = "88888888-8888-4888-8888-888888888888"
	userID     = "99999999-9999-4999-8999-999999999999"
	goodPIDStr = "01jq8zg9k0000000000000000a"
)

// publicProtocol is the admissible baseline: a protocol version that
// inherits its public project's visibility, resting on a state of the
// attesting project with an approved review of exactly that state.
func publicProtocol() attestations.Facts {
	return attestations.Facts{
		ProjectID: projectID,
		Target: attestations.TargetFacts{
			Kind:                    attestations.TargetKindProtocol,
			VersionID:               versionID,
			ObjectID:                objectID,
			Title:                   "A published protocol",
			LifecycleState:          string(domain.LifecycleActive),
			OwningProjectVisibility: "public",
		},
		Basis:  attestations.BasisFacts{StateID: stateID, ProjectID: projectID, StateHash: "sha256:deadbeef"},
		Review: attestations.ReviewFacts{ReviewID: reviewID, Kind: "scientific", Decision: string(domain.ReviewDecisionApproved), ReviewedStateID: stateID, PullRequestProjectID: projectID},
	}
}

func reasonCodes(reasons []attestations.Reason) []string {
	out := make([]string, 0, len(reasons))
	for _, r := range reasons {
		out = append(out, r.Code)
	}
	return out
}

func hasCode(reasons []attestations.Reason, code string) bool {
	for _, r := range reasons {
		if r.Code == code {
			return true
		}
	}
	return false
}

func TestJudgeAdmitsAnAttestationOfAPublicVersion(t *testing.T) {
	if reasons := attestations.Judge(publicProtocol(), false); len(reasons) != 0 {
		t.Fatalf("an admissible attestation reported %v", reasonCodes(reasons))
	}
	// And asking to name the organization is admissible when the project
	// has one that has opted in. Both inputs are facts, not the request.
	f := publicProtocol()
	f.OrganizationID, f.OrganizationSetting = orgID, attestations.OrgVisibilityNamed
	if reasons := attestations.Judge(f, true); len(reasons) != 0 {
		t.Fatalf("a named attestation with a named organization reported %v", reasonCodes(reasons))
	}
}

// The asset arm's positive direction, so the two-axis rule is pinned as a
// CONJUNCTION and not as a rule that refuses assets outright: a version
// public on both axes is admissible, and the very same facts with either
// axis turned are not. Without this, tightening the arm could collapse it to
// "assets are never public" and every refusal test would still pass.
func TestJudgeAdmitsAPublicAssetVersionOnBothAxes(t *testing.T) {
	publicAsset := func() attestations.Facts {
		f := publicProtocol()
		f.Target = attestations.TargetFacts{
			Kind: attestations.TargetKindAsset, VersionID: versionID, ObjectID: assetID,
			Title: "A published asset", AssetVisibility: "public",
			OwningProjectVisibility: "public",
		}
		return f
	}
	if reasons := attestations.Judge(publicAsset(), false); len(reasons) != 0 {
		t.Fatalf("an asset version public on both axes reported %v", reasonCodes(reasons))
	}
	for _, tc := range []struct {
		name string
		axis func(*attestations.TargetFacts)
	}{
		{"the version's own axis not public", func(t *attestations.TargetFacts) { t.AssetVisibility = "private" }},
		{"the origin project not public", func(t *attestations.TargetFacts) { t.OwningProjectVisibility = "private" }},
	} {
		f := publicAsset()
		tc.axis(&f.Target)
		if !hasCode(attestations.Judge(f, false), attestations.ReasonTargetNotPublic) {
			t.Errorf("%s: the asset target was not refused", tc.name)
		}
	}
}

func TestJudgeRefusesOneThingAtATime(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*attestations.Facts)
		want   string
	}{
		{
			name:   "a target of a kind the requirement does not name",
			mutate: func(f *attestations.Facts) { f.Target.Kind = "dataset" },
			want:   attestations.ReasonTargetKindNotAttestable,
		},
		{
			name: "a target whose owning project is private",
			mutate: func(f *attestations.Facts) {
				f.Target.OwningProjectVisibility = "private"
			},
			want: attestations.ReasonTargetNotPublic,
		},
		{
			// A pinned policy resolves to NOT public: this build has no
			// vocabulary that turns one into a public grant, so the
			// fail-closed reading is the only honest one.
			name: "a target that pins a visibility policy of its own",
			mutate: func(f *attestations.Facts) {
				pinned := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
				f.Target.VisibilityPolicyID = &pinned
			},
			want: attestations.ReasonTargetNotPublic,
		},
		{
			name: "an asset target that is not public",
			mutate: func(f *attestations.Facts) {
				f.Target = attestations.TargetFacts{
					Kind: attestations.TargetKindAsset, VersionID: versionID, ObjectID: assetID,
					Title: "An asset", AssetVisibility: "private",
				}
			},
			want: attestations.ReasonTargetNotPublic,
		},
		{
			// The asset arm's SECOND axis. A version flagged 'public'
			// inside a project that is not is exactly the row the three
			// other reachability decisions refuse (asset page read gate,
			// asset feed existence, subscription audience), so a target
			// predicate that stopped at the version's own axis would read
			// it public when the platform does not.
			name: "an asset target whose own axis is public inside a private project",
			mutate: func(f *attestations.Facts) {
				f.Target = attestations.TargetFacts{
					Kind: attestations.TargetKindAsset, VersionID: versionID, ObjectID: assetID,
					Title: "An asset", AssetVisibility: "public",
					OwningProjectVisibility: "private",
				}
			},
			want: attestations.ReasonTargetNotPublic,
		},
		{
			name:   "a target version that is currently retracted",
			mutate: func(f *attestations.Facts) { f.Target.LifecycleState = string(domain.LifecycleAborted) },
			want:   attestations.ReasonTargetAborted,
		},
		{
			name:   "a basis state that is not the attesting project's",
			mutate: func(f *attestations.Facts) { f.Basis.ProjectID = otherID },
			want:   attestations.ReasonBasisStateNotOwned,
		},
		{
			name:   "no internal review named at all",
			mutate: func(f *attestations.Facts) { f.Review = attestations.ReviewFacts{} },
			want:   attestations.ReasonInternalReviewNotOfBasis,
		},
		{
			name: "a review of a different state",
			mutate: func(f *attestations.Facts) {
				f.Review.ReviewedStateID = otherID
			},
			want: attestations.ReasonInternalReviewNotOfBasis,
		},
		{
			name: "a review on a pull request of another project",
			mutate: func(f *attestations.Facts) {
				f.Review.PullRequestProjectID = otherID
			},
			want: attestations.ReasonInternalReviewNotOfBasis,
		},
		{
			name:   "a review that did not approve the state",
			mutate: func(f *attestations.Facts) { f.Review.Decision = string(domain.ReviewDecisionChangesRequested) },
			want:   attestations.ReasonInternalReviewNotApproved,
		},
		{
			name: "asking to be named with no organization at all",
			mutate: func(f *attestations.Facts) {
				f.OrganizationID, f.OrganizationSetting = "", ""
			},
			want: attestations.ReasonAttributionNotPermitted,
		},
		{
			name: "asking to be named while the organization stands anonymous",
			mutate: func(f *attestations.Facts) {
				f.OrganizationID, f.OrganizationSetting = orgID, attestations.OrgVisibilityAnonymous
			},
			want: attestations.ReasonAttributionNotPermitted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := publicProtocol()
			tc.mutate(&f)
			// wantNamed is set for every case, so the attribution cases are
			// reached; the others refuse for their own reason regardless.
			wantNamed := strings.HasPrefix(tc.want, "attribution_")
			reasons := attestations.Judge(f, wantNamed)
			if !hasCode(reasons, tc.want) {
				t.Fatalf("reasons = %v, want one of them to be %q", reasonCodes(reasons), tc.want)
			}
			// The refusal names what it read, so a caller knows where to
			// look rather than only that it was refused.
			for _, r := range reasons {
				if r.Code == tc.want && strings.TrimSpace(r.Detail) == "" {
					t.Errorf("reason %q carries no detail", tc.want)
				}
			}
		})
	}
}

// TestJudgeAnonymousIsAdmissibleEvenWhereNamedIsNot is the floor rule: a
// standing setting is a FLOOR on disclosure, not a mandate to disclose.
// An organization that has opted to be named does not force its projects to
// name it, and a project with no organization attests anonymously without
// any refusal at all.
func TestJudgeAnonymousIsAdmissibleEvenWhereNamedIsNot(t *testing.T) {
	named := publicProtocol()
	named.OrganizationID, named.OrganizationSetting = orgID, attestations.OrgVisibilityNamed
	if reasons := attestations.Judge(named, false); len(reasons) != 0 {
		t.Errorf("an anonymous attestation by a named organization was refused: %v", reasonCodes(reasons))
	}
	noOrg := publicProtocol()
	if reasons := attestations.Judge(noOrg, false); len(reasons) != 0 {
		t.Errorf("an anonymous attestation by a project with no organization was refused: %v", reasonCodes(reasons))
	}
}

// TestPresentIsAConjunctionOfTwoGates walks all four combinations of the
// recorded attribution and the organization's standing setting.
func TestPresentIsAConjunctionOfTwoGates(t *testing.T) {
	org := &attestations.RowOrganization{ID: orgID, Slug: "att-labs", Name: "Attestation Labs"}
	row := func(recorded, setting string) attestations.Row {
		o := *org
		o.Setting = setting
		return attestations.Row{
			PID: goodPIDStr, ValidationType: "reproduction", ValidationResult: "confirmed",
			CreatedAt:    time.Unix(0, 0).UTC(),
			Target:       attestations.RowTarget{Kind: attestations.TargetKindProtocol, ObjectID: objectID, VersionID: versionID, Title: "T"},
			Organization: &o,
		}
	}
	cases := []struct {
		recorded, setting string
		named             bool
	}{
		{attestations.OrgVisibilityNamed, attestations.OrgVisibilityNamed, true},
		{attestations.OrgVisibilityNamed, attestations.OrgVisibilityAnonymous, false},
		{attestations.OrgVisibilityAnonymous, attestations.OrgVisibilityNamed, false},
		{attestations.OrgVisibilityAnonymous, attestations.OrgVisibilityAnonymous, false},
	}
	for _, tc := range cases {
		r := row(tc.recorded, tc.setting)
		r.OrgVisibility = tc.recorded
		got := attestations.Present(r)
		if got.AttributedBy.Mode == attestations.OrgVisibilityNamed != tc.named {
			t.Errorf("recorded=%s setting=%s -> mode=%s, want named=%v",
				tc.recorded, tc.setting, got.AttributedBy.Mode, tc.named)
		}
		if tc.named && (got.AttributedBy.Organization == nil || got.AttributedBy.Organization.Slug != "att-labs") {
			t.Errorf("recorded=%s setting=%s named the organization as %+v", tc.recorded, tc.setting, got.AttributedBy.Organization)
		}
		if !tc.named && got.AttributedBy.Organization != nil {
			t.Errorf("recorded=%s setting=%s still carries an organization", tc.recorded, tc.setting)
		}
		// The disclosure clause is on every row and is never conditional.
		if got.Disclosure.IsEvidence || got.Disclosure.NamesEvidence || got.Disclosure.Statement != attestations.Statement {
			t.Errorf("disclosure = %+v, want the fixed not-evidence statement", got.Disclosure)
		}
	}
}

// TestPresentCollapsesTheTwoAnonymousCases: a project with no organization
// and an organization that may not be named render IDENTICALLY. Which of
// the two holds is a fact about the attesting project's private side, and a
// projection that distinguished them would disclose it.
func TestPresentCollapsesTheTwoAnonymousCases(t *testing.T) {
	base := attestations.Row{
		PID: goodPIDStr, OrgVisibility: attestations.OrgVisibilityAnonymous,
		ValidationType: "reproduction", ValidationResult: "confirmed", CreatedAt: time.Unix(0, 0).UTC(),
		Target: attestations.RowTarget{Kind: attestations.TargetKindProtocol, ObjectID: objectID, VersionID: versionID, Title: "T"},
	}
	noOrg := attestations.Present(base)

	withOrg := base
	withOrg.Organization = &attestations.RowOrganization{ID: orgID, Slug: "att-labs", Name: "Attestation Labs", Setting: attestations.OrgVisibilityAnonymous}
	orgPresent := attestations.Present(withOrg)

	if orgPresent.AttributedBy != noOrg.AttributedBy {
		t.Fatalf("the two anonymous cases differ: %+v vs %+v", noOrg.AttributedBy, orgPresent.AttributedBy)
	}
	// And a row that RECORDED named but whose organization is gone reads
	// anonymous too — the projection never invents an organization.
	namedNoOrg := base
	namedNoOrg.OrgVisibility = attestations.OrgVisibilityNamed
	if got := attestations.Present(namedNoOrg); got.AttributedBy.Mode != attestations.OrgVisibilityAnonymous {
		t.Errorf("a named attestation with no organization row reads %q", got.AttributedBy.Mode)
	}
}

func TestParseTargetRef(t *testing.T) {
	ok := []struct {
		ref    string
		object string
		asset  string
	}{
		{"object_version:" + versionID, versionID, ""},
		{"asset_version:" + versionID, "", versionID},
		{"  object_version:" + versionID + "  ", versionID, ""},
	}
	for _, tc := range ok {
		got, err := attestations.ParseTargetRef(tc.ref)
		if err != nil {
			t.Errorf("ParseTargetRef(%q) = %v", tc.ref, err)
			continue
		}
		if got.ObjectVersionID != tc.object || got.AssetVersionID != tc.asset {
			t.Errorf("ParseTargetRef(%q) = %+v", tc.ref, got)
		}
	}
	// A bare uuid is REFUSED here, unlike a knowledge_version_ref: the
	// prefix is the only thing that says which of two tables the uuid
	// indexes, so accepting one would make the API guess.
	bad := []string{
		"", "   ", versionID, "object_version:", "object_version:not-a-uuid",
		"asset_version:", "asset_version:" + versionID + "x", "version:" + versionID,
		"object_version:" + versionID + ",asset_version:" + versionID,
	}
	for _, ref := range bad {
		if _, err := attestations.ParseTargetRef(ref); !errors.Is(err, attestations.ErrValidation) {
			t.Errorf("ParseTargetRef(%q) err = %v, want ErrValidation", ref, err)
		}
	}
}

// TestBuildPreviewShowsThePrivateHalfItWithholds: the preview exists for
// the caller to see, before writing, that the underlying work stays where
// it is.
func TestBuildPreviewShowsThePrivateHalfItWithholds(t *testing.T) {
	w := attestations.Want{ValidationType: "reproduction", ValidationResult: "confirmed", OrgVisibility: attestations.OrgVisibilityAnonymous}
	got := attestations.BuildPreview(publicProtocol(), w)
	if !got.Admissible || len(got.Blocking) != 0 {
		t.Fatalf("an admissible request previewed %v", reasonCodes(got.Blocking))
	}
	if got.WouldNameOrganization {
		t.Error("the preview promised to name an organization the project does not have")
	}
	fields := map[string]bool{}
	for _, item := range got.Withheld {
		fields[item.Field] = true
		if item.Revealed {
			t.Errorf("withheld entry %q is flagged revealed", item.Field)
		}
	}
	for _, want := range []string{
		"attesting_project", "attesting_project.basis_state",
		"attesting_project.internal_review", "attesting_project.evidence",
	} {
		if !fields[want] {
			t.Errorf("the withheld list does not name %q: %v", want, got.Withheld)
		}
	}
	for _, item := range got.Published {
		if !item.Revealed {
			t.Errorf("published entry %q is not flagged revealed", item.Field)
		}
	}

	// And a refusal is reported as a refusal: blocking entries plus
	// admissible=false, in the same document.
	f := publicProtocol()
	f.Target.OwningProjectVisibility = "private"
	blocked := attestations.BuildPreview(f, w)
	if blocked.Admissible || !hasCode(blocked.Blocking, attestations.ReasonTargetNotPublic) {
		t.Fatalf("a private target previewed %+v", blocked)
	}
}

// ------------------------------------------------------------------ command

// fakeStore counts what was asked of it, so a test can assert that a
// refusal happened BEFORE the store was reached — the property the ordering
// exists for and the one no functional assertion can see.
type fakeStore struct {
	resolveCalls int
	attestCalls  int
	facts        attestations.Facts
	attestErr    error
	lastResolve  attestations.ResolveRequest
	lastAttest   attestations.AttestRequest
}

func (s *fakeStore) ResolveFacts(_ context.Context, in attestations.ResolveRequest) (attestations.Facts, error) {
	s.resolveCalls++
	s.lastResolve = in
	return s.facts, nil
}

func (s *fakeStore) Attest(_ context.Context, req attestations.AttestRequest) (attestations.Attested, error) {
	s.attestCalls++
	s.lastAttest = req
	if s.attestErr != nil {
		return attestations.Attested{}, s.attestErr
	}
	return attestations.Attested{
		PID: req.PID, ValidationType: req.ValidationType, ValidationResult: req.ValidationResult,
		OrgVisibility: req.OrgVisibility, CreatedAt: time.Unix(0, 0).UTC(),
	}, nil
}

func (s *fakeStore) GetPublicAttestation(context.Context, string) (attestations.PublicAttestation, bool, error) {
	return attestations.PublicAttestation{}, false, nil
}

type fakeMembers struct {
	calls int
	role  *domain.ProjectRole
	err   error
}

func (m *fakeMembers) GetMembership(context.Context, string, string) (domain.ProjectMembership, error) {
	m.calls++
	if m.err != nil {
		return domain.ProjectMembership{}, m.err
	}
	if m.role == nil {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return domain.ProjectMembership{Role: *m.role}, nil
}

type stubEngine struct{ verdict authz.Verdict }

func (e stubEngine) Authorize(context.Context, authz.Request) (authz.Decision, error) {
	return authz.Decision{Verdict: e.verdict}, nil
}

func owner() *domain.ProjectRole  { r := domain.ProjectRoleOwner; return &r }
func member() *domain.ProjectRole { r := domain.ProjectRoleViewer; return &r }

func validParams() attestations.PublishParams {
	return attestations.PublishParams{
		ProjectID: projectID, TargetRef: "object_version:" + versionID,
		ValidationType: "reproduction", ValidationResult: "confirmed",
		OrgVisibility: attestations.OrgVisibilityAnonymous,
		BasisStateID:  stateID, InternalReviewID: reviewID,
	}
}

func newTestCommand(t *testing.T, store attestations.StorePort, members attestations.MembershipPort, engine authz.Engine, pid string) *attestations.Command {
	t.Helper()
	return attestations.NewCommand(attestations.Deps{
		Members: members, Store: store, Authz: engine,
		NewPID: func() (domain.PID, error) { return domain.PID(pid), nil },
	})
}

func TestCommandPublishRefusesBeforeItReadsAnything(t *testing.T) {
	alice := attestations.Actor{User: domain.User{ID: userID}}

	t.Run("shape, before the membership is even asked for", func(t *testing.T) {
		store, members := &fakeStore{facts: publicProtocol()}, &fakeMembers{role: owner()}
		cmd := newTestCommand(t, store, members, stubEngine{authz.VerdictAllow}, goodPIDStr)
		cases := map[string]func(*attestations.PublishParams){
			"a bare uuid target":         func(p *attestations.PublishParams) { p.TargetRef = versionID },
			"an unknown validation type": func(p *attestations.PublishParams) { p.ValidationType = "vibes" },
			"an unknown result":          func(p *attestations.PublishParams) { p.ValidationResult = "probably" },
			"an unknown attribution":     func(p *attestations.PublishParams) { p.OrgVisibility = "maybe" },
			"a non-uuid basis state":     func(p *attestations.PublishParams) { p.BasisStateID = "the-latest-one" },
			"a missing internal review":  func(p *attestations.PublishParams) { p.InternalReviewID = "" },
			"a non-uuid project":         func(p *attestations.PublishParams) { p.ProjectID = "my-project" },
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				params := validParams()
				mutate(&params)
				if _, err := cmd.Publish(context.Background(), alice, params); !errors.Is(err, attestations.ErrValidation) {
					t.Fatalf("err = %v, want ErrValidation", err)
				}
				if members.calls != 0 || store.resolveCalls != 0 || store.attestCalls != 0 {
					t.Fatalf("a malformed request reached the ports (members=%d resolve=%d attest=%d)",
						members.calls, store.resolveCalls, store.attestCalls)
				}
			})
		}
	})

	t.Run("an agent is refused by the domain backstop, not by the matrix", func(t *testing.T) {
		store, members := &fakeStore{facts: publicProtocol()}, &fakeMembers{role: owner()}
		// The matrix says ALLOW, so only the backstop can refuse this.
		cmd := newTestCommand(t, store, members, stubEngine{authz.VerdictAllow}, goodPIDStr)
		_, err := cmd.Publish(context.Background(), attestations.Actor{User: domain.User{ID: userID}, IsAgent: true}, validParams())
		var denied *attestations.AgentNotPermittedError
		if !errors.As(err, &denied) {
			t.Fatalf("err = %v, want AgentNotPermittedError", err)
		}
		if denied.Code() != attestations.CodeAgentAttestDenied {
			t.Errorf("code = %q, want %q", denied.Code(), attestations.CodeAgentAttestDenied)
		}
		if members.calls != 0 || store.resolveCalls != 0 || store.attestCalls != 0 {
			t.Fatalf("an agent reached the ports (members=%d resolve=%d attest=%d)",
				members.calls, store.resolveCalls, store.attestCalls)
		}
	})

	t.Run("a denial refuses before the facts are resolved", func(t *testing.T) {
		store, members := &fakeStore{facts: publicProtocol()}, &fakeMembers{role: member()}
		cmd := newTestCommand(t, store, members, stubEngine{authz.VerdictConditional}, goodPIDStr)
		if _, err := cmd.Publish(context.Background(), alice, validParams()); !errors.Is(err, attestations.ErrForbidden) {
			t.Fatalf("err = %v, want ErrForbidden", err)
		}
		// A conditional cell is NOT a permit (authz.Permits admits only
		// allow), and the refusal happened before the store was read.
		if members.calls != 1 {
			t.Errorf("membership was asked %d times, want 1", members.calls)
		}
		if store.resolveCalls != 0 || store.attestCalls != 0 {
			t.Fatalf("a denied caller reached the store (resolve=%d attest=%d)", store.resolveCalls, store.attestCalls)
		}
	})

	t.Run("a non-member is denied, and an invisible project is not found", func(t *testing.T) {
		// The REAL matrix, not a stub: this is the assertion that
		// authz.ActionPublishPrivateToPublic admits an owner of the
		// attesting project and refuses an authenticated account with no
		// role in it. A stub engine could not tell those two apart, and the
		// difference is the whole authorization.
		store := &fakeStore{facts: publicProtocol()}
		cmd := newTestCommand(t, store, &fakeMembers{role: nil}, authz.NewMatrixEngine(), goodPIDStr)
		if _, err := cmd.Publish(context.Background(), alice, validParams()); !errors.Is(err, attestations.ErrForbidden) {
			t.Fatalf("non-member err = %v, want ErrForbidden", err)
		}

		// The owner, over that same matrix, is admitted — so the refusal
		// above is about the role and not about the action being
		// unauthorized for everybody.
		ownerStore := &fakeStore{facts: publicProtocol()}
		ownerCmd := newTestCommand(t, ownerStore, &fakeMembers{role: owner()}, authz.NewMatrixEngine(), goodPIDStr)
		if _, err := ownerCmd.Publish(context.Background(), alice, validParams()); err != nil {
			t.Fatalf("an owner was refused by the real matrix: %v", err)
		}
		if ownerStore.attestCalls != 1 {
			t.Errorf("the owner's publish reached the store %d times, want 1", ownerStore.attestCalls)
		}

		store2 := &fakeStore{facts: publicProtocol()}
		cmd2 := newTestCommand(t, store2, &fakeMembers{err: projects.ErrProjectNotFound}, stubEngine{authz.VerdictAllow}, goodPIDStr)
		if _, err := cmd2.Publish(context.Background(), alice, validParams()); !errors.Is(err, attestations.ErrProjectNotFound) {
			t.Fatalf("invisible project err = %v, want ErrProjectNotFound", err)
		}
		if store.resolveCalls != 0 || store2.resolveCalls != 0 || ownerStore.resolveCalls != 0 {
			t.Error("a refusal resolved facts first")
		}
	})
}

func TestCommandPublishCarriesTheResolvedRequestAndMintsAPid(t *testing.T) {
	store, members := &fakeStore{facts: publicProtocol()}, &fakeMembers{role: owner()}
	cmd := newTestCommand(t, store, members, stubEngine{authz.VerdictAllow}, goodPIDStr)
	got, err := cmd.Publish(context.Background(), attestations.Actor{User: domain.User{ID: userID}}, validParams())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got.PID != goodPIDStr {
		t.Errorf("pid = %q, want the minted %q", got.PID, goodPIDStr)
	}
	if store.attestCalls != 1 {
		t.Fatalf("store.Attest called %d times", store.attestCalls)
	}
	// The private footing travels to the store (it is recorded, never
	// published) and the target is pinned as ONE version id, not two.
	if req := store.lastAttest.Resolve; req.TargetObjectVersionID != versionID || req.TargetAssetVersionID != "" {
		t.Errorf("resolved target = %+v, want the object version only", req)
	}
	if req := store.lastAttest.Resolve; req.BasisStateID != stateID || req.InternalReviewID != reviewID {
		t.Errorf("resolved private footing = %+v", req)
	}
	// The reader travels with the resolution, and it is the ACTOR — not a
	// value from the request. The store resolves the target through this id,
	// so a command that dropped it would resolve every target as a reader
	// with no memberships and hand back the facts of versions the caller may
	// not read (the leak this field exists to close).
	if req := store.lastAttest.Resolve; req.ReaderID != userID {
		t.Errorf("resolved reader = %q, want the actor %q", req.ReaderID, userID)
	}
	if store.lastAttest.Audit.Action != attestations.ActionAttestationCreated {
		t.Errorf("audit action = %q, want %q", store.lastAttest.Audit.Action, attestations.ActionAttestationCreated)
	}
	// The audit row is project-scoped, so it MAY name what the public
	// record does not — an audit that could not say what was done would not
	// be an audit.
	summary, ok := store.lastAttest.Audit.AfterSummary.(map[string]any)
	if !ok {
		t.Fatalf("audit summary is %T, want a map", store.lastAttest.Audit.AfterSummary)
	}
	for _, key := range []string{"basis_state_id", "internal_review_id", "target_version_id"} {
		if _, ok := summary[key]; !ok {
			t.Errorf("the audit summary does not record %q", key)
		}
	}
}

func TestCommandPublishPassesARefusalThroughAndRejectsABadPid(t *testing.T) {
	refused := &attestations.Refused{
		Preview: attestations.BuildPreview(publicProtocol(), attestations.Want{}),
		Reasons: []string{attestations.ReasonTargetNotPublic + ": no"},
	}
	store := &fakeStore{facts: publicProtocol(), attestErr: refused}
	cmd := newTestCommand(t, store, &fakeMembers{role: owner()}, stubEngine{authz.VerdictAllow}, goodPIDStr)
	_, err := cmd.Publish(context.Background(), attestations.Actor{User: domain.User{ID: userID}}, validParams())
	var got *attestations.Refused
	if !errors.As(err, &got) {
		t.Fatalf("err = %v, want *Refused", err)
	}
	if got.Code() != attestations.CodeAttestationRefused {
		t.Errorf("code = %q, want %q", got.Code(), attestations.CodeAttestationRefused)
	}
	if len(got.Reasons) != 1 || len(got.Preview.Withheld) == 0 {
		t.Errorf("the refusal arrived without its report: %+v", got)
	}

	// A generator that produces something that is not a pid is a wiring
	// fault, reported as a store failure rather than written and refused by
	// the column CHECK.
	store2 := &fakeStore{facts: publicProtocol()}
	cmd2 := newTestCommand(t, store2, &fakeMembers{role: owner()}, stubEngine{authz.VerdictAllow}, "not-a-pid")
	if _, err := cmd2.Publish(context.Background(), attestations.Actor{User: domain.User{ID: userID}}, validParams()); !errors.Is(err, attestations.ErrStore) {
		t.Fatalf("bad pid err = %v, want ErrStore", err)
	}
	if store2.attestCalls != 0 {
		t.Error("a bad pid still reached the store")
	}
}

func TestCommandPreviewWritesNothingAndReportsARefusalAsOne(t *testing.T) {
	store := &fakeStore{facts: publicProtocol()}
	cmd := newTestCommand(t, store, &fakeMembers{role: owner()}, stubEngine{authz.VerdictAllow}, goodPIDStr)
	preview, err := cmd.Preview(context.Background(), attestations.Actor{User: domain.User{ID: userID}}, validParams())
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	if !preview.Admissible {
		t.Fatalf("an admissible request previewed %+v", preview)
	}
	if store.attestCalls != 0 {
		t.Fatal("a preview wrote")
	}
	if preview.ProjectID != projectID || preview.Target.VersionID != versionID {
		t.Errorf("preview = %+v", preview)
	}
	// The preview resolves the target through the actor as well: the read it
	// makes is the same reader-relative one the publish re-makes, or a
	// preview could show a target the publish then answers 404 for.
	if store.lastResolve.ReaderID != userID {
		t.Errorf("preview resolved the target for reader %q, want the actor %q", store.lastResolve.ReaderID, userID)
	}

	// The same request over a store that sees a private target: the answer
	// is a *Refused carrying the document the caller previewed against,
	// because "no" is an answer and not a transport error.
	private := publicProtocol()
	private.Target.OwningProjectVisibility = "private"
	blocked := &fakeStore{facts: private}
	cmd = newTestCommand(t, blocked, &fakeMembers{role: owner()}, stubEngine{authz.VerdictAllow}, goodPIDStr)
	preview, err = cmd.Preview(context.Background(), attestations.Actor{User: domain.User{ID: userID}}, validParams())
	var got *attestations.Refused
	if !errors.As(err, &got) {
		t.Fatalf("err = %v, want *Refused", err)
	}
	if preview.Admissible {
		t.Error("the refusal's document says it is admissible")
	}
	if blocked.attestCalls != 0 {
		t.Error("a refused preview wrote")
	}
}

// TestVocabulariesAreClosed pins the three lists: every value is one the
// database CHECK in migration 00120 also admits, and the validators agree
// with the lists rather than with a second copy of them.
func TestVocabulariesAreClosed(t *testing.T) {
	for _, v := range attestations.ValidationTypes() {
		if !attestations.ValidValidationType(v) {
			t.Errorf("ValidationTypes() lists %q and ValidValidationType refuses it", v)
		}
	}
	for _, v := range attestations.ValidationResults() {
		if !attestations.ValidValidationResult(v) {
			t.Errorf("ValidationResults() lists %q and ValidValidationResult refuses it", v)
		}
	}
	for _, v := range attestations.OrgVisibilities() {
		if !attestations.ValidOrgVisibility(v) {
			t.Errorf("OrgVisibilities() lists %q and ValidOrgVisibility refuses it", v)
		}
	}
	if attestations.ValidValidationType("") || attestations.ValidValidationResult("") || attestations.ValidOrgVisibility("") {
		t.Error("the empty string is admitted by a closed vocabulary")
	}
	for _, k := range attestations.AttestableTargetKinds() {
		if !k.Attestable() {
			t.Errorf("AttestableTargetKinds() lists %q and Attestable() refuses it", k)
		}
	}
	if attestations.TargetKind("dataset").Attestable() {
		t.Error("a dataset object is admitted as an attestation target")
	}
	// The pid predicate is the column's, not a second spelling of it.
	if !attestations.ValidPID(goodPIDStr) {
		t.Errorf("ValidPID(%q) = false", goodPIDStr)
	}
	for _, bad := range []string{"", "short", strings.ToUpper(goodPIDStr), "01jq8zg9k0000000000000000i"} {
		if attestations.ValidPID(bad) {
			t.Errorf("ValidPID(%q) = true", bad)
		}
	}
}
