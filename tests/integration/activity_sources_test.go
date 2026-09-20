// Package integration — T0607's API half: the Activity feed's two
// registries and the source filter, against REAL PostgreSQL.
//
// The page reads the project's governance rows (audit_log, T0110) and its
// research events (research_events, T1001) as ONE key-set-paginated
// sequence; ?source=governance|research narrows it to one registry and the
// absence of the parameter reads both. This file locks the contract the
// browser suite (tests/e2e-activity) mocks and the page renders:
//
//   - filter governance/research events → the source filter selects the
//     registry, and the three answers add up: |both| = |governance| +
//     |research|, with no row in the wrong answer and no row missing from
//     its own;
//   - actor/via links → every row carries the actor's handle/display name
//     and its channel, whichever registry it came from;
//   - abort/reopen/release display → the display inputs the page renders
//     (action, target_ref, the before/after summaries, the reason code, the
//     event payload) survive the round trip on both sources.
//
// The rows are seeded two ways on purpose. Governance rows come from real
// API calls wherever the journey can produce them, and research events are
// inserted directly with SQL — which is exactly what the three stores that
// write them in their own transactions do (release_store, asset_publish_store,
// knowledge_publish_store use the same INSERT), and the only way to place an
// event at a chosen instant without waiting for a clock. The readers are
// what this test covers; the writers are covered by their own suites.
//
// The pagination phase is the one that could not be skipped: a merged
// stream is one window, and a window cut by two independent LIMITs would
// duplicate or lose rows at the seam. It walks the feed at two page sizes —
// one row at a time, and three at a time — including across a pair of rows
// that share an instant, where only the (occurred_at, id) tie-break keeps
// the walk from looping or skipping. The multi-row walk is not redundant:
// in a one-row page the cursor is the same value whichever end of the page
// it is read from, so only a wider page can show that it was read from the
// right end.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/audithttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/audit"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

const activitySourcesTaskID = "T0607"

// activityEntry is extended from audit_test.go's wire type: this file needs
// the two T0607 fields (the source and the research row's payload and
// visibility), and reusing the T0110 type would have made one struct carry
// two tasks' expectations. The shared fields are declared identically.
type activitySourceEntry struct {
	ID               string          `json:"id"`
	Source           string          `json:"source"`
	ActorID          *string         `json:"actor_id"`
	ActorHandle      *string         `json:"actor_handle"`
	ActorDisplayName *string         `json:"actor_display_name"`
	Via              string          `json:"via"`
	Action           string          `json:"action"`
	TargetRef        *string         `json:"target_ref"`
	ProjectID        *string         `json:"project_id"`
	OrganizationID   *string         `json:"organization_id"`
	CorrelationID    string          `json:"correlation_id"`
	BeforeSummary    json.RawMessage `json:"before_summary"`
	AfterSummary     json.RawMessage `json:"after_summary"`
	Metadata         json.RawMessage `json:"metadata"`
	Payload          json.RawMessage `json:"payload"`
	Visibility       string          `json:"visibility"`
	OccurredAt       time.Time       `json:"occurred_at"`
}

type activitySourcePage struct {
	Entries    []activitySourceEntry `json:"entries"`
	NextCursor *string               `json:"next_cursor"`
}

// mustSourceActivity GETs one Activity page and decodes it.
func mustSourceActivity(t *testing.T, uc *testUserClient, path string) activitySourcePage {
	t.Helper()
	resp := uc.do(t, http.MethodGet, path, "")
	mustStatus(t, resp, http.StatusOK)
	var page activitySourcePage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("activity payload %s: %v", path, err)
	}
	return page
}

