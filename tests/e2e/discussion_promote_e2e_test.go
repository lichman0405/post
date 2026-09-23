// The Discussion → Promote to Research Object e2e (T0811): the journey the
// way a client drives it — real HTTP servers and clients through the
// production guard + handlers (cmd/api/authhttp, cmd/api/discussionhttp),
// real PostgreSQL (the per-run namespaced test database, migrated to head
// including 00104) and real stores throughout: the thread, the comments,
// the promotion record and the scientific object the promotion creates all
// run on the production graph, never on mocks. Only Redis is in-process
// (miniredis speaking the real protocol, the same trade the auth and
// conflict e2e tests make).
//
// PostgreSQL is a requirement this file adds to the package: when no
// database is reachable the test skips (the infra-conditional pattern the
// gitea integration tests use) so the infra-free unit gate stays green, and
// runs the full journey wherever PostgreSQL is up. The skip is a judgement
// rather than this caller's own: it goes through testdb.RequireDB, which fails
// instead when the environment declared that a database must be reachable
// (POST_REQUIRE_E2E_DB — internal/persistence/testdb/require_db.go).
//
// T0811-TEST "discussion promotion e2e" (blocking):
//
//	go test ./tests/e2e -run TestE2EDiscussionPromotion -count=1
//
// What the journey pins, in order:
//
//  1. a conversation opens on the project (thread + opening comment) and
//     grows (appended comment) through the guarded surface;
//  2. the conversation moves NO scientific state: the branch head and the
//     state_commits count are identical before and after, while the three
//     00104 tables are the only rows that appeared;
//  3. promotion is refused fail-closed for an authenticated non-member —
//     and a refused promotion writes nothing;
//  4. an Issue promotion records provenance and still moves no state (an
//     Issue is not a state commit);
//  5. a Hypothesis promotion goes through the RSG write path: the object
//     exists as version 1 with a brand-new state id, the branch head has
//     moved to that state, and state_commits grew by exactly one;
//  6. provenance is reverse-traceable over the wire: the ref read answers
//     where an object came from, naming the promoting actor and the
//     comment whose author proposed it;
//  7. a comment written after the promotion again moves no state.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/discussionhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/discussions"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	appvalidation "github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

const discussionTaskID = "T0811"

// discussionEnv is one composed deployment: the guarded API server (real
// handlers, real PostgreSQL) + Redis, seeded with one public project on
// whose main branch a research question lives — the object a promoted
// hypothesis answers.
type discussionEnv struct {
	api      *httptest.Server
	client   *http.Client // cookie jar holds alice's session
	pool     *pgxpool.Pool
	project  domain.Project
	branchID string
	// question is the research_question OBJECT id: a hypothesis payload's
	// question_id names the object, and the state commit refuses a
	// reference that names nothing (00005's trigger).
	question string
	csrf     string // alice's session-bound token
}

// discussionAdminURL resolves the PostgreSQL maintenance database for the
// namespaced test database, the same variable + default the rest of the
// suite uses.
func discussionAdminURL() string {
	if u := os.Getenv("POSTGRES_TEST_ADMIN_URL"); u != "" {
		return u
	}
	return "postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post"
}

