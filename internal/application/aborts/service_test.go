package aborts

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
)

// The service's own rules, over fakes, where each law is isolated: the ORDER
// of the steps (every refusal is resolved before any target read — the
// property that makes a denial non-disclosing), the reason-code ruling (an
// open token, checked for shape and nothing else), the two lines of the
// agent refusal, and the replay. The whole journey over real PostgreSQL,
// through the production route and merge, is
// tests/integration/abort_e2e_test.go; the wire mapping is
// cmd/api/aborthttp/handler_test.go.

const (
	svcProjectID = "11111111-2222-4333-8444-555555555555"
	svcObjectID  = "99999999-8888-4777-8666-555555555555"
	svcVersionID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	svcBranchID  = "cccccccc-dddd-4eee-8fff-000000000000"
	svcActorID   = "33333333-4444-4555-8666-777777777777"
	svcKey       = "abort-service-key-0001"
)

// stubTx is the transaction the command's write closure is handed. Nothing
// here reaches it: the fake stores do the writing, and a query reaching it
// would be a real dependency leaking into a unit test.
type stubTx struct{}

func (stubTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("stubTx: no SQL in a unit test")
}

func (stubTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("stubTx: no SQL in a unit test")
}

func (stubTx) QueryRow(context.Context, string, ...any) pgx.Row {
	return stubRow{}
}

type stubRow struct{}

func (stubRow) Scan(...any) error { return errors.New("stubTx: no SQL in a unit test") }

// countReads is every read the command could make of a target. Each one is
// counted so a test can assert that a refusal happened BEFORE all of them —
// which is what makes the refusal non-disclosing.
type countReads struct {
	membership, list, object, versionByID, version, versionByKey int
}

type fakeMembers struct {
	role   *domain.ProjectRole
	err    error
	reads  *countReads
	gotID  string
	gotAct string
}

func (f *fakeMembers) GetMembership(_ context.Context, projectID, actorID string) (domain.ProjectMembership, error) {
	f.reads.membership++
	f.gotID, f.gotAct = projectID, actorID
	if f.err != nil {
		return domain.ProjectMembership{}, f.err
	}
	m := domain.ProjectMembership{}
	if f.role != nil {
		m.Role = *f.role
	}
	return m, nil
}

// fakeAuthz answers per matrix COLUMN, not with one verdict for everybody:
// the fixture's default table is abort_main_object's own row
// (internal/authz/matrix.go:121-129), so a test that says "the actor is a
// viewer" gets the answer the real matrix gives a viewer. `forced` overrides
// the table for the tests that need a verdict no class produces.
type fakeAuthz struct {
	byClass map[authz.ActorClass]authz.Verdict
	forced  *authz.Verdict
	err     error
	calls   int
	got     authz.Request
}

func (f *fakeAuthz) Authorize(_ context.Context, req authz.Request) (authz.Decision, error) {
	f.calls++
	f.got = req
	if f.err != nil {
		return authz.Decision{}, f.err
	}
	if f.forced != nil {
		return authz.Decision{Verdict: *f.forced}, nil
	}
	verdict, ok := f.byClass[req.Class]
	if !ok {
		// An unlisted column is a denial, never an accident.
		verdict = authz.VerdictDeny
	}
	return authz.Decision{Verdict: verdict}, nil
}

// force makes the engine answer one verdict for every column.
func (f *fakeAuthz) force(v authz.Verdict) {
	f.forced = &v
}

type fakeBranches struct {
	list    []domain.Branch
	reads   *countReads
	created []branches.CreateBranchParams
	create  func(branches.CreateBranchParams) (domain.Branch, error)
}

func (f *fakeBranches) List(context.Context, string) ([]domain.Branch, error) {
	f.reads.list++
	return f.list, nil
}

func (f *fakeBranches) Create(_ context.Context, in branches.CreateBranchParams) (domain.Branch, error) {
	f.created = append(f.created, in)
	if f.create != nil {
		return f.create(in)
	}
	return domain.Branch{ID: svcBranchID, Name: in.Name, ProjectID: in.ProjectID, Lifecycle: domain.BranchLifecycleActive}, nil
}

type fakePRs struct {
	reads   *countReads
	created []pullrequests.CreatePullRequestParams
	err     error
	byKey   *domain.PullRequest
	// foreign makes Create answer the way the adapter's per-project
	// creation-key replay does when the key was used for ANOTHER object: it
	// hands back a proposal that is not on the branch this call forked.
	// Set by the borrowed-key test.
	foreign bool
}

func (f *fakePRs) Create(_ context.Context, in pullrequests.CreatePullRequestParams) (domain.PullRequest, error) {
	f.created = append(f.created, in)
	if f.err != nil {
		return domain.PullRequest{}, f.err
	}
	if f.foreign {
		return domain.PullRequest{
			ID: "pr-other", Number: 9, ProjectID: in.ProjectID, State: domain.PullRequestStateOpen,
			SourceBranchID: svcBranchID + "-another-object", TargetBranchID: in.TargetBranchID,
		}, nil
	}
	return domain.PullRequest{
		ID: "pr-1", Number: 7, ProjectID: in.ProjectID, State: domain.PullRequestStateOpen,
		SourceBranchID: in.SourceBranchID, TargetBranchID: in.TargetBranchID,
	}, nil
}

