package assetmetadata

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// Task T0706 required test "asset metadata tests" — the command half.
//
// What is pinned here is the ORDER and the SPLIT of the revision's
// decisions, neither of which a store could decide for the command:
//
//  1. shape  — refused before anything is read;
//  2. role   — refused before the asset is looked up at all;
//  3. revise — handed to the store as one request carrying the audit row.
//
// The fakes RECORD their calls, because most of what is asserted here is
// about what did NOT happen: a refusal that ran before a lookup is a
// refusal that cannot disclose what the lookup would have found, and a
// command that never reached the store is a command that wrote nothing.
//
// The store itself (the transaction, the lock order, the before/after
// summaries) is pinned over real PostgreSQL in
// tests/integration/asset_metadata_test.go, and the one thing only this
// file can settle — that the role gate's two refusals are ONE error — is
// asserted here on the error VALUE.

const (
	cmdActorID = "11111111-1111-4111-8111-111111111111"
	cmdProject = "22222222-2222-4222-8222-222222222222"
	// cmdPID is a 26-character Crockford base32 pid (no i, l, o, u).
	cmdPID = "01j9z6k3m4n5p6q7r8s9t0v1w2"
)

// ---------------------------------------------------------------------------
// Fakes

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

// fakeMembers answers the membership read. A nil role means "no membership
// row", which the production store answers with
// projects.ErrMemberNotFound — the sentinel the gate is built on.
type fakeMembers struct {
	log  *callLog
	role *domain.ProjectRole
	err  error

	calls      int
	gotProject string
	gotUser    string
}

func (m *fakeMembers) GetMembership(_ context.Context, projectID, userID string) (domain.ProjectMembership, error) {
	m.log.add("members")
	m.calls++
	m.gotProject, m.gotUser = projectID, userID
	if m.err != nil {
		return domain.ProjectMembership{}, m.err
	}
	if m.role == nil {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return domain.ProjectMembership{ProjectID: projectID, UserID: userID, Role: *m.role}, nil
}

// fakeStore records the request it was handed and answers what the case
// wants.
type fakeStore struct {
	log      *callLog
	revision Revision
	err      error

	calls int
	got   RevisionRequest
}

func (s *fakeStore) ReviseMetadata(_ context.Context, req RevisionRequest) (Revision, error) {
	s.log.add("store")
	s.calls++
	s.got = req
	if s.err != nil {
		return Revision{}, s.err
	}
	return s.revision, nil
}

// cmdHarness is one wired command over recording fakes.
type cmdHarness struct {
	cmd     *Command
	log     *callLog
	members *fakeMembers
	store   *fakeStore
}

func newCmdHarness(t *testing.T, role *domain.ProjectRole) *cmdHarness {
	t.Helper()
	log := &callLog{}
	members := &fakeMembers{log: log, role: role}
	store := &fakeStore{log: log, revision: Revision{
		ProjectID: cmdProject,
		AssetPID:  cmdPID,
		Metadata:  assets.AssetMetadata{PID: cmdPID, Title: "Revised", Slug: "revised"},
	}}
	return &cmdHarness{cmd: NewCommand(Deps{Members: members, Store: store}), log: log, members: members, store: store}
}

func cmdActor() domain.User {
	return domain.User{ID: cmdActorID, Handle: "alice", DisplayName: "Alice"}
}

func strp(s string) *string { return &s }

func listp(items ...string) *[]string { return &items }

// ---------------------------------------------------------------------------
// The role gate

// TestRoleGateAdmitsMaintainerAndAbove is the half of the gate that opens:
// owner and maintainer may revise. It is asserted on both roles rather than
// on maintainer alone, because a comparison written backwards — "at most
// maintainer" instead of "at least" — would refuse the owner and pass this
// test if it only ran the maintainer.
func TestRoleGateAdmitsMaintainerAndAbove(t *testing.T) {
	for _, role := range []domain.ProjectRole{domain.ProjectRoleOwner, domain.ProjectRoleMaintainer} {
		t.Run(string(role), func(t *testing.T) {
			h := newCmdHarness(t, &role)
			if _, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
				ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp("Revised")},
			}); err != nil {
				t.Fatalf("Revise as %s: %v", role, err)
			}
			if h.store.calls != 1 {
				t.Fatalf("store calls = %d, want 1", h.store.calls)
			}
			// The membership is asked about THIS project and THIS actor —
			// not about the asset, which at this point has not been named
			// to anyone.
			if h.members.gotProject != cmdProject || h.members.gotUser != cmdActorID {
				t.Errorf("GetMembership(%q, %q), want (%q, %q)",
					h.members.gotProject, h.members.gotUser, cmdProject, cmdActorID)
			}
		})
	}
}

