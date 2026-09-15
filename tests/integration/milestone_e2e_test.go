package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/milestonehttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/milestones"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// Package integration — T0609-TEST-01 "milestone e2e" (blocking): the
// project milestone API exercised end to end against REAL PostgreSQL —
// real migrations, real pgx stores, the real auth guard (session + CSRF)
// and the real milestonehttp/projectshttp/orgshttp handlers, composed
// exactly as cmd/api/main.go composes them. Only the session store is
// in-memory (Redis semantics are orthogonal and covered elsewhere).
//
// The acceptance criteria each map to a phase of the test:
//
//   - "Project 没有强制 Completed 终态" → TestMilestoneE2E: every create
//     in the journey leaves projects.activity_status at its pre-existing
//     value ('planning' — never advanced, never completed), the database
//     CHECK rejects a literal 'completed' activity_status (SQLSTATE
//     23514), and the milestone kind CHECK rejects a 'completed' kind;
//   - "Milestone timeline 正确" → TestMilestoneE2E: milestones created
//     deliberately out of chronological order render in research order
//     (occurred_at ascending, creation order breaking date ties) — the
//     timeline is the dates' order, never the insertion order.
//
// The requirements map to the other phases:
//
//   - separation from releases → milestones are recorded with no release
//     anywhere in the flow, and no release is required (a release-less
//     milestone is a first-class row);
//   - kinds + custom label → each canonical kind records; a custom
//     milestone requires its label (wire 400 and database CHECK);
//   - optional release link → a milestone may name a release of the same
//     project; a link that does not resolve in the project answers 404
//     MILESTONE_RELEASE_NOT_FOUND (existence hiding, docs/45).

const milestoneE2ETaskID = "T0609"

// milestoneE2EPayload is the wire shape of one milestone (milestonehttp
// milestonePayload).
type milestoneE2EPayload struct {
	ID         string  `json:"id"`
	ProjectID  string  `json:"project_id"`
	Kind       string  `json:"kind"`
	Label      *string `json:"label"`
	OccurredAt string  `json:"occurred_at"`
	ReleaseID  *string `json:"release_id"`
	CreatedBy  string  `json:"created_by"`
	CreatedAt  string  `json:"created_at"`
}

// milestoneE2EList is the list envelope ({"milestones":[...]}).
type milestoneE2EList struct {
	Milestones []milestoneE2EPayload `json:"milestones"`
}

// newMilestoneServer composes the production tree (like cmd/api/main.go)
// — auth + orgs + projects + milestones over one real test database —
// and returns the server plus the pool for SQL-level verification.
func newMilestoneServer(t *testing.T, ctx context.Context) (*httptest.Server, *pgxpool.Pool) {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), milestoneE2ETaskID)
	sessions := memstore.NewSessions()
	limiter := memstore.NewLimiter()
	cfg := authn.Config{
		WebOrigin:          "http://web.test",
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 1000,
		LoginLimitPerIP:    10000,
		LoginWindow:        time.Minute,
		SignupLimitPerIP:   10000,
	}
	authAPI := authhttp.New(authhttp.Deps{
		Users:      persistence.NewCredentialStore(pool),
		Sessions:   sessions,
		Limiter:    limiter,
		OIDCClient: nil,
		Cfg:        cfg,
		Secure:     false,
	})
	orgStore := persistence.NewOrgStore(pool)
	orgAPI := orgshttp.New(orgshttp.Deps{Store: orgStore})
	projectStore := persistence.NewProjectStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectStore,
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	releaseStore := persistence.NewReleaseStore(pool)
	milestoneCommand := milestones.NewCommand(
		projectStore,
		releaseStore,
		persistence.NewMilestoneStore(pool),
		authz.NewMatrixEngine(),
	)
	milestoneAPI := milestonehttp.New(milestonehttp.Deps{
		Command:  milestoneCommand,
		Projects: projectAPI.Service(),
	})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", authAPI.Routes())
	apiMux.Handle("/api/v1/organizations", orgAPI.Routes())
	apiMux.Handle("/api/v1/organizations/", orgAPI.Routes())
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	milestoneAPI.Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)
	return ts, pool
}