func (f *fakePRs) GetByCreationKey(context.Context, string, string) (domain.PullRequest, error) {
	f.reads.versionByKey++
	if f.byKey == nil {
		return domain.PullRequest{}, pullrequests.ErrPullRequestNotFound
	}
	return *f.byKey, nil
}

type fakeCommits struct {
	reads  *countReads
	calls  int
	params states.CommitParams
	err    error
}

func (f *fakeCommits) Commit(ctx context.Context, in states.CommitParams, write states.WriteFunc) (domain.ProjectState, domain.StateCommit, error) {
	f.calls++
	f.params = in
	if f.err != nil {
		return domain.ProjectState{}, domain.StateCommit{}, f.err
	}
	if err := write(ctx, stubTx{}, "state-proposal"); err != nil {
		return domain.ProjectState{}, domain.StateCommit{}, err
	}
	return domain.ProjectState{ID: "state-proposal"}, domain.StateCommit{ID: "commit-1"}, nil
}

type fakeEvents struct {
	recorded []events.Event
	err      error
}

func (f *fakeEvents) Record(_ context.Context, _ events.DBTX, e events.Event) error {
	f.recorded = append(f.recorded, e)
	return f.err
}

type fakeObjects struct {
	reads      *countReads
	object     domain.ScientificObject
	objectErr  error
	version    domain.ScientificObjectVersion
	recorded   *domain.ScientificObjectVersion
	byKey      *domain.ScientificObjectVersion
	writeErr   error
	appendCall int
	params     AbortWriteParams
	// trackAppendedKey makes the version-key index answer with the row the
	// append wrote, from the moment it is written — the state the store's
	// own index reaches once the row is committed. Set (with
	// fakePRs.foreign) by the borrowed-key test, which needs the key to
	// name nothing when the request starts and to name this object's own
	// version by the time the proposal is refused.
	trackAppendedKey bool
	byKeyAfterAppend *domain.ScientificObjectVersion
}

func (f *fakeObjects) GetObject(context.Context, string) (domain.ScientificObject, error) {
	f.reads.object++
	if f.objectErr != nil {
		return domain.ScientificObject{}, f.objectErr
	}
	return f.object, nil
}

func (f *fakeObjects) GetVersionByID(context.Context, string) (domain.ScientificObjectVersion, error) {
	f.reads.versionByID++
	if f.version.ID == "" {
		return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
	}
	return f.version, nil
}

func (f *fakeObjects) GetVersion(context.Context, string, int) (domain.ScientificObjectVersion, error) {
	f.reads.version++
	if f.version.ID == "" {
		return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
	}
	return f.version, nil
}

func (f *fakeObjects) GetVersionByAbortRequestKey(context.Context, string, string) (domain.ScientificObjectVersion, error) {
	f.reads.versionByKey++
	if f.byKey != nil {
		return *f.byKey, nil
	}
	if f.byKeyAfterAppend != nil {
		return *f.byKeyAfterAppend, nil
	}
	return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
}

func (f *fakeObjects) AppendAbortVersionInTx(_ context.Context, _ states.Transaction, in AbortWriteParams) (domain.ScientificObjectVersion, error) {
	f.appendCall++
	f.params = in
	if f.writeErr != nil {
		return domain.ScientificObjectVersion{}, f.writeErr
	}
	out := domain.ScientificObjectVersion{
		ID: "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff", ObjectID: in.ObjectID,
		VersionNo: in.ExpectedVersionNo + 1, LifecycleState: domain.LifecycleAborted,
		Payload: in.Version.Payload, Abort: in.Version.Abort,
		BranchID: in.Version.BranchID,
	}
	f.recorded = &out
	if f.trackAppendedKey {
		// From here on the key names this row, as the store's index does
		// once the append has committed.
		f.byKeyAfterAppend = &out
	}
	return out, nil
}