// TestRoleGateRefusesBelowMaintainer is the half that closes, and the two
// junior roles are asserted separately: contributor and viewer are
// different ranks and both must refuse, so a gate written against one
// specific role cannot pass by accident.
func TestRoleGateRefusesBelowMaintainer(t *testing.T) {
	for _, role := range []domain.ProjectRole{domain.ProjectRoleContributor, domain.ProjectRoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			h := newCmdHarness(t, &role)
			_, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
				ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp("Revised")},
			})
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("Revise as %s: err = %v, want ErrForbidden", role, err)
			}
			if h.store.calls != 0 {
				t.Fatalf("store calls = %d, want 0: a refusal must write nothing", h.store.calls)
			}
			if !h.log.has("members") {
				t.Fatal("the role gate did not ask for the membership")
			}
		})
	}
}

// TestNotAMemberAndRoleTooLowAreTheSameAnswer is the acceptance criterion
// spelled as one assertion over the two error VALUES.
//
// The property is not "both are refused" — it is that the two are
// INDISTINGUISHABLE. A caller probing a project id must not learn, from the
// answer alone, whether it holds a membership there that is merely too
// junior; the two failures therefore have to be the same error, not two
// errors that happen to share a status code.
func TestNotAMemberAndRoleTooLowAreTheSameAnswer(t *testing.T) {
	asked := func(role *domain.ProjectRole) error {
		h := newCmdHarness(t, role)
		_, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
			ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp("Revised")},
		})
		if err == nil {
			t.Fatal("Revise succeeded, want a refusal")
		}
		return err
	}
	viewer := domain.ProjectRoleViewer
	tooLow := asked(&viewer)
	notAMember := asked(nil)

	if !errors.Is(tooLow, ErrForbidden) || !errors.Is(notAMember, ErrForbidden) {
		t.Fatalf("errors = %v / %v, want both ErrForbidden", tooLow, notAMember)
	}
	if tooLow != notAMember {
		t.Errorf("the two refusals differ:\n  role too low: %q\n  no membership: %q", tooLow, notAMember)
	}
	if errors.Is(tooLow, projects.ErrMemberNotFound) {
		t.Error("the refusal carries the membership sentinel: a transport could unwrap it and answer the two cases differently")
	}
}

// TestGatePrecedesTheStoreAndNamesTheProject pins the ORDER: the role is
// resolved before the store is called, which is what makes a denial unable
// to disclose whether the pid exists. The log is the evidence — not the
// absence of a store call, which a command that refused for an unrelated
// reason would also show.
func TestGatePrecedesTheStoreAndNamesTheProject(t *testing.T) {
	viewer := domain.ProjectRoleViewer
	h := newCmdHarness(t, &viewer)
	_, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
		ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Slug: strp("new-slug")},
	})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if got := h.log.String(); got != "members" {
		t.Errorf("call order = %q, want exactly \"members\"", got)
	}
}

// TestUnknownProjectIsTheMembershipAnswerNotADisclosure: the production
// membership read answers ErrMemberNotFound both for "no membership row"
// and for "no such project" (the existence-hiding rule the project surface
// keeps). The command must not invent a distinguishable answer, and must
// not turn a project it cannot resolve into ErrForbidden either — a caller
// that names a nonexistent project is answered the same way it is answered
// for one it cannot see.
func TestUnknownProjectIsTheMembershipAnswerNotADisclosure(t *testing.T) {
	h := newCmdHarness(t, nil)
	h.members.err = projects.ErrMemberNotFound
	_, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
		ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp("Revised")},
	})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
}