// newDiscussionEnv mirrors cmd/api main.go's wiring: same Deps construction,
// same guard + subtree composition — the storage is the real PostgreSQL plus
// miniredis for sessions. The memstore users are seeded with the SAME ids
// the PostgreSQL users carry, so the authenticated principal references a
// real users row (every created_by FK lands on it).
func newDiscussionEnv(t *testing.T, ctx context.Context) *discussionEnv {
	t.Helper()
	admin := discussionAdminURL()
	testdb.RequireDB(t, ctx, admin, "discussion promotion e2e", "the journey needs a real database")
	pool, _ := testdb.Setup(t, ctx, admin, discussionTaskID)

	// alice: the project owner, seeded in PostgreSQL and mirrored into the
	// auth memstore under the same id. bob: an authenticated non-member.
	password := "long-enough-password-1"
	hash, err := authn.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	creds := persistence.NewCredentialStore(pool)
	alice, err := creds.CreateWithPassword(ctx, "disc-alice@example.com", hash, "disc-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	bob, err := creds.CreateWithPassword(ctx, "disc-bob@example.com", hash, "disc-bob", "Bob")
	if err != nil {
		t.Fatalf("seed bob: %v", err)
	}
	users := memstore.NewUsers()
	users.Seed(authn.UserRecord{User: alice, PasswordHash: hash})
	users.Seed(authn.UserRecord{User: bob, PasswordHash: hash})

	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = redisClient.Close() })
	authAPI := authhttp.New(authhttp.Deps{
		Users:      users,
		Sessions:   persistence.NewRedisSessionStore(redisClient),
		Limiter:    persistence.NewRedisRateLimiter(redisClient),
		OIDCClient: nil,
		Cfg:        defaultCfg(),
		Secure:     false,
	})

	// The project over the real stores and the RSG write path that owns the
	// object a promotion creates — the composition cmd/api/main.go uses.
	orgStore := persistence.NewOrgStore(pool)
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "discusion-e2e", Name: "Discussion E2E",
	}, alice.ID, affiliationToday())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	projectStore := persistence.NewProjectStore(pool)
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "discussion-project",
		Name:            "Discussion Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPublic,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create fixture project: %v", err)
	}

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	stateStore := persistence.NewStateStore(pool)
	statesSvc := states.NewService(stateStore,
		appvalidation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe()))
	projectsSvc := projects.NewService(projectStore, orgStore, authz.NewMatrixEngine())
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projectsSvc,
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})

	main, err := rsgSvc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create main: %v", err)
	}
	question, err := rsgSvc.CreateObject(ctx, alice, project.ID, main.ID, rsg.CreateObjectInput{
		ObjectType: "research_question",
		Payload:    json.RawMessage(`{"question":"Does the promoted hypothesis hold?"}`),
	})
	if err != nil {
		t.Fatalf("seed research question: %v", err)
	}

	// The guarded v1 subtree: auth + discussions, exactly as main.go
	// composes them.
	v1 := http.NewServeMux()
	authAPI.Register(v1) // auth routes ride the guarded subtree, as in main.go
	discussionAPI := discussionhttp.New(discussionhttp.Deps{
		Command: discussions.NewCommand(discussions.Deps{
			Projects:   projectsSvc,
			Threads:    persistence.NewDiscussionStore(pool),
			Promotions: persistence.NewDiscussionStore(pool),
			Hypotheses: rsgSvc,
			Evidence:   rsgSvc,
			Authz:      authz.NewMatrixEngine(),
			ForkGate:   persistence.NewForkStore(pool),
		}),
	})
	discussionAPI.Register(v1)

	mux := http.NewServeMux()
	mux.Handle("/api/v1/", authAPI.Guard(v1))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}

	env := &discussionEnv{
		api:      server,
		client:   &http.Client{Jar: jar},
		pool:     pool,
		project:  project,
		branchID: main.ID,
		question: question.Object.ID,
	}
	env.login(t, "disc-alice@example.com", password)
	env.refreshCSRF(t)
	return env
}

// do issues one request through the env's cookie jar (JSON content type
// implied by a non-empty body), carrying alice's CSRF token on writes.
func (e *discussionEnv) do(t *testing.T, method, path, body string) *http.Response {
	t.Helper()
	headers := map[string]string{}
	if method != http.MethodGet {
		headers["X-CSRF-Token"] = e.csrf
	}
	return doWith(t, e.client, e.api.URL+path, method, body, headers)
}