// fixture is a fully wired command over the fakes.
type fixture struct {
	svc      *Service
	reads    *countReads
	members  *fakeMembers
	authz    *fakeAuthz
	branches *fakeBranches
	prs      *fakePRs
	commits  *fakeCommits
	events   *fakeEvents
	objects  *fakeObjects
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	reads := &countReads{}
	owner := domain.ProjectRoleOwner
	f := &fixture{
		reads:   reads,
		members: &fakeMembers{role: &owner, reads: reads},
		// abort_main_object's own row, class by class.
		authz: &fakeAuthz{byClass: map[authz.ActorClass]authz.Verdict{
			authz.ActorPublicAnonymous:        authz.VerdictDeny,
			authz.ActorAuthenticatedNonMember: authz.VerdictDeny,
			authz.ActorViewer:                 authz.VerdictDeny,
			authz.ActorContributor:            authz.VerdictDeny,
			authz.ActorMaintainer:             authz.VerdictViaPR,
			authz.ActorOwner:                  authz.VerdictViaPR,
			authz.ActorAgent:                  authz.VerdictProposalOnly,
		}},
		branches: &fakeBranches{reads: reads},
		prs:      &fakePRs{reads: reads},
		commits:  &fakeCommits{reads: reads},
		events:   &fakeEvents{},
		objects:  &fakeObjects{reads: reads},
	}
	mainState := "state-main"
	f.branches.list = []domain.Branch{{
		ID: svcBranchID + "-main", Name: "main", ProjectID: svcProjectID,
		Lifecycle: domain.BranchLifecycleActive, BaseStateID: &mainState,
	}}
	f.objects.object = domain.ScientificObject{
		ID: svcObjectID, ProjectID: svcProjectID, ObjectType: "claim", CurrentVersionNo: 1,
	}
	mainID := svcBranchID + "-main"
	f.objects.version = domain.ScientificObjectVersion{
		ID: svcVersionID, ObjectID: svcObjectID, VersionNo: 1, BranchID: &mainID,
		LifecycleState: domain.LifecycleActive, SchemaID: "claim", SchemaVersion: "1",
		Title: "a claim", Payload: []byte(`{"statement":"a claim"}`),
	}
	f.svc = NewService(Deps{
		Members: f.members, Authz: f.authz, Objects: f.objects, Branches: f.branches,
		PullRequests: f.prs, Commits: f.commits, Events: f.events,
	})
	f.svc.now = func() time.Time { return time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC) }
	return f
}

func rolePtr(r domain.ProjectRole) *domain.ProjectRole { return &r }

func (f *fixture) actor() Actor { return Actor{User: domain.User{ID: svcActorID}} }

func (f *fixture) input() Input {
	return Input{
		ProjectID: svcProjectID, ObjectID: svcObjectID, ObjectVersionRef: svcVersionID,
		ReasonCode:     "superseded_by_better_evidence",
		Explanation:    "the 2026-08-14 re-run contradicts this claim",
		IdempotencyKey: svcKey,
	}
}

// requireNoReads asserts that not one target was read: every counter is zero.
// A refusal that passes this is a refusal that cannot disclose whether the
// project or the object exists.
func (f *fixture) requireNoReads(t *testing.T, what string) {
	t.Helper()
	r := f.reads
	if r.membership != 0 || r.list != 0 || r.object != 0 ||
		r.versionByID != 0 || r.version != 0 || r.versionByKey != 0 {
		t.Fatalf("%s read something before refusing: %+v", what, *r)
	}
}

// TestAbortShapeIsCheckedBeforeAnythingIsRead: a malformed request is refused
// without reading, and each shape rule is its own refusal.
func TestAbortShapeIsCheckedBeforeAnythingIsRead(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Input)
		want   string
	}{
		{"no project", func(in *Input) { in.ProjectID = "" }, "project_id is required"},
		{"project not a uuid", func(in *Input) { in.ProjectID = "not-a-uuid" }, "project_id must be a uuid"},
		{"object not a uuid", func(in *Input) { in.ObjectID = "abc" }, "object_id must name a scientific object"},
		{"version ref not a uuid", func(in *Input) { in.ObjectVersionRef = "object_version:x" }, "object_version_ref must name an object version"},
		{"no reason code", func(in *Input) { in.ReasonCode = "" }, "reason_code must be 1..64 characters"},
		{"reason code with a space", func(in *Input) { in.ReasonCode = "superseded by evidence" }, "reason_code must be 1..64 characters"},
		{"reason code with uppercase", func(in *Input) { in.ReasonCode = "Superseded" }, "reason_code must be 1..64 characters"},
		{"reason code too long", func(in *Input) { in.ReasonCode = strings.Repeat("a", 65) }, "reason_code must be 1..64 characters"},
		{"no explanation", func(in *Input) { in.Explanation = "   " }, "explanation is required"},
		{"explanation too long", func(in *Input) { in.Explanation = strings.Repeat("x", MaxExplanationLen+1) }, "explanation must be at most"},
		{"replacement ref too long", func(in *Input) {
			in.Explanation = "ok"
			in.ReplacementRef = strings.Repeat("x", MaxReplacementRefLen+1)
		}, "replacement_ref must be at most"},
		{"no key", func(in *Input) { in.IdempotencyKey = "" }, "Idempotency-Key is required"},
		{"key too short", func(in *Input) { in.IdempotencyKey = "short" }, "Idempotency-Key must be at least"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			in := f.input()
			tc.mutate(&in)
			_, err := f.svc.AbortProposal(context.Background(), f.actor(), in)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("abort = %v, want ErrValidation", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal says %q, want it to mention %q", err, tc.want)
			}
			f.requireNoReads(t, "the shape check")
		})
	}
}