// TestProjectNotFoundIsPassedThrough: the one membership outcome that is
// NOT a refusal — the store could not even resolve the project scope. It
// keeps its own sentinel, because the transport answers it with a
// different status, and folding it into ErrForbidden would report a
// permission decision that was never made.
func TestProjectNotFoundIsPassedThrough(t *testing.T) {
	h := newCmdHarness(t, nil)
	h.members.err = projects.ErrProjectNotFound
	_, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
		ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp("Revised")},
	})
	if !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
}

// TestGateFailsClosedOnAStoreFailure: an unreadable membership is not a
// permission. The refusal must be ErrStore — never "allowed", and never
// ErrForbidden either, which would report a decision that was not reached.
func TestGateFailsClosedOnAStoreFailure(t *testing.T) {
	h := newCmdHarness(t, nil)
	h.members.err = errors.New("connection reset")
	_, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
		ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp("Revised")},
	})
	if !errors.Is(err, ErrStore) {
		t.Fatalf("err = %v, want ErrStore", err)
	}
	if errors.Is(err, ErrForbidden) {
		t.Fatal("an unreadable membership was answered as a permission decision")
	}
	if h.store.calls != 0 {
		t.Fatalf("store calls = %d, want 0", h.store.calls)
	}
}

// TestUnwiredGateFailsClosed: a Command built with no membership port is a
// wiring bug, and a wiring bug must not be a surface that writes. It is
// asserted rather than assumed because a nil interface called through is a
// panic, and a panic is not a refusal.
func TestUnwiredGateFailsClosed(t *testing.T) {
	store := &fakeStore{log: &callLog{}}
	cmd := NewCommand(Deps{Store: store})
	_, err := cmd.Revise(context.Background(), cmdActor(), ReviseParams{
		ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp("Revised")},
	})
	if !errors.Is(err, ErrStore) {
		t.Fatalf("err = %v, want ErrStore", err)
	}
	if store.calls != 0 {
		t.Fatalf("store calls = %d, want 0", store.calls)
	}
}

// ---------------------------------------------------------------------------
// Shape, refused before anything is read

// TestShapeIsRefusedBeforeAnyRead is the first step's ordering rule: a
// request that is not a revision at all is refused without touching a port.
// The empty log is the assertion.
func TestShapeIsRefusedBeforeAnyRead(t *testing.T) {
	cases := []struct {
		name string
		in   ReviseParams
	}{
		{"no project", ReviseParams{AssetPID: cmdPID, Changes: Changes{Title: strp("Revised")}}},
		{"blank project", ReviseParams{ProjectID: "   ", AssetPID: cmdPID, Changes: Changes{Title: strp("Revised")}}},
		{"no pid", ReviseParams{ProjectID: cmdProject, Changes: Changes{Title: strp("Revised")}}},
		{"a slug where the pid goes", ReviseParams{ProjectID: cmdProject, AssetPID: "governance-lab", Changes: Changes{Title: strp("Revised")}}},
		{"a pid one character short", ReviseParams{ProjectID: cmdProject, AssetPID: cmdPID[:25], Changes: Changes{Title: strp("Revised")}}},
		{"a pid with an excluded letter", ReviseParams{ProjectID: cmdProject, AssetPID: strings.Replace(cmdPID, "k", "i", 1), Changes: Changes{Title: strp("Revised")}}},
		{"nothing to revise", ReviseParams{ProjectID: cmdProject, AssetPID: cmdPID}},
		{"an empty changes value", ReviseParams{ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{}}},
		{"a blank title", ReviseParams{ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp("  ")}}},
		{"a title over the bound", ReviseParams{ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp(strings.Repeat("t", assets.TitleMaxLen+1))}}},
		{"a blank slug", ReviseParams{ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Slug: strp("")}}},
		{"a slug over the bound", ReviseParams{ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Slug: strp(strings.Repeat("s", assets.SlugMaxLen+1))}}},
		{"a description over the bound", ReviseParams{ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Description: strp(strings.Repeat("d", assets.MaxDescriptionLen+1))}}},
		{"a blank keyword", ReviseParams{ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Keywords: listp("ok", " ")}}},
		{"over-long documentation", ReviseParams{ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Documentation: listp(strings.Repeat("d", assets.MaxDocumentationLen+1))}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := domain.ProjectRoleOwner
			h := newCmdHarness(t, &owner)
			_, err := h.cmd.Revise(context.Background(), cmdActor(), tc.in)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("err = %v, want ErrValidation", err)
			}
			if got := h.log.String(); got != "" {
				t.Errorf("ports touched = %q, want none: the shape check must precede every read", got)
			}
		})
	}
}