// createMilestone posts one milestone create; key == "" sends no
// Idempotency-Key.
func createMilestone(t *testing.T, uc *testUserClient, projectID, kind, label, occurredAt, releaseID, key string) *http.Response {
	t.Helper()
	labelField := ""
	if label != "" {
		labelField = fmt.Sprintf(`"label":%q,`, label)
	}
	releaseField := ""
	if releaseID != "" {
		releaseField = fmt.Sprintf(`,"release_id":%q`, releaseID)
	}
	body := fmt.Sprintf(`{"kind":%q,%s"occurred_at":%q%s}`, kind, labelField, occurredAt, releaseField)
	return uc.doKeyed(t, http.MethodPost, "/api/v1/projects/"+projectID+"/milestones", body, key)
}

// decodeMilestone reads one milestone payload off the wire.
func decodeMilestone(t *testing.T, resp *http.Response) milestoneE2EPayload {
	t.Helper()
	var m milestoneE2EPayload
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("milestone payload: %v", err)
	}
	return m
}

// listMilestones reads the timeline off the wire.
func listMilestones(t *testing.T, uc *testUserClient, projectID string) []milestoneE2EPayload {
	t.Helper()
	resp := uc.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/milestones", "")
	mustStatus(t, resp, http.StatusOK)
	var body milestoneE2EList
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("milestone list payload: %v", err)
	}
	return body.Milestones
}