// TestAbortReasonCodeIsAnOpenToken: the shape is enforced and the VALUE is
// not. Nothing in this build enumerates reason codes and nothing here
// narrows them: a token no vocabulary would contain is accepted and stored
// as given, which is the ruling this task carries (V1 cannot break aborts
// down by reason; that cost is named in the task result, not paid for by
// guessing a list).
func TestAbortReasonCodeIsAnOpenToken(t *testing.T) {
	for _, code := range []string{
		"superseded_by_better_evidence",
		"covid_19_reanalysis_2026",
		"zzz",
		strings.Repeat("a", 64),
	} {
		t.Run(code, func(t *testing.T) {
			f := newFixture(t)
			in := f.input()
			in.ReasonCode = code
			if _, err := f.svc.AbortProposal(context.Background(), f.actor(), in); err != nil {
				t.Fatalf("abort with reason code %q = %v, want it accepted (the token is the caller's)", code, err)
			}
			if f.objects.params.Version.Abort == nil {
				t.Fatal("the write carried no abort record")
			}
			if got := f.objects.params.Version.Abort.ReasonCode; got != code {
				t.Fatalf("the stored reason code is %q, want the caller's %q stored as given", got, code)
			}
		})
	}
}

// TestAbortRefusalsPrecedeEveryRead is the ordering law for the two refusals
// that are not about the request's shape: an agent, and a class the matrix
// does not admit. Both must be resolved before a single target read, or the
// refusal would disclose whether the project and the object exist.
func TestAbortRefusalsPrecedeEveryRead(t *testing.T) {
	t.Run("an agent", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.svc.AbortProposal(context.Background(), Actor{User: domain.User{ID: svcActorID}, IsAgent: true}, f.input())
		var refused *AgentNotPermittedError
		if !errors.As(err, &refused) {
			t.Fatalf("abort as an agent = %v, want *AgentNotPermittedError", err)
		}
		f.requireNoReads(t, "the agent backstop")
		if f.authz.calls != 0 {
			t.Fatal("the agent backstop consulted the matrix; it must refuse on the actor value alone, before any dependency")
		}
		if f.commits.calls != 0 || f.objects.appendCall != 0 || len(f.events.recorded) != 0 {
			t.Fatal("a refused agent wrote something")
		}
	})

	t.Run("a class the matrix denies", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			role    *domain.ProjectRole
			verdict authz.Verdict
		}{
			// The four deny cells, as the matrix's row spells them.
			{"a non-member", nil, authz.VerdictDeny},
			{"a viewer", rolePtr(domain.ProjectRoleViewer), authz.VerdictDeny},
			{"a contributor", rolePtr(domain.ProjectRoleContributor), authz.VerdictDeny},
			// And the conditional the matrix does NOT resolve here: an
			// agent's `proposal_only` is a refusal for this command
			// whatever the class, which is the matrix's own line of the
			// agent defence (the backstop is the other one, exercised
			// above). Forcing the verdict is how a non-agent reaches this
			// branch: the agent CELL is unreachable behind the backstop,
			// and this proves the second line refuses independently of it.
			{"a class whose cell is proposal_only", rolePtr(domain.ProjectRoleOwner), authz.VerdictProposalOnly},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newFixture(t)
				f.members.role = tc.role
				f.authz.force(tc.verdict)
				_, err := f.svc.AbortProposal(context.Background(), f.actor(), f.input())
				if !errors.Is(err, ErrForbidden) {
					t.Fatalf("the %s verdict = %v, want ErrForbidden", tc.verdict, err)
				}
				// The membership read is the ONE read a refusal makes: it is
				// what the decision is made FROM, and it answers the same for
				// an unknown project as for a stranger.
				if f.reads.membership != 1 {
					t.Fatalf("the membership was resolved %d times, want 1", f.reads.membership)
				}
				if f.reads.object != 0 || f.reads.versionByID != 0 || f.reads.versionByKey != 0 || f.reads.list != 0 {
					t.Fatalf("the refusal read the target: %+v", *f.reads)
				}
				if f.commits.calls != 0 || len(f.events.recorded) != 0 {
					t.Fatal("a refused request wrote something")
				}
			})
		}
	})

	t.Run("an actor with no principal", func(t *testing.T) {
		f := newFixture(t)
		_, err := f.svc.AbortProposal(context.Background(), Actor{}, f.input())
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("an actor-less abort = %v, want ErrForbidden (the matrix's anonymous cell)", err)
		}
		f.requireNoReads(t, "the actor check")
	})

	t.Run("an unknown project", func(t *testing.T) {
		f := newFixture(t)
		f.members.err = projects.ErrProjectNotFound
		_, err := f.svc.AbortProposal(context.Background(), f.actor(), f.input())
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("an abort in an unknown project = %v, want ErrForbidden", err)
		}
		// The class the command decides for an unknown project is the
		// non-member class, and the matrix denies it — the caller cannot
		// tell "no such project" from "you are not in it".
		if f.authz.got.Class != authz.ActorAuthenticatedNonMember {
			t.Fatalf("the decision was made for class %q, want the non-member class", f.authz.got.Class)
		}
	})
}