func (e *discussionEnv) login(t *testing.T, email, password string) {
	t.Helper()
	resp := e.do(t, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":%q}`, email, password))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
}

func (e *discussionEnv) refreshCSRF(t *testing.T) {
	t.Helper()
	resp := e.do(t, http.MethodGet, "/api/v1/auth/session", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	var sess struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(bodyBytes(t, resp), &sess); err != nil || sess.CSRFToken == "" {
		t.Fatalf("session payload: %s", bodyBytes(t, resp))
	}
	e.csrf = sess.CSRFToken
}

// threadURL renders the discussion collection of the fixture's project.
func (e *discussionEnv) threadURL() string {
	return "/api/v1/projects/" + e.project.ID + "/discussions"
}

// count reads one scalar count. The test's whole "state did not move" claim
// is made of these, so a broken query fails loudly instead of counting zero.
func (e *discussionEnv) count(t *testing.T, ctx context.Context, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := e.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", sql, err)
	}
	return n
}

// head reads the branch head: the branch row's base_state_id IS the head
// pointer the state store maintains (persistence.StateStore.GetBranchHead
// resolves the head through that column), so reading it here reads the same
// fact the product reads.
func (e *discussionEnv) head(t *testing.T, ctx context.Context) string {
	t.Helper()
	var head string
	if err := e.pool.QueryRow(ctx,
		`SELECT base_state_id::text FROM branches WHERE id = $1`, e.branchID).Scan(&head); err != nil {
		t.Fatalf("read branch head: %v", err)
	}
	return head
}

// commits counts the branch's state transitions.
func (e *discussionEnv) commits(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	return e.count(t, ctx, `SELECT count(*) FROM state_commits WHERE branch_id = $1`, e.branchID)
}

// discussionRowCounts reads the three 00104 tables: the conversation's own
// rows, and nothing else, are what an ordinary write path may add.
func (e *discussionEnv) discussionRowCounts(t *testing.T, ctx context.Context) (threads, comments, promotions int64) {
	t.Helper()
	return e.count(t, ctx, `SELECT count(*) FROM discussion_threads WHERE project_id = $1`, e.project.ID),
		e.count(t, ctx, `SELECT count(*) FROM discussion_comments WHERE project_id = $1`, e.project.ID),
		e.count(t, ctx, `SELECT count(*) FROM discussion_promotions WHERE project_id = $1`, e.project.ID)
}

// --- wire shapes (the fields the test asserts on) ------------------------

type discussionCommentView struct {
	ID        string  `json:"id"`
	ThreadID  string  `json:"thread_id"`
	Body      *string `json:"body"`
	CreatedBy string  `json:"created_by"`
	Deleted   bool    `json:"deleted"`
}

type discussionThreadView struct {
	ID           string                  `json:"id"`
	ProjectID    string                  `json:"project_id"`
	TargetType   string                  `json:"target_type"`
	TargetID     string                  `json:"target_id"`
	CreatedBy    string                  `json:"created_by"`
	CommentCount int64                   `json:"comment_count"`
	Comments     []discussionCommentView `json:"comments"`
}

type discussionPromotionView struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	ThreadID     string    `json:"thread_id"`
	CommentID    string    `json:"comment_id"`
	PromotedKind string    `json:"promoted_kind"`
	PromotedRef  string    `json:"promoted_ref"`
	PromotedBy   string    `json:"promoted_by"`
	PromotedAt   time.Time `json:"promoted_at"`
}

type discussionOriginView struct {
	Promotion discussionPromotionView `json:"promotion"`
	Thread    discussionThreadView    `json:"thread"`
	Comment   discussionCommentView   `json:"comment"`
}

type discussionOpenView struct {
	Thread   discussionThreadView    `json:"thread"`
	Comments []discussionCommentView `json:"comments"`
}

type discussionReadView struct {
	Thread   discussionThreadView    `json:"thread"`
	Comments []discussionCommentView `json:"comments"`
}

type discussionListView struct {
	Threads []discussionThreadView `json:"threads"`
}

type discussionPromoteView struct {
	Promotion discussionPromotionView `json:"promotion"`
	Thread    discussionThreadView    `json:"thread"`
	Comment   discussionCommentView   `json:"comment"`
	Issue     *struct {
		ID        string `json:"id"`
		ProjectID string `json:"project_id"`
		Number    int64  `json:"number"`
		IssueType string `json:"issue_type"`
		Title     string `json:"title"`
		Body      string `json:"body"`
		State     string `json:"state"`
		CreatedBy string `json:"created_by"`
	} `json:"issue"`
	ScientificObject *struct {
		ID             string          `json:"id"`
		ProjectID      string          `json:"project_id"`
		ObjectType     string          `json:"object_type"`
		CurrentVersion int             `json:"current_version"`
		VersionID      string          `json:"version_id"`
		VersionNo      int             `json:"version_no"`
		StateID        string          `json:"state_id"`
		BranchID       *string         `json:"branch_id"`
		Title          string          `json:"title"`
		LifecycleState string          `json:"lifecycle_state"`
		Payload        json.RawMessage `json:"payload"`
	} `json:"scientific_object"`
}

type discussionPromotionList struct {
	Promotions []discussionOriginView `json:"promotions"`
}