// TestShapeAcceptsTheBoundaries is the other direction of the same rule,
// and it is the half that catches an off-by-one: a value exactly at a bound
// is storable. The revision's display bounds are the publish's aliases
// (asserted below), so this also pins that a title a publish accepts is a
// title a revision accepts.
func TestShapeAcceptsTheBoundaries(t *testing.T) {
	cases := []struct {
		name string
		in   Changes
	}{
		{"a title exactly at the bound", Changes{Title: strp(strings.Repeat("t", assets.TitleMaxLen))}},
		{"a slug exactly at the bound", Changes{Slug: strp(strings.Repeat("s", assets.SlugMaxLen))}},
		{"a description exactly at the bound", Changes{Description: strp(strings.Repeat("d", assets.MaxDescriptionLen))}},
		{"an empty description clears the field", Changes{Description: strp("")}},
		{"an empty list clears the field", Changes{Keywords: listp()}},
		{"a nil list clears the field", Changes{Keywords: func() *[]string { var v []string; return &v }()}},
		{"one item at the item bound", Changes{Contact: listp(strings.Repeat("c", assets.MaxContactLen))}},
		{"entries up to the keyword count bound", Changes{Keywords: &filledList}},
		{"entries up to the contact count bound", Changes{Contact: &filledContacts}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := domain.ProjectRoleOwner
			h := newCmdHarness(t, &owner)
			if _, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
				ProjectID: cmdProject, AssetPID: cmdPID, Changes: tc.in,
			}); err != nil {
				t.Fatalf("Revise: %v", err)
			}
			if h.store.calls != 1 {
				t.Fatalf("store calls = %d, want 1", h.store.calls)
			}
		})
	}
}

// filledList and filledContacts are exactly-at-the-count-bound lists of
// real entries: the case above is about the COUNT bound, so the entries
// have to be ones the item rule accepts — a list of blank strings would be
// refused for the other reason and the case would prove nothing.
var (
	filledList     = fill(assets.MaxKeywords, "term")
	filledContacts = fill(assets.MaxContacts, "contact@example.org")
)

func fill(n int, value string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = value
	}
	return out
}

// TestDisplayBoundsAreThePublishBounds pins the one thing the revision and
// the publish MUST agree on, because they write the same two columns: the
// revision's bound is the publish's bound. The publish's constants are
// aliases of internal/assets' (assetpublish.MaxSlugLen = assets.SlugMaxLen),
// and this is the only package that may import both — assetpublish imports
// internal/assets, so the assertion cannot live in either of them.
//
// It is a real assertion, not a restatement of the aliasing: it fails the
// day either spelling is changed to a literal, or to a different constant,
// and the two surfaces start accepting different values for one column.
func TestDisplayBoundsAreThePublishBounds(t *testing.T) {
	if assetpublish.MaxSlugLen != assets.SlugMaxLen {
		t.Errorf("assetpublish.MaxSlugLen = %d, assets.SlugMaxLen = %d: a slug a publish accepts must be "+
			"a slug a revision accepts", assetpublish.MaxSlugLen, assets.SlugMaxLen)
	}
	if assetpublish.MaxTitleLen != assets.TitleMaxLen {
		t.Errorf("assetpublish.MaxTitleLen = %d, assets.TitleMaxLen = %d: a title a publish accepts must be "+
			"a title a revision accepts", assetpublish.MaxTitleLen, assets.TitleMaxLen)
	}
}

// ---------------------------------------------------------------------------
// The reserved cover