// TestAbortAcceptsTheMatrixsConditionalCells: `via_pr` is PERMITTED — the
// command is the resolution of that condition, because it creates a proposal
// and never writes main — and the two cells the matrix names for this action
// (maintainer and owner) both carry it.
func TestAbortAcceptsTheMatrixsConditionalCells(t *testing.T) {
	for _, tc := range []struct {
		name string
		role domain.ProjectRole
	}{
		{"maintainer", domain.ProjectRoleMaintainer},
		{"owner", domain.ProjectRoleOwner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			role := tc.role
			f.members.role = &role
			res, err := f.svc.AbortProposal(context.Background(), f.actor(), f.input())
			if err != nil {
				t.Fatalf("abort as the %s = %v, want the proposal", tc.name, err)
			}
			if f.authz.got.Class != authz.ClassOf(true, &role, false) {
				t.Fatalf("the decision was made for class %q", f.authz.got.Class)
			}
			if res.PullRequestNumber == 0 {
				t.Fatalf("the %s got no proposal: %+v", tc.name, res)
			}
			// A proposal, not an abort: the version the command wrote is on
			// a FORKED branch, never on main.
			if f.objects.params.Version.BranchID == nil || *f.objects.params.Version.BranchID == f.branches.list[0].ID {
				t.Fatalf("the version was written to the main line: %v", f.objects.params.Version.BranchID)
			}
		})
	}
}

// TestAbortWritesTheRecordTheCommitAndTheEventTogether: docs/46:7's five
// fields plus the actor and the time reach the version row, the audit row is
// written with the version, the event is recorded in the same write closure,
// and the two conditional fields (a replacement ref that was not given, the
// key) are carried as the caller gave them.
func TestAbortWritesTheRecordTheCommitAndTheEventTogether(t *testing.T) {
	f := newFixture(t)
	in := f.input()
	in.ReplacementRef = "object_version:11111111-1111-4111-8111-111111111111"
	res, err := f.svc.AbortProposal(context.Background(), f.actor(), in)
	if err != nil {
		t.Fatalf("abort = %v", err)
	}
	w := f.objects.params
	if w.Version.Abort == nil {
		t.Fatal("the version row carried no abort record")
	}
	rec := w.Version.Abort
	if rec.ReasonCode != in.ReasonCode || rec.Explanation != in.Explanation || rec.ReplacementRef != in.ReplacementRef {
		t.Fatalf("the record is %+v, want the caller's", rec)
	}
	if rec.DecidedBy != svcActorID {
		t.Fatalf("the record's actor is %q, want the aborting principal", rec.DecidedBy)
	}
	if !rec.DecidedAt.Equal(time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("the record's time is %v, want the service clock's", rec.DecidedAt)
	}
	if w.Version.AbortRequestKey != svcKey {
		t.Fatalf("the version row carries key %q, want the request's %q", w.Version.AbortRequestKey, svcKey)
	}
	if w.ExpectedVersionNo != 1 {
		t.Fatalf("the append compares against version %d, want the log's head 1", w.ExpectedVersionNo)
	}
	if w.Audit.Action != domain.ActionScientificObjectAborted || w.Audit.ActorID != svcActorID {
		t.Fatalf("the audit entry is %+v", w.Audit)
	}
	after, ok := w.Audit.AfterSummary.(map[string]any)
	if !ok {
		t.Fatalf("the audit entry's after summary is %T, want a JSON object", w.Audit.AfterSummary)
	}
	if after["reason_code"] != in.ReasonCode || after["explanation"] != in.Explanation {
		t.Fatalf("the audit entry lost the record: %+v", after)
	}
	if len(f.events.recorded) != 1 {
		t.Fatalf("%d events were recorded, want 1", len(f.events.recorded))
	}
	evt := f.events.recorded[0]
	if evt.EventType != "scientific_object.aborted" {
		t.Fatalf("the event type is %q, want the registered scientific_object.aborted", evt.EventType)
	}
	if evt.ProjectID != svcProjectID || evt.ActorID != svcActorID {
		t.Fatalf("the event names project %q actor %q", evt.ProjectID, evt.ActorID)
	}
	// The event payload carries identity and reference only: the free-text
	// explanation lives in the two rows (the version and the audit), not in
	// the fan-out.
	if strings.Contains(string(evt.Payload), "re-run contradicts") {
		t.Fatalf("the event payload carries the human explanation: %s", evt.Payload)
	}
	if !strings.Contains(string(evt.Payload), in.ReasonCode) {
		t.Fatalf("the event payload does not carry the reason code: %s", evt.Payload)
	}
	if res.VersionID != f.objects.recorded.ID || res.LifecycleState != string(domain.LifecycleAborted) {
		t.Fatalf("the result is %+v", res)
	}
	if res.Replayed {
		t.Fatal("a fresh proposal reported itself as a replay")
	}
	if f.reads.object != 1 || f.reads.versionByID != 1 {
		t.Fatalf("the fresh path read the target %+v", *f.reads)
	}
}