// TestE2EDiscussionPromotion is the required "discussion promotion e2e".
func TestE2EDiscussionPromotion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	e := newDiscussionEnv(t, ctx)

	head0, commits0 := e.head(t, ctx), e.commits(t, ctx)
	threads0, comments0, promotions0 := e.discussionRowCounts(t, ctx)

	// 1. The conversation opens on the project — the target's own
	// addressing scheme is the project's id for kind=project.
	open := e.do(t, http.MethodPost, e.threadURL(), fmt.Sprintf(
		`{"target_type":"project","target_id":%q,"body":"Should we try a lower synthesis temperature?"}`,
		e.project.ID))
	if open.StatusCode != http.StatusCreated {
		t.Fatalf("open thread = %d: %s", open.StatusCode, bodyBytes(t, open))
	}
	var opened discussionOpenView
	if err := json.Unmarshal(bodyBytes(t, open), &opened); err != nil {
		t.Fatalf("decode open: %v", err)
	}
	if opened.Thread.TargetType != "project" || opened.Thread.TargetID != e.project.ID {
		t.Fatalf("thread target = %s/%s, want project/%s", opened.Thread.TargetType, opened.Thread.TargetID, e.project.ID)
	}
	if opened.Thread.CreatedBy == "" || len(opened.Comments) != 1 || opened.Comments[0].ID == "" {
		t.Fatalf("opening answer = %+v", opened)
	}
	threadID := opened.Thread.ID

	// The proposal: a second comment — the one that gets promoted.
	added := e.do(t, http.MethodPost, e.threadURL()+"/"+threadID+"/comments",
		`{"body":"The 300K run converged; a hypothesis worth stating is that 280K holds the same product."}`)
	if added.StatusCode != http.StatusCreated {
		t.Fatalf("add comment = %d: %s", added.StatusCode, bodyBytes(t, added))
	}
	var proposal discussionCommentView
	if err := json.Unmarshal(bodyBytes(t, added), &proposal); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	if proposal.Body == nil || *proposal.Body == "" {
		t.Fatalf("comment = %+v, want the body it was written with", proposal)
	}

	// 2. The conversation moved no scientific state: the branch head and the
	// commit count are identical, and the only rows that appeared are the
	// conversation's own.
	threads1, comments1, promotions1 := e.discussionRowCounts(t, ctx)
	if threads1 != threads0+1 || comments1 != comments0+2 || promotions1 != promotions0 {
		t.Fatalf("discussion rows = %d/%d/%d threads/comments/promotions, want %d/%d/%d",
			threads1, comments1, promotions1, threads0+1, comments0+2, promotions0)
	}
	if head := e.head(t, ctx); head != head0 {
		t.Fatalf("the conversation moved the branch head: %s -> %s", head0, head)
	}
	if commits := e.commits(t, ctx); commits != commits0 {
		t.Fatalf("the conversation wrote a state commit: %d -> %d", commits0, commits)
	}

	// 3. Fail closed: bob is authenticated, sees the public project, and may
	// not promote in it. The refusal writes nothing at all.
	bobJar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("bob cookiejar: %v", err)
	}
	bobClient := &http.Client{Jar: bobJar}
	bobLogin := doWith(t, bobClient, e.api.URL+"/api/v1/auth/login", http.MethodPost,
		`{"email":"disc-bob@example.com","password":"long-enough-password-1"}`, nil)
	if bobLogin.StatusCode != http.StatusOK {
		t.Fatalf("bob login = %d: %s", bobLogin.StatusCode, bodyBytes(t, bobLogin))
	}
	var bobSess struct {
		CSRFToken string `json:"csrf_token"`
	}
	bobSession := doWith(t, bobClient, e.api.URL+"/api/v1/auth/session", http.MethodGet, "", nil)
	if bobSession.StatusCode != http.StatusOK {
		t.Fatalf("bob session = %d: %s", bobSession.StatusCode, bodyBytes(t, bobSession))
	}
	if err := json.Unmarshal(bodyBytes(t, bobSession), &bobSess); err != nil || bobSess.CSRFToken == "" {
		t.Fatalf("bob session payload: %s", bodyBytes(t, bobSession))
	}
	refused := doWith(t, bobClient,
		e.api.URL+e.threadURL()+"/"+threadID+"/comments/"+proposal.ID+"/promotions", http.MethodPost,
		fmt.Sprintf(`{"kind":"issue","issue_type":"question","title":%q}`, "bob tries to promote"),
		map[string]string{"X-CSRF-Token": bobSess.CSRFToken})
	if refused.StatusCode != http.StatusForbidden {
		t.Fatalf("bob promote = %d: %s", refused.StatusCode, bodyBytes(t, refused))
	}
	if _, _, promotions := e.discussionRowCounts(t, ctx); promotions != promotions0 {
		t.Fatalf("a refused promotion wrote a row: %d, want %d", promotions, promotions0)
	}
	if head := e.head(t, ctx); head != head0 {
		t.Fatalf("a refused promotion moved the branch head: %s -> %s", head0, head)
	}

	// 4. The owner promotes the proposal to an Issue: a recorded promotion
	// with its provenance, and still no state transition — an Issue is not
	// part of the research state.
	promoteURL := e.threadURL() + "/" + threadID + "/comments/" + proposal.ID + "/promotions"
	issueResp := e.do(t, http.MethodPost, promoteURL, fmt.Sprintf(
		`{"kind":"issue","issue_type":"question","title":%q}`, "Does 280K hold the same product?"))
	if issueResp.StatusCode != http.StatusCreated {
		t.Fatalf("promote to issue = %d: %s", issueResp.StatusCode, bodyBytes(t, issueResp))
	}
	var issueView discussionPromoteView
	if err := json.Unmarshal(bodyBytes(t, issueResp), &issueView); err != nil {
		t.Fatalf("decode issue promotion: %v", err)
	}
	if issueView.Issue == nil || issueView.Issue.Number == 0 {
		t.Fatalf("issue promotion answer = %+v", issueView)
	}
	if want := "issue:" + issueView.Issue.ID; issueView.Promotion.PromotedRef != want {
		t.Fatalf("promoted_ref = %q, want %q", issueView.Promotion.PromotedRef, want)
	}
	if issueView.Promotion.PromotedBy == "" || issueView.Comment.CreatedBy != issueView.Promotion.PromotedBy {
		t.Fatalf("promotion provenance = %+v / comment %+v", issueView.Promotion, issueView.Comment)
	}
	if head := e.head(t, ctx); head != head0 {
		t.Fatalf("an Issue promotion moved the branch head: %s -> %s", head0, head)
	}
	if commits := e.commits(t, ctx); commits != commits0 {
		t.Fatalf("an Issue promotion wrote a state commit: %d -> %d", commits0, commits)
	}

	// 5. The owner promotes the same proposal to a Hypothesis: the RSG write
	// path creates the object, which IS a state transition — version 1, a
	// new state id, and the branch head moved onto it.
	hypResp := e.do(t, http.MethodPost, promoteURL, fmt.Sprintf(
		`{"kind":"hypothesis","branch_id":%q,"hypothesis":{"question_id":%q}}`, e.branchID, e.question))
	if hypResp.StatusCode != http.StatusCreated {
		t.Fatalf("promote to hypothesis = %d: %s", hypResp.StatusCode, bodyBytes(t, hypResp))
	}
	var hypView discussionPromoteView
	if err := json.Unmarshal(bodyBytes(t, hypResp), &hypView); err != nil {
		t.Fatalf("decode hypothesis promotion: %v", err)
	}
	obj := hypView.ScientificObject
	if obj == nil {
		t.Fatalf("hypothesis promotion answer carries no object: %s", bodyBytes(t, hypResp))
	}
	if obj.ObjectType != "hypothesis" || obj.VersionNo != 1 || obj.StateID == head0 {
		t.Fatalf("created object = %+v, want a hypothesis at version 1 in a new state", obj)
	}
	if obj.BranchID == nil || *obj.BranchID != e.branchID {
		t.Fatalf("created object branch = %v, want %s", obj.BranchID, e.branchID)
	}
	var statement, questionID string
	var payload map[string]string
	if err := json.Unmarshal(obj.Payload, &payload); err != nil {
		t.Fatalf("decode hypothesis payload: %v", err)
	}
	statement, questionID = payload["statement"], payload["question_id"]
	if statement != *proposal.Body {
		t.Fatalf("hypothesis statement = %q, want the promoted comment %q", statement, *proposal.Body)
	}
	if questionID != e.question {
		t.Fatalf("hypothesis question_id = %q, want %q", questionID, e.question)
	}
	if want := "hypothesis:" + obj.ID; hypView.Promotion.PromotedRef != want {
		t.Fatalf("promoted_ref = %q, want %q", hypView.Promotion.PromotedRef, want)
	}
	if head := e.head(t, ctx); head != obj.StateID {
		t.Fatalf("branch head = %s, want the promoted object's state %s", head, obj.StateID)
	}
	if commits := e.commits(t, ctx); commits != commits0+1 {
		t.Fatalf("state commits = %d, want exactly one more than %d", commits, commits0)
	}
	var storedState, storedTitle string
	var storedVersionNo int
	if err := e.pool.QueryRow(ctx,
		`SELECT state_id, version_no, title FROM scientific_object_versions WHERE id = $1`, obj.VersionID).
		Scan(&storedState, &storedVersionNo, &storedTitle); err != nil {
		t.Fatalf("read the promoted version: %v", err)
	}
	if storedState != obj.StateID || storedVersionNo != 1 {
		t.Fatalf("stored version = %s v%d, want %s v1", storedState, storedVersionNo, obj.StateID)
	}

	// 6. Provenance is reverse-traceable over the wire: the ref read names
	// the promotion, the promoting actor and the comment whose author
	// proposed it.
	listResp := e.do(t, http.MethodGet, e.threadURL()+"/promotions?ref="+hypView.Promotion.PromotedRef, "")
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("promotions for ref = %d: %s", listResp.StatusCode, bodyBytes(t, listResp))
	}
	var byRef discussionPromotionList
	if err := json.Unmarshal(bodyBytes(t, listResp), &byRef); err != nil {
		t.Fatalf("decode promotions: %v", err)
	}
	if len(byRef.Promotions) != 1 {
		t.Fatalf("promotions for %s = %+v, want exactly the one", hypView.Promotion.PromotedRef, byRef.Promotions)
	}
	origin := byRef.Promotions[0]
	if origin.Promotion.ID != hypView.Promotion.ID || origin.Thread.ID != threadID || origin.Comment.ID != proposal.ID {
		t.Fatalf("origin = %+v, want the promotion in this thread of this comment", origin)
	}
	if origin.Promotion.PromotedBy != origin.Thread.CreatedBy || origin.Comment.Body == nil {
		t.Fatalf("origin provenance = %+v", origin)
	}
	byID := e.do(t, http.MethodGet, e.threadURL()+"/promotions/"+hypView.Promotion.ID, "")
	if byID.StatusCode != http.StatusOK {
		t.Fatalf("promotion by id = %d: %s", byID.StatusCode, bodyBytes(t, byID))
	}
	var single discussionOriginView
	if err := json.Unmarshal(bodyBytes(t, byID), &single); err != nil {
		t.Fatalf("decode promotion: %v", err)
	}
	if single.Promotion.PromotedRef != hypView.Promotion.PromotedRef || single.Comment.ID != proposal.ID {
		t.Fatalf("promotion by id = %+v", single)
	}

	// 7. The conversation continues after the promotion and still moves no
	// state.
	after := e.do(t, http.MethodPost, e.threadURL()+"/"+threadID+"/comments",
		`{"body":"Noted — the hypothesis is on the branch now."}`)
	if after.StatusCode != http.StatusCreated {
		t.Fatalf("comment after the promotion = %d: %s", after.StatusCode, bodyBytes(t, after))
	}
	if head := e.head(t, ctx); head != obj.StateID {
		t.Fatalf("a comment after the promotion moved the branch head: %s -> %s", obj.StateID, head)
	}
	if commits := e.commits(t, ctx); commits != commits0+1 {
		t.Fatalf("a comment after the promotion wrote a state commit: %d, want %d", commits, commits0+1)
	}

	// The thread read renders the whole conversation, and the list read
	// carries its activity.
	read := e.do(t, http.MethodGet, e.threadURL()+"/"+threadID, "")
	if read.StatusCode != http.StatusOK {
		t.Fatalf("read thread = %d: %s", read.StatusCode, bodyBytes(t, read))
	}
	var thread discussionReadView
	if err := json.Unmarshal(bodyBytes(t, read), &thread); err != nil {
		t.Fatalf("decode thread: %v", err)
	}
	if len(thread.Comments) != 3 {
		t.Fatalf("thread comments = %d, want 3", len(thread.Comments))
	}
	list := e.do(t, http.MethodGet, e.threadURL()+"?target_type=project&target_id="+e.project.ID, "")
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list threads = %d: %s", list.StatusCode, bodyBytes(t, list))
	}
	var threads discussionListView
	if err := json.Unmarshal(bodyBytes(t, list), &threads); err != nil {
		t.Fatalf("decode thread list: %v", err)
	}
	if len(threads.Threads) != 1 || threads.Threads[0].ID != threadID || threads.Threads[0].CommentCount != 3 {
		t.Fatalf("thread list = %+v, want the one thread with 3 live comments", threads.Threads)
	}
}