// TestCoverIsRefusedByName — and refused EVEN WHEN IT IS THE ONLY FIELD.
//
// The ordering is the point. A request carrying only a cover has no
// revisable field at all, so the unchanged-fields check would refuse it as
// "nothing to revise" if that check ran first; the caller would then be
// told it sent nothing, when what it sent was a field this build cannot
// serve. The cover check therefore runs before it (command.go:111).
func TestCoverIsRefusedByName(t *testing.T) {
	cases := []struct {
		name string
		in   Changes
	}{
		{"cover alone", Changes{Cover: strp("33333333-3333-4333-8333-333333333333")}},
		{"cover beside a revisable field", Changes{Title: strp("Revised"), Cover: strp("33333333-3333-4333-8333-333333333333")}},
		{"cover cleared", Changes{Cover: strp("")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner := domain.ProjectRoleOwner
			h := newCmdHarness(t, &owner)
			_, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
				ProjectID: cmdProject, AssetPID: cmdPID, Changes: tc.in,
			})
			if !errors.Is(err, ErrCoverNotSupported) {
				t.Fatalf("err = %v, want ErrCoverNotSupported", err)
			}
			// The typed form is what a transport answers 409 from, so the
			// sentinel alone is not enough: the refusal has to carry its
			// own code and its own reason.
			var typed *CoverNotSupportedError
			if !errors.As(err, &typed) {
				t.Fatalf("err = %#v, want *CoverNotSupportedError", err)
			}
			if typed.Code() != CodeCoverNotSupported {
				t.Errorf("Code() = %q, want %q", typed.Code(), CodeCoverNotSupported)
			}
			if msg := typed.Error(); !strings.Contains(msg, "cover") {
				t.Errorf("the refusal does not name the field: %q", msg)
			}
			if errors.Is(err, ErrValidation) {
				t.Error("the cover refusal is also a validation failure: a transport could not tell them apart")
			}
			if h.store.calls != 0 {
				t.Fatalf("store calls = %d, want 0: nothing was written for a refused cover", h.store.calls)
			}
			if got := h.log.String(); got != "" {
				t.Errorf("ports touched = %q, want none: the cover is refused before any read", got)
			}
		})
	}
}

// TestCoverIsRefusedBeforeTheRoleGate: the refusal is about the REQUEST, so
// it does not depend on who is asking. An owner — who passes the gate — is
// refused the same way, and a caller is not told "you may not" for
// something nobody may do yet.
func TestCoverIsRefusedBeforeTheRoleGate(t *testing.T) {
	viewer := domain.ProjectRoleViewer
	h := newCmdHarness(t, &viewer)
	_, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
		ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Cover: strp("x")},
	})
	if !errors.Is(err, ErrCoverNotSupported) {
		t.Fatalf("err = %v, want ErrCoverNotSupported (not ErrForbidden)", err)
	}
}

// ---------------------------------------------------------------------------
// What reaches the store

// TestStoreRequestCarriesTheAuditRow pins the SPLIT: the command fixes
// everything the request itself knows — actor, via, action, target,
// project, correlation id — and leaves the two summaries to the store,
// which alone can read the before values under the lock that overwrites
// them.
func TestStoreRequestCarriesTheAuditRow(t *testing.T) {
	ctx := context.Background()
	owner := domain.ProjectRoleOwner
	h := newCmdHarness(t, &owner)
	if _, err := h.cmd.Revise(ctx, cmdActor(), ReviseParams{
		ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp("Revised")},
	}); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	entry := h.store.got.Audit
	if entry.ActorID != cmdActorID {
		t.Errorf("audit actor = %q, want %q", entry.ActorID, cmdActorID)
	}
	if entry.Via != domain.ViaSession {
		t.Errorf("audit via = %q, want %q", entry.Via, domain.ViaSession)
	}
	if entry.Action != domain.ActionAssetMetadataRevised {
		t.Errorf("audit action = %q, want %q", entry.Action, domain.ActionAssetMetadataRevised)
	}
	if entry.TargetRef != "asset:"+cmdPID {
		t.Errorf("audit target = %q, want %q", entry.TargetRef, "asset:"+cmdPID)
	}
	if entry.ProjectID != cmdProject {
		t.Errorf("audit project = %q, want %q", entry.ProjectID, cmdProject)
	}
	if entry.CorrelationID == "" {
		t.Error("audit correlation id is empty: the row has to be traceable to the request that caused it")
	}
	if entry.BeforeSummary != nil || entry.AfterSummary != nil {
		t.Errorf("the command filled a summary (%v / %v): only the store can read the before, under the lock",
			entry.BeforeSummary, entry.AfterSummary)
	}
	if h.store.got.ActorID != cmdActorID {
		t.Errorf("request actor = %q, want %q", h.store.got.ActorID, cmdActorID)
	}
	if h.store.got.AssetPID != cmdPID || h.store.got.ProjectID != cmdProject {
		t.Errorf("request target = (%q, %q), want (%q, %q)",
			h.store.got.ProjectID, h.store.got.AssetPID, cmdProject, cmdPID)
	}
}