// seedRelease inserts one release row directly (the milestone link check
// reads the row; the release's own creation flow is T0606's, tested
// there) and returns its id.
func seedRelease(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, createdBy, version string) string {
	t.Helper()
	q := sqlc.New(pool)
	var stateID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO project_states (project_id, state_hash, manifest_version)
		 VALUES ($1, $2, '1.0') RETURNING id`,
		projectID, "seed-state-"+version).Scan(&stateID); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	row, err := q.CreateRelease(ctx, sqlc.CreateReleaseParams{
		ProjectID:    parseUUIDOrDie(projectID),
		Version:      version,
		Title:        version,
		StateID:      parseUUIDOrDie(stateID),
		Manifest:     `{"version":"` + version + `"}`,
		ManifestHash: "seed-hash-" + version,
		CreatedBy:    parseUUIDOrDie(createdBy),
	})
	if err != nil {
		t.Fatalf("seed release: %v", err)
	}
	return pgUUIDTextTest(row.ID)
}

// TestMilestoneE2E drives the full milestone journey over the wire
// against real PostgreSQL.
func TestMilestoneE2E(t *testing.T) {
	ctx := testCtx(t)
	ts, pool := newMilestoneServer(t, ctx)

	alice, aliceID := signup(t, ts.URL, "milestone-alice@example.com", "milestone-alice")
	bob, bobID := signup(t, ts.URL, "milestone-bob@example.com", "milestone-bob")

	// --- alice creates the organization and one private project ---
	resp := alice.do(t, http.MethodPost, "/api/v1/organizations",
		`{"slug":"milestone-labs","name":"Milestone Labs"}`)
	mustStatus(t, resp, http.StatusCreated)
	var createdOrg orgResponse
	if err := json.NewDecoder(resp.Body).Decode(&createdOrg); err != nil {
		t.Fatalf("org create payload: %v", err)
	}
	orgID := createdOrg.Organization.ID

	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		fmt.Sprintf(`{"slug":"milestone-lab","name":"Milestone Lab","purpose":"timeline facts","visibility":"private","organization_id":%q}`, orgID))
	mustStatus(t, resp, http.StatusCreated)
	var createdProj projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&createdProj); err != nil {
		t.Fatalf("project create payload: %v", err)
	}
	projectID := createdProj.Project.ID
	if createdProj.Project.ActivityStatus != "planning" {
		t.Fatalf("fresh project activity_status = %q, want planning", createdProj.Project.ActivityStatus)
	}

	// Bob joins as contributor: he reads the timeline, but the matrix
	// (the reused ActionCreateRelease) denies him the create.
	if _, err := pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
		projectID, bobID, "contributor"); err != nil {
		t.Fatalf("seed bob membership: %v", err)
	}

	t.Run("private project hides the timeline from anonymous", func(t *testing.T) {
		anon := newTestUserClient(ts.URL)
		resp := anon.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/milestones", "")
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, milestones.CodeMilestoneProjectNotFound)
	})

	// --- the timeline journey: create deliberately OUT of chronological
	// order, so the timeline's correctness (occurred_at order) is
	// distinguishable from the insertion order ---
	timeline := []milestoneE2EPayload{}

	t.Run("create records each canonical kind and a custom label", func(t *testing.T) {
		creates := []struct {
			kind, label, occurredAt, releaseID string
		}{
			// Insertion order: paper, candidate, custom, external, patent.
			{"paper_submitted", "JACS 2026", "2026-03-05T10:00:00Z", ""},
			{"candidate_selected", "", "2026-01-10T09:00:00Z", ""},
			{"custom", "First external replicate", "2026-06-01T12:00:00Z", ""},
			// Same-date pair: the timeline breaks the tie by creation
			// order — external_validation was created before patent_filed.
			{"external_validation", "", "2026-04-02T08:00:00Z", ""},
			{"patent_filed", "US-2026-xxxx", "2026-04-02T08:00:00Z", ""},
		}
		for i, c := range creates {
			resp := createMilestone(t, alice, projectID, c.kind, c.label, c.occurredAt, c.releaseID, "")
			mustStatus(t, resp, http.StatusCreated)
			m := decodeMilestone(t, resp)
			if m.Kind != c.kind || m.ProjectID != projectID || m.CreatedBy != aliceID {
				t.Fatalf("create %d = %+v, want kind %q on the project by alice", i, m, c.kind)
			}
			if c.label == "" && m.Label != nil {
				t.Fatalf("create %d label = %v, want null (no custom label)", i, m.Label)
			}
			if c.label != "" && (m.Label == nil || *m.Label != c.label) {
				t.Fatalf("create %d label = %v, want %q", i, m.Label, c.label)
			}
			if m.ReleaseID != nil {
				t.Fatalf("create %d release_id = %v, want null (the link is optional)", i, m.ReleaseID)
			}
			if got, err := time.Parse(time.RFC3339, m.OccurredAt); err != nil || got.UTC().Format(time.RFC3339) != c.occurredAt {
				t.Fatalf("create %d occurred_at = %q, want %q", i, m.OccurredAt, c.occurredAt)
			}
			timeline = append(timeline, m)
		}
	})

	t.Run("timeline renders in research order, not insertion order", func(t *testing.T) {
		got := listMilestones(t, alice, projectID)
		wantKinds := []string{
			"candidate_selected",  // 2026-01-10
			"paper_submitted",     // 2026-03-05
			"external_validation", // 2026-04-02 (created first — tie break)
			"patent_filed",        // 2026-04-02 (created second)
			"custom",              // 2026-06-01
		}
		if len(got) != len(wantKinds) {
			t.Fatalf("timeline length = %d, want %d", len(got), len(wantKinds))
		}
		for i, kind := range wantKinds {
			if got[i].Kind != kind {
				t.Fatalf("timeline[%d].kind = %q, want %q (full: %+v)", i, got[i].Kind, kind, got)
			}
		}
		// The insertion order was paper first; the timeline is not the
		// insertion order.
		if got[0].Kind == "paper_submitted" {
			t.Fatalf("timeline starts with the first-inserted row — the occurred_at order was not applied")
		}
	})

	t.Run("get one milestone and existence hiding", func(t *testing.T) {
		resp := alice.do(t, http.MethodGet,
			"/api/v1/projects/"+projectID+"/milestones/"+timeline[0].ID, "")
		mustStatus(t, resp, http.StatusOK)
		if m := decodeMilestone(t, resp); m.ID != timeline[0].ID {
			t.Fatalf("get returned %q, want %q", m.ID, timeline[0].ID)
		}
		resp = alice.do(t, http.MethodGet,
			"/api/v1/projects/"+projectID+"/milestones/00000000-0000-0000-0000-000000000000", "")
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, milestones.CodeMilestoneNotFound)
	})

	t.Run("validation refuses malformed records", func(t *testing.T) {
		// custom without a label has no name.
		resp := createMilestone(t, alice, projectID, "custom", "  ", "2026-06-02T00:00:00Z", "", "")
		mustStatus(t, resp, http.StatusBadRequest)
		mustEnvelope(t, resp, milestones.CodeMilestoneValidationFailed)
		// no kind is a completion state — "completed" is not in the
		// vocabulary (the no-forced-Completed acceptance).
		resp = createMilestone(t, alice, projectID, "completed", "", "2026-06-02T00:00:00Z", "", "")
		mustStatus(t, resp, http.StatusBadRequest)
		mustEnvelope(t, resp, milestones.CodeMilestoneValidationFailed)
		// a malformed date cannot position a timeline entry.
		resp = createMilestone(t, alice, projectID, "paper_submitted", "", "02/06/2026", "", "")
		mustStatus(t, resp, http.StatusBadRequest)
		mustEnvelope(t, resp, milestones.CodeMilestoneValidationFailed)
	})

	t.Run("authz denies the create below maintainer", func(t *testing.T) {
		resp := createMilestone(t, bob, projectID, "paper_submitted", "", "2026-07-01T00:00:00Z", "", "")
		mustStatus(t, resp, http.StatusForbidden)
		mustEnvelope(t, resp, milestones.CodeMilestoneForbidden)
		anon := newTestUserClient(ts.URL)
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/projects/"+projectID+"/milestones",
			strings.NewReader(`{"kind":"paper_submitted","occurred_at":"2026-07-01T00:00:00Z"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		aresp, err := anon.client.Do(req)
		if err != nil {
			t.Fatalf("anonymous POST: %v", err)
		}
		defer aresp.Body.Close()
		mustStatus(t, aresp, http.StatusUnauthorized)
	})

	t.Run("release link resolves in the project only", func(t *testing.T) {
		releaseID := seedRelease(t, ctx, pool, projectID, aliceID, "seed-v1")
		resp := createMilestone(t, alice, projectID, "paper_submitted", "links the release",
			"2026-08-01T00:00:00Z", releaseID, "")
		mustStatus(t, resp, http.StatusCreated)
		if m := decodeMilestone(t, resp); m.ReleaseID == nil || *m.ReleaseID != releaseID {
			t.Fatalf("release_id = %v, want %q", m.ReleaseID, releaseID)
		}
		// A release of ANOTHER project is not found (no foreign
		// existence leak).
		other := alice.do(t, http.MethodPost, "/api/v1/projects",
			`{"slug":"other-lab","name":"Other Lab","purpose":"foreign boundary","visibility":"private"}`)
		mustStatus(t, other, http.StatusCreated)
		var otherProj projectResponse
		if err := json.NewDecoder(other.Body).Decode(&otherProj); err != nil {
			t.Fatalf("other project payload: %v", err)
		}
		foreignRelease := seedRelease(t, ctx, pool, otherProj.Project.ID, aliceID, "foreign-v1")
		resp = createMilestone(t, alice, projectID, "paper_submitted", "", "2026-08-02T00:00:00Z", foreignRelease, "")
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, milestones.CodeMilestoneReleaseNotFound)
		// An unknown id is the same outcome.
		resp = createMilestone(t, alice, projectID, "paper_submitted", "", "2026-08-03T00:00:00Z",
			"00000000-0000-0000-0000-000000000000", "")
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, milestones.CodeMilestoneReleaseNotFound)
	})

	t.Run("idempotency replays and never duplicates", func(t *testing.T) {
		resp := createMilestone(t, alice, projectID, "candidate_selected", "", "2026-09-01T00:00:00Z", "", "milestone-key-1")
		mustStatus(t, resp, http.StatusCreated)
		first := decodeMilestone(t, resp)

		resp = createMilestone(t, alice, projectID, "candidate_selected", "", "2026-09-01T00:00:00Z", "", "milestone-key-1")
		mustStatus(t, resp, http.StatusCreated)
		replayed := decodeMilestone(t, resp)
		if replayed.ID != first.ID {
			t.Fatalf("replay id = %q, want the first create's %q", replayed.ID, first.ID)
		}

		resp = createMilestone(t, alice, projectID, "candidate_selected", "", "2026-09-02T00:00:00Z", "", "milestone-key-2")
		mustStatus(t, resp, http.StatusCreated)
		if fresh := decodeMilestone(t, resp); fresh.ID == first.ID {
			t.Fatalf("a fresh key returned the first create's row %q", fresh.ID)
		}

		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM project_milestones WHERE project_id = $1`, projectID).Scan(&n); err != nil {
			t.Fatalf("count milestones: %v", err)
		}
		// journey rows + the release-linked row + the two idempotency rows.
		if n != len(timeline)+3 {
			t.Fatalf("milestone rows = %d, want %d (the replay must not duplicate)", n, len(timeline)+3)
		}
	})

	t.Run("recording milestones never touches the project lifecycle", func(t *testing.T) {
		// After every create above, the project's activity_status is
		// still its pre-existing value — milestones are timeline facts,
		// never lifecycle transitions (the no-forced-Completed
		// acceptance).
		var status string
		if err := pool.QueryRow(ctx,
			`SELECT activity_status FROM projects WHERE id = $1`, projectID).Scan(&status); err != nil {
			t.Fatalf("read activity_status: %v", err)
		}
		if status != "planning" {
			t.Fatalf("activity_status = %q, want planning (milestones must not advance it)", status)
		}
		// The database itself refuses a "completed" activity_status — no
		// forced Completed terminal state exists at any layer.
		_, err := pool.Exec(ctx,
			`UPDATE projects SET activity_status = 'completed' WHERE id = $1`, projectID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("UPDATE to 'completed' error = %v, want CHECK violation 23514", err)
		}
		// The milestone kind CHECK likewise refuses a completion kind.
		_, err = pool.Exec(ctx,
			`INSERT INTO project_milestones (project_id, kind, occurred_at, created_by)
			 VALUES ($1, 'completed', now(), $2)`, projectID, aliceID)
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("kind='completed' insert error = %v, want CHECK violation 23514", err)
		}
		// The custom-label CHECK holds at the database level too.
		_, err = pool.Exec(ctx,
			`INSERT INTO project_milestones (project_id, kind, label, occurred_at, created_by)
			 VALUES ($1, 'custom', NULL, now(), $2)`, projectID, aliceID)
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("custom-without-label insert error = %v, want CHECK violation 23514", err)
		}
	})

	t.Run("audit records the milestone.created action", func(t *testing.T) {
		var targetRef, action, actor string
		if err := pool.QueryRow(ctx,
			`SELECT target_ref, action, actor_id FROM audit_log
			 WHERE project_id = $1 AND action = 'milestone.created'
			 ORDER BY occurred_at DESC LIMIT 1`, projectID).Scan(&targetRef, &action, &actor); err != nil {
			t.Fatalf("read audit row: %v", err)
		}
		if action != "milestone.created" {
			t.Fatalf("audit action = %q, want milestone.created", action)
		}
		if !strings.HasPrefix(targetRef, "milestone:") {
			t.Fatalf("target_ref = %q, want the milestone:<id> shape", targetRef)
		}
		if actor != aliceID {
			t.Fatalf("audit actor = %q, want alice", actor)
		}
	})

	t.Run("bob reads the timeline he cannot write", func(t *testing.T) {
		got := listMilestones(t, bob, projectID)
		if len(got) != len(timeline)+3 {
			t.Fatalf("bob's timeline length = %d, want %d", len(got), len(timeline)+3)
		}
	})
}