// TestAbortKeepsTheAbortedVersionUntouched: the append copies the aborted
// version's content and moves only the lifecycle. The command must not send
// the store anything that could be mistaken for an edit of the older row.
func TestAbortKeepsTheAbortedVersionUntouched(t *testing.T) {
	f := newFixture(t)
	original := f.objects.version.Payload
	if _, err := f.svc.AbortProposal(context.Background(), f.actor(), f.input()); err != nil {
		t.Fatalf("abort = %v", err)
	}
	if string(f.objects.params.Version.Payload) != string(original) {
		t.Fatalf("the appended version's payload is %s, want the aborted version's %s", f.objects.params.Version.Payload, original)
	}
	if f.objects.version.LifecycleState != domain.LifecycleActive || f.objects.version.VersionNo != 1 {
		t.Fatalf("the command edited the aborted version in place: %+v", f.objects.version)
	}
	if !strings.EqualFold(f.objects.version.ID, svcVersionID) {
		t.Fatalf("the aborted version is no longer the one the request named: %s", f.objects.version.ID)
	}
}

// TestAbortReplaysOnTheKeyAlone: a repeated request carrying a key a version
// already records answers with what was recorded and writes NOTHING — no
// second version, no second audit row, no second event, no second PR. The
// state is the idempotency record; there is no ledger.
func TestAbortReplaysOnTheKeyAlone(t *testing.T) {
	f := newFixture(t)
	// The recorded row carries the branch its proposal was forked from and
	// the proposal sits on it — the shape the store writes, and the shape
	// the replay's proposal check reads (a row that recorded no branch
	// names no proposal, fail-closed).
	branchID := svcBranchID
	recorded := domain.ScientificObjectVersion{
		ID: "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff", ObjectID: svcObjectID, VersionNo: 2,
		LifecycleState: domain.LifecycleAborted, BranchID: &branchID,
		Abort: &domain.AbortRecord{
			ReasonCode: "superseded_by_better_evidence", Explanation: "the 2026-08-14 re-run contradicts this claim",
			DecidedBy: svcActorID, DecidedAt: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC),
		},
	}
	f.objects.byKey = &recorded
	f.prs.byKey = &domain.PullRequest{
		ID: "pr-1", Number: 7, ProjectID: svcProjectID, State: domain.PullRequestStateOpen,
		SourceBranchID: branchID,
	}
	in := f.input()
	// The replayed request names a version that is aborted by now: the
	// fresh path would refuse it, and the replay must answer anyway — that
	// is the difference the step order buys.
	in.ObjectVersionRef = svcVersionID

	res, err := f.svc.AbortProposal(context.Background(), f.actor(), in)
	if err != nil {
		t.Fatalf("the replay = %v", err)
	}
	if !res.Replayed {
		t.Fatal("the repeat did not report itself as a replay")
	}
	if res.VersionID != recorded.ID || res.VersionNo != recorded.VersionNo {
		t.Fatalf("the replay answered version %s v%d, want the recorded %s v%d", res.VersionID, res.VersionNo, recorded.ID, recorded.VersionNo)
	}
	if res.PullRequestNumber != 7 {
		t.Fatalf("the replay answered PR %d, want the proposal's 7", res.PullRequestNumber)
	}
	if res.ReasonCode != recorded.Abort.ReasonCode || res.Explanation != recorded.Abort.Explanation ||
		res.DecidedBy != recorded.Abort.DecidedBy {
		t.Fatalf("the replay does not report the recorded record: %+v", res)
	}
	if res.AbortedVersionID != svcVersionID {
		t.Fatalf("the replay names the aborted version as %q, want the row one number below the recorded one", res.AbortedVersionID)
	}
	if f.objects.appendCall != 0 || f.commits.calls != 0 || len(f.prs.created) != 0 || len(f.events.recorded) != 0 {
		t.Fatalf("the replay wrote something: append=%d commit=%d prs=%d events=%d",
			f.objects.appendCall, f.commits.calls, len(f.prs.created), len(f.events.recorded))
	}
}