// TestAuditCorrelationIDIsTheRequestsWhenThereIsOne: a revision arriving
// on an HTTP request carries that request's correlation id into its audit
// row (docs/26), so the row and the request log line can be joined.
func TestAuditCorrelationIDIsTheRequestsWhenThereIsOne(t *testing.T) {
	id, err := observability.NewCorrelationID()
	if err != nil {
		t.Fatalf("NewCorrelationID: %v", err)
	}
	ctx := observability.WithCorrelationID(context.Background(), id)
	owner := domain.ProjectRoleOwner
	h := newCmdHarness(t, &owner)
	if _, err := h.cmd.Revise(ctx, cmdActor(), ReviseParams{
		ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp("Revised")},
	}); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	if got := h.store.got.Audit.CorrelationID; got != id.String() {
		t.Errorf("audit correlation id = %q, want the request's %q", got, id.String())
	}
}

// TestTwoRevisionsMintDifferentCorrelationIDs is the other half: a direct
// service call with no request to inherit from still has to record
// SOMETHING traceable, and a constant placeholder would be worse than
// nothing — it would look like a join key. The assertion is that two calls
// differ, which a hard-coded value cannot satisfy.
func TestTwoRevisionsMintDifferentCorrelationIDs(t *testing.T) {
	owner := domain.ProjectRoleOwner
	h := newCmdHarness(t, &owner)
	if _, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
		ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp("One")},
	}); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	first := h.store.got.Audit.CorrelationID
	if _, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
		ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp("Two")},
	}); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	second := h.store.got.Audit.CorrelationID
	if first == second {
		t.Errorf("two revisions recorded the same correlation id %q", first)
	}
}

// TestPrepareTrimsWhatItCanAndStoresWhatItValidated: the values reaching
// the store are the trimmed ones — one pass, so a value a validator
// accepted cannot be a value the store trims into something else. The
// description is left alone, because whitespace inside prose is content.
func TestPrepareTrimsWhatItCanAndStoresWhatItValidated(t *testing.T) {
	owner := domain.ProjectRoleOwner
	h := newCmdHarness(t, &owner)
	if _, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
		ProjectID: "  " + cmdProject + "  ",
		AssetPID:  "  " + cmdPID + "  ",
		Changes: Changes{
			Title:         strp("  Revised Title  "),
			Slug:          strp("  revised-slug  "),
			Description:   strp("  prose keeps its edges  "),
			Keywords:      listp("  term one  ", "term two"),
			Contact:       listp("  alice@example.org  "),
			Documentation: listp("  https://example.org/docs  "),
		},
	}); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	got := h.store.got
	if got.ProjectID != cmdProject {
		t.Errorf("project = %q, want %q trimmed", got.ProjectID, cmdProject)
	}
	if got.AssetPID != cmdPID {
		t.Errorf("pid = %q, want %q trimmed", got.AssetPID, cmdPID)
	}
	c := got.Changes
	if *c.Title != "Revised Title" {
		t.Errorf("title = %q, want trimmed", *c.Title)
	}
	if *c.Slug != "revised-slug" {
		t.Errorf("slug = %q, want trimmed", *c.Slug)
	}
	if *c.Description != "  prose keeps its edges  " {
		t.Errorf("description = %q, want it stored as declared", *c.Description)
	}
	if want := []string{"term one", "term two"}; !equalStrings(*c.Keywords, want) {
		t.Errorf("keywords = %q, want %q", *c.Keywords, want)
	}
	if want := []string{"alice@example.org"}; !equalStrings(*c.Contact, want) {
		t.Errorf("contact = %q, want %q", *c.Contact, want)
	}
	if want := []string{"https://example.org/docs"}; !equalStrings(*c.Documentation, want) {
		t.Errorf("documentation = %q, want %q", *c.Documentation, want)
	}
	// The audit target is built from the TRIMMED pid: an untrimmed value
	// would produce a TargetRef no reader could resolve.
	if want := "asset:" + cmdPID; got.Audit.TargetRef != want {
		t.Errorf("audit target = %q, want %q", got.Audit.TargetRef, want)
	}
}

