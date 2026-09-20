// Task T0613 required test "activity feed audience e2e".
//
// docs/adr/ADR-024 (the third read outlet of the same rule): a research
// event's `visibility` column says whether the PUBLIC read may render the
// row, and the Activity feed's research branch carried no reader at all.
// The route's gate answers "may this reader see this PROJECT" —
// cmd/api/main.go hands the Activity surface the projects read gate with the
// comment "so a resource's activity is exactly as visible as the resource
// itself" — and a PUBLIC project's read is allowed for every matrix class
// (specs/policies/permissions-matrix.csv, read_public_project). So any
// signed-in reader passes the gate on a public project, and then every
// research_events row of that project was rendered to whoever passed it,
// `e.visibility` being selected as data rather than enforced.
//
// The write path derives those rows: internal/application/rsg/events.go
// stores an event as `private` when its SUBJECT — the branch it committed
// to — is private ("an event is never more visible than its subject"), and
// the branch's visibility is explicitly chosen by its creator
// (POST /branches, infra/migrations/00004_rsg_state.sql's CHECK). A private
// branch inside a public project is a legal shape, so private events reach
// a public project's feed by ordinary use.
//
// The rule this suite pins, over real PostgreSQL and the whole composed
// path:
//
//  1. THE ROW SET, BOTH DIRECTIONS. A signed-in reader with NO membership in
//     the project is rendered exactly the `public` rows of the research
//     branch of the page; a MEMBER of the project is rendered all of them,
//     `visibility` still travelling as data. A fix that rendered nobody the
//     private rows would take them away from the members who own them.
//  2. THE ROWS COME FROM THE WRITE PATH. The fixture does not plant a
//     `visibility` value: it creates a public project and a private branch
//     in it through the product's routes, commits on it, and lets the
//     outbox dispatcher publish the event the commit produced. The suite
//     reads the stored visibility back off the table and refuses to run the
//     cases if the shape is not there (a fixture that silently produced
//     public rows would make every negative below vacuous).
//  3. THE GATE'S ANSWER IS UNCHANGED. A reader who may not read the project
//     still gets PROJECT_NOT_FOUND, and the readers who now see fewer rows
//     still get 200 with a page — the fix moves rows, not status codes.
//  4. FAIL CLOSED. A reader that names no user (the path the store takes for
//     an unresolvable actor) is rendered the public rows only: "not found"
//     must never read as "is a member".
//
// The governance half of the feed (audit_log) is deliberately NOT part of
// this rule: that table has no per-row visibility column at all
// (infra/migrations/00012_events_audit.sql), so "a project's activity is as
// visible as the project" is the only rule available to it — see the
// query's header and internal/application/audit/service.go.

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/audithttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"
	"github.com/lichman0405/post/internal/application/audit"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// activityAudienceTaskID namespaces this task's test databases
// (test_T0613_<run_id>).
const activityAudienceTaskID = "T0613"

// --------------------------------------------------------------------------
// The fixture

// activityAudienceFixture is the world the cases read: ONE PUBLIC project
// whose main branch is public and which also carries a PRIVATE branch, each
// with a commit on it. Both commits run the real write path, so the two
// research events the feed renders carry the visibility the write path
// derived for their branch.
//
// Three humans: alice owns the project, dana is seeded as a member (V1 has
// no route that ADDS a membership — the members route changes an existing
// one's role — so the row is written directly, the shape other suites use
// for a fixture fact this build has no surface for), and bob is signed in
// and a member of nothing, which is exactly the reader the ADR's "尚未决定
// #1" describes and the one this rule must still refuse.
type activityAudienceFixture struct {
	ts    *httptest.Server
	pool  *pgxpool.Pool
	orgID string

	alice, bob, dana       *testUserClient
	aliceID, bobID, danaID string
	dispatcher             *events.Dispatcher

	projectID       string
	mainBranchID    string
	privateBranchID string

	// The events the two commits produced, read back off the table: the
	// private branch's rows and main's rows.
	privateEventIDs []string
	publicEventIDs  []string
}