// seedResearchEvent inserts one research event at a chosen instant, the way
// the release / asset-publish / knowledge-publish stores do inside their own
// transactions. id is returned so a test can name the row it seeded.
func seedResearchEvent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, actorID, eventType, visibility, payload string, occurredAt time.Time) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `
		INSERT INTO research_events
		    (event_type, actor_id, project_id, visibility, payload, correlation_id, occurred_at, via)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8)
		RETURNING id`,
		eventType, actorID, projectID, visibility, payload, "activity-sources-test", occurredAt, "session").Scan(&id); err != nil {
		t.Fatalf("seed research event %s: %v", eventType, err)
	}
	return id
}

// TestActivitySourcesIntegration is T0607's API contract test.
func TestActivitySourcesIntegration(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), activitySourcesTaskID)

	// --- composition, identical to cmd/api/main.go (audit_test.go's, kept
	// local so the two files' journeys cannot interfere) ---
	sessions := memstore.NewSessions()
	cfg := authn.Config{
		WebOrigin:          "http://web.test",
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 1000,
		LoginLimitPerIP:    10000,
		LoginWindow:        time.Minute,
		SignupLimitPerIP:   10000,
	}
	auditStore := persistence.NewAuditStore(pool)
	authAPI := authhttp.New(authhttp.Deps{
		Users:      persistence.NewCredentialStore(pool),
		Sessions:   sessions,
		Limiter:    memstore.NewLimiter(),
		OIDCClient: nil,
		Cfg:        cfg,
		Secure:     false,
		Audit:      auditStore,
	})
	orgStore := persistence.NewOrgStore(pool)
	orgAPI := orgshttp.New(orgshttp.Deps{Store: orgStore})
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(pool),
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	auditAPI := audithttp.New(audithttp.Deps{
		Store:    auditStore,
		Projects: audit.ProjectsReadGate(projectAPI.Service()),
		Orgs:     orgAPI.Service(),
	})
	apiMux := http.NewServeMux()
	authAPI.Register(apiMux)
	apiMux.Handle("/api/v1/organizations", orgAPI.Routes())
	apiMux.Handle("/api/v1/organizations/", orgAPI.Routes())
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	auditAPI.Register(apiMux)
	edge := observability.Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)))(authAPI.Guard(apiMux))
	ts := httptest.NewServer(edge)
	defer ts.Close()

	alice, aliceID := signup(t, ts.URL, "activity-alice@example.com", "activity-alice")
	bob, _ := signup(t, ts.URL, "activity-bob@example.com", "activity-bob")

	// An organization and a project alice owns.
	resp := alice.do(t, http.MethodPost, "/api/v1/organizations",
		`{"slug":"activity-labs","name":"Activity Research"}`)
	mustStatus(t, resp, http.StatusCreated)
	var createdOrg orgResponse
	if err := json.NewDecoder(resp.Body).Decode(&createdOrg); err != nil {
		t.Fatalf("create org payload: %v", err)
	}
	orgID := createdOrg.Organization.ID

	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"activity-mof","name":"Activity MOF","purpose":"screening","visibility":"private","organization_id":"`+orgID+`"}`)
	mustStatus(t, resp, http.StatusCreated)
	var created projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("create project payload: %v", err)
	}
	projectID := created.Project.ID

	// ---------------------------------------------------------------------
	// Phase 1 — the two registries reach one feed, and each row says which
	// it came from.
	// ---------------------------------------------------------------------

	// The project's own creation is one governance row (project.created,
	// written by the projects store in its transaction).
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)

	// An ABORT's governance row, written through the real store in the shape
	// its producer writes (internal/application/aborts/service.go, auditEntry:
	// the object ref, the version pair, the record docs/46:7 requires). This
	// is the row the page's most elaborate rendering path reads — the reason
	// code, the human explanation and the lifecycle move, none of which the
	// event payload carries — so it is the one governance row worth getting
	// from the store rather than asserting the store's own suite covers.
	const abortedObjectID = "0a0b0c0d-3333-4000-8000-000000000004"
	if err := auditStore.Record(ctx, domain.AuditEntry{
		ActorID:       aliceID,
		Via:           domain.ViaSession,
		Action:        domain.ActionScientificObjectAborted,
		TargetRef:     "object:" + abortedObjectID,
		ProjectID:     projectID,
		CorrelationID: "activity-sources-abort",
		BeforeSummary: map[string]any{
			"object_id":       abortedObjectID,
			"version_no":      3,
			"lifecycle_state": "active",
		},
		AfterSummary: map[string]any{
			"object_id":          abortedObjectID,
			"object_type":        "structure",
			"aborted_version_id": "0a0b0c0d-6666-4000-8000-000000000007",
			"aborted_version_no": 3,
			"version_no":         4,
			"lifecycle_state":    "aborted",
			"reason_code":        "contaminated",
			"explanation":        "the sample was contaminated in transit",
		},
	}); err != nil {
		t.Fatalf("record the abort's audit row: %v", err)
	}

	// A release's event and an abort's event, at instants that interleave
	// with it: the abort event is the newest, the release event the oldest.
	// Both are inserted with the ids the page will render, at chosen
	// instants, because a merged order is only testable when the times are
	// known.
	// The abort event is seeded in the shape its producer writes
	// (internal/application/aborts/events.go, abortedEvent): identity and
	// reference only. The human explanation is deliberately NOT in the
	// payload — it is free text, it lives on the version row and on the
	// ABORT'S AUDIT ROW, and a consumer that needs the words reads them by
	// the version id this payload names. A fixture with an explanation in it
	// would have the page reading a field the platform never writes there.
	abortEventID := seedResearchEvent(t, ctx, pool, projectID, aliceID,
		"scientific_object.aborted", "private",
		`{"object_id":"obj-1","object_type":"structure","version_no":4,"state_id":"state-1",`+
			`"branch_id":"branch-1","aborted_version_no":3,"reason_code":"contaminated"}`,
		base.Add(3*time.Minute))
	releaseEventID := seedResearchEvent(t, ctx, pool, projectID, aliceID,
		"release.published", "private",
		`{"release_id":"rel-1","version":"v1.2.3"}`,
		base.Add(1*time.Minute))

	all := mustSourceActivity(t, alice, "/api/v1/projects/"+projectID+"/activity")
	if len(all.Entries) != 4 {
		t.Fatalf("feed entries = %d, want 4 (2 governance + 2 research): %+v", len(all.Entries), all.Entries)
	}

	sources := map[string]activitySourceEntry{}
	for _, e := range all.Entries {
		if e.Source != "governance" && e.Source != "research" {
			t.Fatalf("entry %s source = %q, want governance or research", e.ID, e.Source)
		}
		sources[e.ID] = e
		if e.Action == "" {
			t.Errorf("entry %s: empty action — the page has nothing to render", e.ID)
		}
		if e.Via == "" {
			t.Errorf("entry %s (%s): empty via — the channel is what the row's rendering names", e.ID, e.Action)
		}
		// actor/via links: the actor's identity is joined for rendering.
		if e.ActorHandle == nil || *e.ActorHandle != "activity-alice" {
			t.Errorf("entry %s (%s) actor_handle = %v, want activity-alice", e.ID, e.Action, e.ActorHandle)
		}
		if e.ActorDisplayName == nil || *e.ActorDisplayName != "activity-alice" {
			t.Errorf("entry %s (%s) actor_display_name = %v", e.ID, e.Action, e.ActorDisplayName)
		}
		if e.CorrelationID == "" {
			t.Errorf("entry %s (%s): empty correlation id", e.ID, e.Action)
		}
		if e.ProjectID == nil || *e.ProjectID != projectID {
			t.Errorf("entry %s (%s) project_id = %v, want %s", e.ID, e.Action, e.ProjectID, projectID)
		}
	}

	abort, ok := sources[abortEventID]
	if !ok {
		t.Fatalf("the abort event %s is not in the feed: %+v", abortEventID, all.Entries)
	}
	if abort.Source != "research" || abort.Action != "scientific_object.aborted" {
		t.Errorf("abort event row = %+v, want a research row with its event type", abort)
	}
	if abort.Visibility != "private" {
		t.Errorf("abort event visibility = %q, want the stored private", abort.Visibility)
	}
	if abort.TargetRef != nil {
		t.Errorf("research row target_ref = %v, want NULL (research events have no target ref)", *abort.TargetRef)
	}
	// The display inputs the abort family renders, as the page reads them
	// off the wire: the reason code, the version the abort produced and the
	// version it retracted. Nothing else is required to be present, and in
	// particular no explanation — see the seeding comment above.
	var abortPayload struct {
		ObjectID       string `json:"object_id"`
		VersionNo      int    `json:"version_no"`
		AbortedVersion int    `json:"aborted_version_no"`
		ReasonCode     string `json:"reason_code"`
	}
	if err := json.Unmarshal(abort.Payload, &abortPayload); err != nil {
		t.Fatalf("abort event payload %s: %v", abort.Payload, err)
	}
	if abortPayload.ObjectID != "obj-1" || abortPayload.VersionNo != 4 ||
		abortPayload.AbortedVersion != 3 || abortPayload.ReasonCode != "contaminated" {
		t.Errorf("abort event payload = %+v — the page's abort rendering reads all four", abortPayload)
	}
	if _, ok := sources[releaseEventID]; !ok {
		t.Errorf("the release event %s is not in the feed", releaseEventID)
	}

	// The governance row of the same feed: one summary pair, no payload. The
	// abort's row is the interesting one — the research half of the SAME act
	// is above it in this feed, and the two are read together.
	var governance, abortAudit activitySourceEntry
	for _, e := range all.Entries {
		if e.Source != "governance" {
			continue
		}
		if e.Action == domain.ActionScientificObjectAborted {
			abortAudit = e
			continue
		}
		governance = e
	}
	if governance.Action != "project.created" {
		t.Fatalf("governance row = %+v, want project.created", governance)
	}
	if governance.Payload != nil {
		t.Errorf("governance row payload = %s, want absent (the event body is a research field)", governance.Payload)
	}
	if governance.Visibility != "" {
		t.Errorf("governance row visibility = %q, want empty", governance.Visibility)
	}
	if len(governance.AfterSummary) == 0 {
		t.Error("governance row carries no after_summary — the state it produced is not on the wire")
	}
	if governance.OrganizationID == nil || *governance.OrganizationID != orgID {
		t.Errorf("governance row organization_id = %v, want %s", governance.OrganizationID, orgID)
	}

	// The abort's governance row: what the page renders it from — the object
	// it names, the reason code, the human explanation, and the lifecycle
	// move — all present, and the research fields absent.
	if abortAudit.ID == "" {
		t.Fatalf("the abort's governance row is not in the feed: %+v", all.Entries)
	}
	if abortAudit.TargetRef == nil || *abortAudit.TargetRef != "object:"+abortedObjectID {
		t.Errorf("abort audit target_ref = %v, want object:%s", abortAudit.TargetRef, abortedObjectID)
	}
	if abortAudit.Payload != nil || abortAudit.Visibility != "" {
		t.Errorf("abort audit row carries research fields (payload/visibility): %+v", abortAudit)
	}
	var abortAuditAfter struct {
		ReasonCode   string `json:"reason_code"`
		Explanation  string `json:"explanation"`
		Lifecycle    string `json:"lifecycle_state"`
		VersionNo    int    `json:"version_no"`
		AbortedVerNo int    `json:"aborted_version_no"`
		AbortedVerID string `json:"aborted_version_id"`
	}
	if err := json.Unmarshal(abortAudit.AfterSummary, &abortAuditAfter); err != nil {
		t.Fatalf("abort audit after_summary %s: %v", abortAudit.AfterSummary, err)
	}
	var abortAuditBefore struct {
		Lifecycle string `json:"lifecycle_state"`
	}
	if err := json.Unmarshal(abortAudit.BeforeSummary, &abortAuditBefore); err != nil {
		t.Fatalf("abort audit before_summary %s: %v", abortAudit.BeforeSummary, err)
	}
	if abortAuditAfter.ReasonCode != "contaminated" || abortAuditAfter.Explanation == "" {
		t.Errorf("abort audit after_summary = %+v, want the reason code and the human explanation", abortAuditAfter)
	}
	// The two halves of the same act disagree on purpose about the free text:
	// the governance row carries it, the event does not (aborts/events.go).
	// A page that read the explanation off the event would render an abort
	// with no reason at all.
	if abortAuditBefore.Lifecycle != "active" || abortAuditAfter.Lifecycle != "aborted" {
		t.Errorf("lifecycle move = %q → %q, want active → aborted (the transition the row records)",
			abortAuditBefore.Lifecycle, abortAuditAfter.Lifecycle)
	}
	if abortAuditAfter.AbortedVerID == "" || abortAuditAfter.AbortedVerNo != 3 ||
		abortAuditAfter.VersionNo != 4 {
		t.Errorf("abort audit version pair = %+v, want the aborted version 3 and the recording version 4", abortAuditAfter)
	}

	// Order: newest first across BOTH registries (the abort event at +3m,
	// the governance row at ~now, the release event at +1m... the
	// governance row's own instant is the project's creation time, which is
	// after all three of the base offsets), so the assertion is on the
	// sequence being non-increasing rather than on a hand-computed order.
	for i := 1; i < len(all.Entries); i++ {
		prev, cur := all.Entries[i-1], all.Entries[i]
		if cur.OccurredAt.After(prev.OccurredAt) {
			t.Errorf("entry %d (%s at %s) is newer than entry %d (%s at %s): the merged feed is not newest-first",
				i, cur.Action, cur.OccurredAt, i-1, prev.Action, prev.OccurredAt)
		}
	}

	// ---------------------------------------------------------------------
	// Phase 2 — the source filter selects the registry, and the three
	// answers add up.
	// ---------------------------------------------------------------------
	governed := mustSourceActivity(t, alice, "/api/v1/projects/"+projectID+"/activity?source=governance")
	if len(governed.Entries) == 0 {
		t.Fatal("source=governance returned nothing; the project has a governance row")
	}
	for _, e := range governed.Entries {
		if e.Source != "governance" {
			t.Errorf("source=governance returned a %s row (%s)", e.Source, e.Action)
		}
		if e.Payload != nil || e.Visibility != "" {
			t.Errorf("governance row %s carries research fields (payload/visibility)", e.Action)
		}
	}
	researched := mustSourceActivity(t, alice, "/api/v1/projects/"+projectID+"/activity?source=research")
	if len(researched.Entries) != 2 {
		t.Fatalf("source=research entries = %d, want 2", len(researched.Entries))
	}
	for _, e := range researched.Entries {
		if e.Source != "research" {
			t.Errorf("source=research returned a %s row (%s)", e.Source, e.Action)
		}
		if len(e.Payload) == 0 {
			t.Errorf("research row %s carries no payload — the event body is the row", e.Action)
		}
		if e.BeforeSummary != nil || e.AfterSummary != nil || e.Metadata != nil {
			t.Errorf("research row %s carries audit summaries (before/after/metadata)", e.Action)
		}
	}
	if len(governed.Entries)+len(researched.Entries) != len(all.Entries) {
		t.Errorf("the filter does not partition the feed: %d governance + %d research != %d unfiltered",
			len(governed.Entries), len(researched.Entries), len(all.Entries))
	}

	// An unknown source is refused, never silently widened to "both" and
	// never answered as an empty page: a reader who mistyped a value must
	// not be told the project has no such history.
	for _, bad := range []string{"audit", "events", "all", "Governance", " governance"} {
		resp := alice.do(t, http.MethodGet,
			"/api/v1/projects/"+projectID+"/activity?source="+url.QueryEscape(bad), "")
		mustStatus(t, resp, http.StatusBadRequest)
		mustEnvelope(t, resp, "VALIDATION_FAILED")
	}

	// Visibility of a denied read is unchanged by the filter (the source
	// selects a registry, it never widens who may read it).
	resp = bob.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/activity?source=research", "")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "PROJECT_NOT_FOUND")

	// ---------------------------------------------------------------------
	// Phase 3 — the organization feed is governance-only, and says so.
	// ---------------------------------------------------------------------
	orgPage := mustSourceActivity(t, alice, "/api/v1/organizations/"+orgID+"/activity")
	if len(orgPage.Entries) == 0 {
		t.Fatal("the org feed is empty; the org was created and a project added under it")
	}
	for _, e := range orgPage.Entries {
		if e.Source != "governance" {
			t.Errorf("org feed row %s source = %q, want governance", e.Action, e.Source)
		}
	}
	orgGoverned := mustSourceActivity(t, alice, "/api/v1/organizations/"+orgID+"/activity?source=governance")
	if len(orgGoverned.Entries) != len(orgPage.Entries) {
		t.Errorf("org feed with source=governance = %d rows, without = %d; the filter must be the same feed",
			len(orgGoverned.Entries), len(orgPage.Entries))
	}
	// research is refused, not answered empty: research events are
	// project-scoped, so "the organization's research events" is not a gap
	// in the data, it is a query this schema cannot answer.
	resp = alice.do(t, http.MethodGet, "/api/v1/organizations/"+orgID+"/activity?source=research", "")
	mustStatus(t, resp, http.StatusBadRequest)
	mustEnvelope(t, resp, "VALIDATION_FAILED")

	// ---------------------------------------------------------------------
	// Phase 4 — one window: pages of a merged feed neither duplicate nor
	// drop rows, including across a pair that shares an instant.
	// ---------------------------------------------------------------------

	// A second project, seeded so the walk has to cross a tie: one
	// governance row and one research event at the SAME occurred_at, with
	// ids ordered the other way round from their sources (the event's id
	// sorts after the audit row's), so only the (occurred_at, id) tie-break
	// can produce the right sequence.
	tieProject := projectID // reuse the scope, with an instant of its own
	tie := base.Add(-10 * time.Minute)
	if _, err := pool.Exec(ctx, `
		INSERT INTO audit_log (id, actor_id, via, action, correlation_id, project_id, occurred_at, metadata)
		VALUES ('00000000-0000-4000-8000-00000000a001', $1, 'session', 'release.created',
		        'activity-sources-tie', $2, $3, '{}'::jsonb)`,
		aliceID, tieProject, tie); err != nil {
		t.Fatalf("seed tie audit row: %v", err)
	}
	tieEventID := "00000000-0000-4000-8000-00000000e002"
	if _, err := pool.Exec(ctx, `
		INSERT INTO research_events (id, event_type, actor_id, project_id, visibility, payload, correlation_id, occurred_at, via)
		VALUES ($1, 'scientific_object.reopened', $2, $3, 'private', '{"object_id":"obj-1"}'::jsonb,
		        'activity-sources-tie', $4, 'session')`,
		tieEventID, aliceID, tieProject, tie); err != nil {
		t.Fatalf("seed tie research event: %v", err)
	}

	// Walk the whole feed one row at a time. A merged feed is one window;
	// two independent LIMITs would repeat or skip rows exactly here.
	seen := map[string]int{}
	var order []string
	cursor := ""
	pages := 0
	for {
		path := "/api/v1/projects/" + projectID + "/activity?limit=1"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		page := mustSourceActivity(t, alice, path)
		pages++
		if pages > 20 {
			t.Fatalf("the walk did not terminate after 20 pages; last cursor %q — the keyset is not progressing", cursor)
		}
		if len(page.Entries) == 0 {
			break
		}
		e := page.Entries[0]
		seen[e.ID]++
		if seen[e.ID] > 1 {
			t.Fatalf("entry %s (%s) came back twice in one walk", e.ID, e.Action)
		}
		order = append(order, e.ID)
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	// 6 rows: project.created + the abort's audit row + 2 seeded events +
	// the tie pair.
	if len(seen) != 6 {
		t.Fatalf("the walk returned %d distinct rows, want 6 (ids %v)", len(seen), order)
	}
	if seen[tieEventID] != 1 || seen["00000000-0000-4000-8000-00000000a001"] != 1 {
		t.Errorf("the tied pair was not returned exactly once each: %v", seen)
	}
	// The tie-break: at equal instants, the larger id comes first, so the
	// event (e002) precedes the audit row (a001).
	tieEvent, tieAudit := -1, -1
	for i, id := range order {
		switch id {
		case tieEventID:
			tieEvent = i
		case "00000000-0000-4000-8000-00000000a001":
			tieAudit = i
		}
	}
	if tieEvent < 0 || tieAudit < 0 {
		t.Fatalf("the tied pair is missing from the walk (%d, %d): %v", tieEvent, tieAudit, order)
	}
	if tieEvent+1 != tieAudit {
		t.Errorf("at one instant the order is %v; the event (…e002) must come directly before the audit row (…a001) — equal instants are broken by id DESC",
			order)
	}

	// The same walk at a page size larger than one. At limit=1 the first and
	// the last row of a page ARE the same row, so a cursor taken from the
	// wrong end of the page — the newest row instead of the oldest — is
	// invisible there: it produces the identical cursor. The real page asks
	// for 25 rows at a time and appends the next window, so this walk is the
	// realistic one, and it is where that defect shows up as a repeated row.
	wideSeen := map[string]int{}
	var wideOrder []string
	wideCursor := ""
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatalf("the limit=3 walk did not terminate after 20 pages; last cursor %q — the keyset is not progressing", wideCursor)
		}
		path := "/api/v1/projects/" + projectID + "/activity?limit=3"
		if wideCursor != "" {
			path += "&cursor=" + url.QueryEscape(wideCursor)
		}
		page := mustSourceActivity(t, alice, path)
		if len(page.Entries) == 0 {
			break
		}
		for _, e := range page.Entries {
			wideSeen[e.ID]++
			if wideSeen[e.ID] > 1 {
				t.Fatalf("entry %s (%s) came back twice in the limit=3 walk: pages must not overlap", e.ID, e.Action)
			}
			wideOrder = append(wideOrder, e.ID)
		}
		if page.NextCursor == nil {
			break
		}
		wideCursor = *page.NextCursor
	}
	if len(wideSeen) != 6 {
		t.Fatalf("the limit=3 walk returned %d distinct rows, want 6 (ids %v)", len(wideSeen), wideOrder)
	}
	if strings.Join(wideOrder, ",") != strings.Join(order, ",") {
		t.Errorf("the limit=3 walk returned %v; the limit=1 walk returned %v — a page size must not change the rows or their order",
			wideOrder, order)
	}

	// The same walk filtered to one registry must terminate too, and return
	// exactly that registry's rows.
	for _, tc := range []struct {
		source string
		want   int
	}{
		{"governance", 3}, // project.created + the abort row + the tie audit row
		{"research", 3},   // the abort, the release and the reopen events
	} {
		got := 0
		cursor := ""
		for {
			// limit=2, not 1: a filtered page of one row cannot tell a cursor
			// taken from the top of the page from one taken from its bottom.
			path := fmt.Sprintf("/api/v1/projects/%s/activity?limit=2&source=%s", projectID, tc.source)
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			page := mustSourceActivity(t, alice, path)
			got += len(page.Entries)
			if page.NextCursor == nil || len(page.Entries) == 0 {
				break
			}
			cursor = *page.NextCursor
		}
		if got != tc.want {
			t.Errorf("paged through %d %s rows, want %d", got, tc.source, tc.want)
		}
	}
}