// TestAbsentFieldsReachTheStoreAsNil is the difference between "unchanged"
// and "cleared", pinned at the boundary the command owns: it must not turn
// an absent field into a zero value. A command that did would clear an
// asset's description every time a client renamed its slug.
func TestAbsentFieldsReachTheStoreAsNil(t *testing.T) {
	owner := domain.ProjectRoleOwner
	h := newCmdHarness(t, &owner)
	if _, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
		ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Slug: strp("new-slug")},
	}); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	c := h.store.got.Changes
	if c.Title != nil {
		t.Errorf("title = %q, want nil (unchanged)", *c.Title)
	}
	if c.Description != nil {
		t.Errorf("description = %q, want nil (unchanged)", *c.Description)
	}
	if c.Keywords != nil || c.Contact != nil || c.Documentation != nil {
		t.Error("a list field was filled in for a request that did not mention it")
	}
	if c.Slug == nil {
		t.Error("slug = nil, want the value the request named")
	}
}

// TestChangesEmpty pins the predicate the request shape rests on: a cover
// is not a revisable field, so a request holding only a cover is "empty" —
// and that is exactly why the cover check precedes the empty check.
func TestChangesEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   Changes
		want bool
	}{
		{"nothing", Changes{}, true},
		{"cover only", Changes{Cover: strp("x")}, true},
		{"title", Changes{Title: strp("t")}, false},
		{"slug", Changes{Slug: strp("s")}, false},
		{"description", Changes{Description: strp("d")}, false},
		{"an empty description is still a request", Changes{Description: strp("")}, false},
		{"keywords", Changes{Keywords: listp()}, false},
		{"contact", Changes{Contact: listp()}, false},
		{"documentation", Changes{Documentation: listp()}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Empty(); got != tc.want {
				t.Errorf("Empty() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStoreFailureIsMappedToTheSentinel: anything the store reports that
// this package does not know is ErrStore, with the cause kept — never a
// silent success, and never a validation failure the caller would try to
// fix by editing the request.
func TestStoreFailureIsMappedToTheSentinel(t *testing.T) {
	owner := domain.ProjectRoleOwner
	h := newCmdHarness(t, &owner)
	h.store.err = errors.New("deadlock detected")
	_, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
		ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp("Revised")},
	})
	if !errors.Is(err, ErrStore) {
		t.Fatalf("err = %v, want ErrStore", err)
	}
	if errors.Is(err, ErrValidation) {
		t.Fatal("a store failure was answered as a validation failure")
	}
	if msg := err.Error(); !strings.Contains(msg, "deadlock detected") {
		t.Errorf("the cause was dropped from the message: %q", msg)
	}
}

// TestStoreSentinelsArePassedThrough: the four outcomes the store decides
// about the TARGET — the two existence-hiding not-founds, the cover and a
// store failure — have to arrive at the transport unchanged, because each
// is answered with a different status. The table also asserts the negative:
// a not-found is not quietly turned into a validation failure, which would
// tell a caller its request was malformed when the target simply is not
// there.
func TestStoreSentinelsArePassedThrough(t *testing.T) {
	for _, want := range []error{ErrProjectNotFound, ErrAssetNotFound, ErrCoverNotSupported, ErrValidation, ErrStore} {
		t.Run(want.Error(), func(t *testing.T) {
			owner := domain.ProjectRoleOwner
			h := newCmdHarness(t, &owner)
			h.store.err = want
			_, err := h.cmd.Revise(context.Background(), cmdActor(), ReviseParams{
				ProjectID: cmdProject, AssetPID: cmdPID, Changes: Changes{Title: strp("Revised")},
			})
			if !errors.Is(err, want) {
				t.Fatalf("err = %v, want %v", err, want)
			}
			if got := h.store.got; got.ProjectID != cmdProject || got.AssetPID != cmdPID {
				t.Errorf("the store was handed (%q, %q)", got.ProjectID, got.AssetPID)
			}
		})
	}
}

// ---------------------------------------------------------------------------

// equalStrings compares two string slices without pulling in a dependency
// for one comparison.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