func newActivityAudienceFixture(t *testing.T, ctx context.Context) *activityAudienceFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), activityAudienceTaskID)

	// --- composition, identical to cmd/api/main.go for the surfaces this
	// suite drives: the auth/signup API, the org and project routes, the RSG
	// write surface (branches and commits) and the Activity surface with the
	// SAME projects service instance behind its gate.
	auditStore := persistence.NewAuditStore(pool)
	authAPI := authhttp.New(authhttp.Deps{
		Users:    persistence.NewCredentialStore(pool),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
		Secure: false,
		Audit:  auditStore,
	})
	orgStore := persistence.NewOrgStore(pool)
	orgAPI := orgshttp.New(orgshttp.Deps{Store: orgStore})
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(pool),
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	stateStore := persistence.NewStateStore(pool)
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectAPI.Service(),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    states.NewService(stateStore, newCommitGuard(t)),
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	auditAPI := audithttp.New(audithttp.Deps{
		Store:    auditStore,
		Projects: audit.ProjectsReadGate(projectAPI.Service()),
		Orgs:     orgAPI.Service(),
	})
	mux := http.NewServeMux()
	authAPI.Register(mux)
	mux.Handle("/api/v1/organizations", orgAPI.Routes())
	mux.Handle("/api/v1/organizations/", orgAPI.Routes())
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	rsghttp.New(rsghttp.Deps{Service: rsgSvc}).Register(mux)
	auditAPI.Register(mux)
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	f := &activityAudienceFixture{
		ts:   ts,
		pool: pool,
		dispatcher: events.NewDispatcher(pool,
			events.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))),
	}
	f.alice, f.aliceID = signup(t, ts.URL, "aa-alice@example.com", "aa-alice")
	f.bob, f.bobID = signup(t, ts.URL, "aa-bob@example.com", "aa-bob")
	f.dana, f.danaID = signup(t, ts.URL, "aa-dana@example.com", "aa-dana")

	// --- one PUBLIC project, through the product's routes ----------------
	resp := f.alice.do(t, http.MethodPost, "/api/v1/organizations",
		`{"slug":"aa-labs","name":"Activity Audience Labs"}`)
	mustStatus(t, resp, http.StatusCreated)
	var org orgResponse
	if err := json.NewDecoder(resp.Body).Decode(&org); err != nil {
		t.Fatalf("org create payload: %v", err)
	}
	resp = f.alice.do(t, http.MethodPost, "/api/v1/projects",
		fmt.Sprintf(`{"slug":"aa-public","name":"Activity Audience","purpose":"activity audience",`+
			`"visibility":"public","organization_id":%q}`, org.Organization.ID))
	mustStatus(t, resp, http.StatusCreated)
	var created projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("project create payload: %v", err)
	}
	f.projectID = created.Project.ID
	f.orgID = org.Organization.ID

	// --- main (public) and one PRIVATE branch in the same project -------
	//
	// The visibility is the creator's explicit choice, which is what makes
	// this shape ordinary use rather than a contrived one.
	f.mainBranchID = f.createBranch(t, f.alice, "main", "", "public")
	var genesis string
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM project_states WHERE project_id = $1 AND parent_state_id IS NULL`,
		parseUUIDOrDie(f.projectID)).Scan(&genesis); err != nil {
		t.Fatalf("read the genesis state of the project: %v", err)
	}
	f.privateBranchID = f.createBranch(t, f.alice, "draft-uptake", genesis, "private")

	// --- the one-sided member and the non-member ------------------------
	f.seedMembership(t, ctx, f.danaID)

	// --- one commit on each branch, then the outbox dispatcher ----------
	f.commitObject(t, f.alice, f.privateBranchID, "the draft branch's internal reading")
	f.commitObject(t, f.alice, f.mainBranchID, "the reading main carries")
	if _, err := f.dispatcher.RunOnce(ctx); err != nil {
		t.Fatalf("publish the outbox rows the two commits wrote: %v", err)
	}

	// --- read the shapes back off the TABLE -----------------------------
	f.privateEventIDs = f.eventIDsOfBranch(t, ctx, f.privateBranchID)
	f.publicEventIDs = f.eventIDsOfBranch(t, ctx, f.mainBranchID)

	return f
}

// createBranch creates one branch through the product's route and returns
// its id.
func (f *activityAudienceFixture) createBranch(t *testing.T, uc *testUserClient, name, baseRef, visibility string) string {
	t.Helper()
	resp := uc.do(t, http.MethodPost, "/api/v1/projects/"+f.projectID+"/branches",
		fmt.Sprintf(`{"name":%q,"base_ref":%q,"visibility":%q}`, name, baseRef, visibility))
	mustStatus(t, resp, http.StatusCreated)
	raw := readAll(t, resp)
	var got struct {
		ID         string `json:"id"`
		Visibility string `json:"visibility"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("branch payload: %v: %s", err, raw)
	}
	if got.Visibility != visibility {
		t.Fatalf("branch %q was created with visibility %q, want %q: %s", name, got.Visibility, visibility, raw)
	}
	return got.ID
}