// TestAbortRefusesAProposalAnotherObjectHolds: the creation key's index is
// per project (migration 00089) while the abort's key index is per object
// (00100), so one key sent for two objects in one project comes back from
// the adapter carrying the FIRST object's proposal. The command must refuse
// that row rather than report it: the version the second request just
// committed records this object's abort, and nothing proposes it.
//
// The fake is set up so the refusal cannot hide behind a missing read: the
// key names nothing when the request starts, and names this object's own
// version from the moment the append commits — which is precisely the state
// the replay fallback reads. A refusal is still what must come out, because
// the append above wrote this object's version under this very key and a
// replay of it would answer 201 for a request that was refused.
func TestAbortRefusesAProposalAnotherObjectHolds(t *testing.T) {
	f := newFixture(t)
	f.prs.foreign = true
	f.objects.trackAppendedKey = true

	res, err := f.svc.AbortProposal(context.Background(), f.actor(), f.input())
	if err == nil {
		t.Fatalf("the borrowed key answered %+v, want a refusal", res)
	}
	if !errors.Is(err, ErrIdempotencyKeyInUse) {
		t.Fatalf("the refusal = %v, want ErrIdempotencyKeyInUse", err)
	}
	var coded *IdempotencyKeyInUseError
	if !errors.As(err, &coded) {
		t.Fatalf("the refusal is %T, want the coded *IdempotencyKeyInUseError", err)
	}
	if coded.Code() != CodeConflict {
		t.Fatalf("the refusal's wire code is %q, want the conflict class %q", coded.Code(), CodeConflict)
	}
	// The sentence must not turn the refusal into a read of the other
	// object's record: no proposal, no branch, no object of somebody
	// else's is named in it.
	for _, leak := range []string{"pr-other", svcBranchID + "-another-object", "another object", "Number: 9"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("the refusal names what holds the key (%q): %s", leak, err.Error())
		}
	}
	if res.VersionID != "" || res.PullRequestNumber != 0 || res.BranchID != "" {
		t.Fatalf("the refused request reported a proposal: %+v", res)
	}
	// The residue, recorded rather than blessed: the refusal is decided
	// after the version commit (nothing was added ahead of the write, so
	// the append's compare-and-swap is still the single winner), which
	// leaves this object with an aborted version on a branch no PR
	// proposes. The fake proves the shape: the append ran, the creation was
	// attempted, and no proposal came back.
	if f.objects.appendCall != 1 || len(f.prs.created) != 1 {
		t.Fatalf("the refused request wrote append=%d creates=%d, want the version committed and the creation attempted",
			f.objects.appendCall, len(f.prs.created))
	}
	if f.objects.recorded == nil || f.objects.recorded.LifecycleState != domain.LifecycleAborted {
		t.Fatalf("the residue is %+v, want the aborted version the refusal left behind", f.objects.recorded)
	}
}

// TestAbortProposalBranchNameIsDerivedFromTheKey: two requests with one key
// derive ONE branch name, which is what turns a race into a name collision
// (adopted) rather than two proposals; two keys derive two.
func TestAbortProposalBranchNameIsDerivedFromTheKey(t *testing.T) {
	a := proposalBranchName(svcObjectID, "abort-key-aaaaaaaa")
	b := proposalBranchName(svcObjectID, "abort-key-aaaaaaaa")
	c := proposalBranchName(svcObjectID, "abort-key-bbbbbbbb")
	if a != b {
		t.Fatalf("one key derived two names: %q vs %q", a, b)
	}
	if a == c {
		t.Fatal("two keys derived the same name: two proposals for one object would collide")
	}
	if !strings.HasPrefix(a, "abort/"+svcObjectID+"-") {
		t.Fatalf("the name %q does not name the object it is about", a)
	}
}

// TestAbortAdoptsTheBranchAConcurrentRequestForked: the fork is the first
// write, so two identical requests race there. The loser must adopt the
// branch the winner forked rather than fail — the name was derived from the
// key, so a taken name means this key already forked it.
func TestAbortAdoptsTheBranchAConcurrentRequestForked(t *testing.T) {
	f := newFixture(t)
	mine := domain.Branch{ID: svcBranchID, Name: proposalBranchName(svcObjectID, svcKey), ProjectID: svcProjectID, Lifecycle: domain.BranchLifecycleActive}
	f.branches.create = func(branches.CreateBranchParams) (domain.Branch, error) {
		return domain.Branch{}, branches.ErrBranchNameTaken
	}
	f.branches.list = append(f.branches.list, mine)

	res, err := f.svc.AbortProposal(context.Background(), f.actor(), f.input())
	if err != nil {
		t.Fatalf("adopting the forked branch = %v", err)
	}
	if res.BranchID != svcBranchID {
		t.Fatalf("the proposal names branch %q, want the adopted %q", res.BranchID, svcBranchID)
	}
	if f.objects.params.Version.BranchID == nil || *f.objects.params.Version.BranchID != svcBranchID {
		t.Fatalf("the version was written to %v, want the adopted branch", f.objects.params.Version.BranchID)
	}
}

// TestAbortReplaysWhenTheWriteLosesARace: the winner's transaction committed
// between this request's idempotency read and its append. The append reports
// a conflict, and the command must re-read the key and answer with the
// winner's recorded proposal rather than reporting a failure for a request
// that has, in fact, succeeded.
func TestAbortReplaysWhenTheWriteLosesARace(t *testing.T) {
	f := newFixture(t)
	f.objects.writeErr = &sciobjects.VersionConflictError{}
	// The winner committed: by the time the loser re-reads, the key names a
	// version.
	winner := domain.ScientificObjectVersion{
		ID: "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff", ObjectID: svcObjectID, VersionNo: 2,
		LifecycleState: domain.LifecycleAborted,
		Abort: &domain.AbortRecord{ReasonCode: "superseded_by_better_evidence", Explanation: "the winner's words",
			DecidedBy: svcActorID, DecidedAt: time.Date(2026, 9, 19, 12, 0, 1, 0, time.UTC)},
	}
	f.objects.byKey = &winner
	f.prs.byKey = &domain.PullRequest{ID: "pr-1", Number: 7, ProjectID: svcProjectID, State: domain.PullRequestStateOpen}

	res, err := f.svc.AbortProposal(context.Background(), f.actor(), f.input())
	if err != nil {
		t.Fatalf("the loser of the race = %v, want the winner's proposal", err)
	}
	if !res.Replayed || res.VersionID != winner.ID {
		t.Fatalf("the loser answered %+v, want the winner's recorded version %s", res, winner.ID)
	}
	if res.Explanation != "the winner's words" {
		t.Fatalf("the loser reported its OWN words (%q), not the recorded ones", res.Explanation)
	}
	if len(f.prs.created) != 0 {
		t.Fatal("the loser opened a pull request")
	}
}

// TestAbortReportsAConflictWhenTheKeyNamesNothing: the same lost race, but
// the winner has not committed yet — the key still names nothing, so the
// request neither won nor replayed and must say so rather than pretend.
func TestAbortReportsAConflictWhenTheKeyNamesNothing(t *testing.T) {
	f := newFixture(t)
	f.objects.writeErr = &sciobjects.VersionConflictError{}
	_, err := f.svc.AbortProposal(context.Background(), f.actor(), f.input())
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a lost race with no recorded winner = %v, want ErrConflict", err)
	}
}

// TestAbortOpensNoProposalWhenTheWriteFails: the version, the audit row and
// the event are one transaction, so a write that fails inside it must leave
// no PR behind — a proposal whose branch carries nothing would be a
// governance record of an abort that did not happen.
func TestAbortOpensNoProposalWhenTheWriteFails(t *testing.T) {
	f := newFixture(t)
	f.objects.writeErr = errors.New("insert scientific object version: connection reset")
	_, err := f.svc.AbortProposal(context.Background(), f.actor(), f.input())
	if !errors.Is(err, ErrStore) {
		t.Fatalf("a failed write = %v, want ErrStore", err)
	}
	if len(f.prs.created) != 0 {
		t.Fatal("a proposal was opened for a write that did not commit")
	}
	if len(f.events.recorded) != 0 {
		t.Fatal("the event was recorded although the transaction failed")
	}
}

// TestAbortRefusesAVersionThatIsNotAbortable: only the object's CURRENT
// version, in lifecycle 'active', on the main line may be the subject of an
// abort. Everything else is refused with the version-not-found outcome, so a
// caller cannot use the refusal to map the object's history.
func TestAbortRefusesAVersionThatIsNotAbortable(t *testing.T) {
	otherBranch := svcBranchID + "-feature"
	for _, tc := range []struct {
		name   string
		mutate func(*fakeObjects)
		want   error
	}{
		{"a version of another object", func(o *fakeObjects) { o.version.ObjectID = "44444444-5555-4666-8777-888888888888" }, ErrVersionNotFound},
		{"a superseded version", func(o *fakeObjects) { o.object.CurrentVersionNo = 2 }, ErrVersionNotFound},
		{"an already aborted version", func(o *fakeObjects) { o.version.LifecycleState = domain.LifecycleAborted }, ErrVersionNotFound},
		{"a version on another branch", func(o *fakeObjects) { o.version.BranchID = &otherBranch }, ErrNotMainObject},
		{"a version with no branch", func(o *fakeObjects) { o.version.BranchID = nil }, ErrNotMainObject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.mutate(f.objects)
			if _, err := f.svc.AbortProposal(context.Background(), f.actor(), f.input()); !errors.Is(err, tc.want) {
				t.Fatalf("the abort = %v, want %v", err, tc.want)
			}
			if f.objects.appendCall != 0 || f.commits.calls != 0 {
				t.Fatal("a refused subject was written anyway")
			}
		})
	}
}

// TestAbortRefusesAnObjectOfAnotherProject: an object of a foreign project
// answers exactly as an unknown one, so the outcome cannot be used to probe
// for another project's entities.
func TestAbortRefusesAnObjectOfAnotherProject(t *testing.T) {
	f := newFixture(t)
	f.objects.object.ProjectID = "44444444-5555-4666-8777-888888888888"
	_, err := f.svc.AbortProposal(context.Background(), f.actor(), f.input())
	if !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("an object of another project = %v, want ErrObjectNotFound", err)
	}
}

// TestAbortRefusesAnUnwiredCommand: a half-wired command refuses rather than
// panicking or writing part of the abort.
func TestAbortRefusesAnUnwiredCommand(t *testing.T) {
	svc := NewService(Deps{})
	if _, err := svc.AbortProposal(context.Background(), Actor{User: domain.User{ID: svcActorID}}, Input{
		ProjectID: svcProjectID, ObjectID: svcObjectID, ObjectVersionRef: svcVersionID,
		ReasonCode: "superseded", Explanation: "x", IdempotencyKey: svcKey,
	}); !errors.Is(err, ErrStore) {
		t.Fatalf("an unwired command = %v, want ErrStore", err)
	}
}