// commitObject writes one object on one branch through the product's route:
// a state commit, which is what makes the write path derive and record a
// research event for the branch's visibility.
func (f *activityAudienceFixture) commitObject(t *testing.T, uc *testUserClient, branchID, statement string) {
	t.Helper()
	body := fmt.Sprintf(`{"object_type":"research_question","payload":{"statement":%q,`+
		`"purpose":%q,"question_state":"open"}}`, statement, "why: "+statement)
	resp := uc.do(t, http.MethodPost,
		"/api/v1/projects/"+f.projectID+"/branches/"+branchID+"/objects", body)
	mustStatus(t, resp, http.StatusCreated)
}

// eventIDsOfBranch reads the research event ids the commits on one branch
// produced, oldest first, and refuses to report an empty list: a branch
// whose commit produced no event would make the audience cases vacuous.
func (f *activityAudienceFixture) eventIDsOfBranch(t *testing.T, ctx context.Context, branchID string) []string {
	t.Helper()
	rows, err := f.pool.Query(ctx, `
		SELECT e.id::text, e.visibility, e.event_type
		FROM research_events e
		WHERE e.project_id = $1 AND e.payload->>'branch_id' = $2
		ORDER BY e.occurred_at, e.id`, parseUUIDOrDie(f.projectID), branchID)
	if err != nil {
		t.Fatalf("read the research events of branch %s: %v", branchID, err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id, visibility, eventType string
		if err := rows.Scan(&id, &visibility, &eventType); err != nil {
			t.Fatalf("scan research event: %v", err)
		}
		// The write path's own derivation, checked here rather than
		// assumed: the events of a private branch are private, and the
		// public branch's are public (rsg/events.go: an event is never more
		// visible than its subject).
		want := "public"
		if branchID == f.privateBranchID {
			want = "private"
		}
		if visibility != want {
			t.Fatalf("the commit on branch %s produced a %s event (%s); the write path derives visibility from the branch",
				branchID, visibility, eventType)
		}
		// A commit produces the transition's own event plus one per semantic
		// write (rsg/events.go); the fixture names the branch in both, which
		// is how these rows are attributed to it here.
		switch eventType {
		case "state.committed", "scientific_object.version_created":
		default:
			t.Fatalf("event %s type = %q, want the commit's own events", id, eventType)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the research events of branch %s: %v", branchID, err)
	}
	if len(ids) == 0 {
		t.Fatalf("branch %s carries no research event: the commit's event was not published", branchID)
	}
	return ids
}

// researchPage reads the project's research Activity page as one reader.
func (f *activityAudienceFixture) researchPage(t *testing.T, uc *testUserClient) activitySourcePage {
	t.Helper()
	return mustSourceActivity(t, uc, "/api/v1/projects/"+f.projectID+"/activity?source=research&limit=50")
}

// renderedIDs is the set of row ids one page rendered.
func renderedIDs(page activitySourcePage) map[string]activitySourceEntry {
	out := make(map[string]activitySourceEntry, len(page.Entries))
	for _, e := range page.Entries {
		out[e.ID] = e
	}
	return out
}

// --------------------------------------------------------------------------
// The required test

// TestActivityFeedAudienceIsTheProjectsMembers: one fixture, three readers,
// one row set each — over the research branch of the Activity feed.
func TestActivityFeedAudienceIsTheProjectsMembers(t *testing.T) {
	ctx := testCtx(t)
	f := newActivityAudienceFixture(t, ctx)

	// The fixture's own facts, read off the TABLE: the cases below are only
	// meaningful while a private event really sits in a public project's
	// feed, with the visibility this suite assumes.
	t.Run("the fixture carries both shapes", func(t *testing.T) {
		var visibility string
		if err := f.pool.QueryRow(ctx, `SELECT visibility FROM projects WHERE id = $1`,
			parseUUIDOrDie(f.projectID)).Scan(&visibility); err != nil {
			t.Fatalf("read the project back: %v", err)
		}
		if visibility != "public" {
			t.Fatalf("the fixture project is %s, want the public project these cases are about", visibility)
		}
		var branchVisibility string
		if err := f.pool.QueryRow(ctx, `SELECT visibility FROM branches WHERE id = $1`,
			parseUUIDOrDie(f.privateBranchID)).Scan(&branchVisibility); err != nil {
			t.Fatalf("read the private branch back: %v", err)
		}
		if branchVisibility != "private" {
			t.Fatalf("the fixture branch is %s, want private", branchVisibility)
		}
		if len(f.privateEventIDs) == 0 || len(f.publicEventIDs) == 0 {
			t.Fatalf("the fixture carries %d private and %d public events, want at least one of each",
				len(f.privateEventIDs), len(f.publicEventIDs))
		}
		// bob holds no membership here at all: the case below would be
		// vacuous if he did.
		if n := countRows(t, ctx, f.pool,
			`SELECT count(*) FROM project_memberships WHERE user_id = $1 AND project_id = $2`,
			parseUUIDOrDie(f.bobID), parseUUIDOrDie(f.projectID)); n != 0 {
			t.Fatalf("the fixture's non-member holds %d memberships in the project", n)
		}
	})

	for _, c := range []struct {
		name        string
		uc          *testUserClient
		seesPrivate bool
	}{
		{name: "signed-in non-member", uc: f.bob},
		{
			name: "project member",
			uc:   f.dana,
			// dana's membership row is seeded by the fixture; she must keep
			// seeing what she sees today.
			seesPrivate: true,
		},
	} {
		c := c
		t.Run(c.name, func(t *testing.T) {
			page := f.researchPage(t, c.uc)
			seen := renderedIDs(page)

			// Every public row is still rendered: a fix that showed nobody
			// anything cannot pass as this fix.
			for _, id := range f.publicEventIDs {
				if _, ok := seen[id]; !ok {
					t.Errorf("the public event %s of a public branch is missing from the page — rendering nobody anything is not this fix: %+v",
						id, page.Entries)
				}
			}
			for _, id := range f.privateEventIDs {
				_, ok := seen[id]
				if ok != c.seesPrivate {
					t.Errorf("the page renders the private event %s = %v, want %v for the %s reader: %+v",
						id, ok, c.seesPrivate, c.name, page.Entries)
				}
				// The row that IS rendered still carries its stored
				// visibility as data (the page renders that column).
				if ok && seen[id].Visibility != "private" {
					t.Errorf("the rendered private row %s carries visibility %q, want the stored private",
						id, seen[id].Visibility)
				}
			}
			// Nothing else came back: the page is the two commits' rows.
			for _, e := range page.Entries {
				if e.Source != "research" {
					t.Errorf("row %s source = %q, want research (?source=research selects one registry)", e.ID, e.Source)
				}
			}
			want := len(f.publicEventIDs)
			if c.seesPrivate {
				want += len(f.privateEventIDs)
			}
			if len(page.Entries) != want {
				t.Errorf("the page rendered %d rows, want %d (%d public + %d private for the %s reader)",
					len(page.Entries), want, len(f.publicEventIDs), len(f.privateEventIDs), c.name)
			}
		})
	}
}

// TestActivityFeedAudienceKeepsTheGateAnswer: the fix moves ROWS, not status
// codes. A signed-in non-member still reads a public project's feed with
// 200, and a project he may not read at all still answers PROJECT_NOT_FOUND
// — the existence-hiding rule the projects surface owns (docs/45).
func TestActivityFeedAudienceKeepsTheGateAnswer(t *testing.T) {
	ctx := testCtx(t)
	f := newActivityAudienceFixture(t, ctx)

	// bob may read the public project: 200, with a page and not an
	// unavailability envelope.
	page := f.researchPage(t, f.bob)
	if len(page.Entries) == 0 {
		t.Fatalf("a signed-in non-member got an EMPTY feed for a public project; the public rows are still his to read")
	}

	// A private project is out of his reach, feed and all.
	resp := f.alice.do(t, http.MethodPost, "/api/v1/projects",
		fmt.Sprintf(`{"slug":"aa-secret","name":"Activity Audience Secret","purpose":"private",`+
			`"visibility":"private","organization_id":%q}`, f.orgID))
	mustStatus(t, resp, http.StatusCreated)
	var created projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("project create payload: %v", err)
	}
	resp = f.bob.do(t, http.MethodGet, "/api/v1/projects/"+created.Project.ID+"/activity?source=research", "")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "PROJECT_NOT_FOUND")
}

// seedMembership writes one membership row for a user (see the fixture
// note: V1 has no route that adds a member).
func (f *activityAudienceFixture) seedMembership(t *testing.T, ctx context.Context, userID string) {
	t.Helper()
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'contributor')`,
		parseUUIDOrDie(f.projectID), parseUUIDOrDie(userID)); err != nil {
		t.Fatalf("seed the membership of %s: %v", userID, err)
	}
}
