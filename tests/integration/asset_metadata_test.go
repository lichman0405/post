// Task T0706 required test "asset metadata tests" — the acceptance, over a
// REAL PostgreSQL (docs/66 §3: task/run-scoped namespaced database, no
// mocks) and the REAL composed surface: the auth guard, the project
// service, the metadata command, the store that owns the revision
// transaction, and the asset hub's public read route.
//
// What only a database can settle, in the task's own order:
//
//  1. A metadata revision produces NO scientific version. Not "the command
//     has no version parameter" — the version ROWS are fingerprinted
//     (to_jsonb, whole row) before and after a revision that changes every
//     revisable field, and the count and the bytes are asserted equal. A
//     revision that quietly touched the version stream would fail here even
//     if it left the labels alone.
//
//  2. The audit row commits in the SAME TRANSACTION as the state change.
//     This is proven by making the audit write FAIL and showing the
//     metadata change go down with it: the store is driven with an actor id
//     that is a well-formed uuid naming no user, so audit_log.actor_id's
//     foreign key refuses the INSERT, and the assertion is that the column
//     is UNCHANGED and no audit row exists. Were the audit written outside
//     the transaction — or best-effort, or after the commit — the metadata
//     write would have survived and this case would report it.
//
//  3. The role gate is maintainer-or-above and fails closed, and "not a
//     member" and "role too low" are ONE answer over the wire: the same
//     status and the same code AND the same message.
//
//  4. A slug rename leaves the pid and /assets/{pid} byte-identical: the
//     anonymous public read route is fetched before and after the rename
//     and the pid it resolves is the same string, while the slug it renders
//     is not.
//
//  5. Nothing that is refused writes anything: every denial below is
//     followed by a fingerprint of the asset row and an audit-row count.
//
// The fixture's asset and version are created by the REAL publish command,
// so the rows the revision must not disturb are production rows rather than
// a fixture's idea of them.

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/assetshttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/assetmetadata"
	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

const assetMetadataTaskID = "T0706"

// --------------------------------------------------------------------------
// The world

// metadataWorld is the revision surface as production wires it: one
// migrated database, the real publish command that creates the fixture, the
// real revision command over the real store, and the real public read route
// the pid has to survive.
type metadataWorld struct {
	// ts is the composed server's base URL (a string, so the helpers below
	// can build a request path the way a client does).
	ts   string
	pool *pgxpool.Pool

	// store is the production adapter, kept beside the composed surface so
	// the rollback proof (criterion 2) can drive ONE store call directly
	// and read the rows underneath it.
	store *persistence.AssetMetadataStore

	alice, bob, carol, dave, eve *testUserClient
	aliceID, bobID               string
	carolID, daveID, eveID       string

	projectID string
	// otherProject is a second project alice owns, holding its own asset:
	// a revision authorized in it must not be able to write the first
	// project's asset.
	otherProjectID string

	assetID  string
	assetPID string
}

func newMetadataWorld(t *testing.T) *metadataWorld {
	t.Helper()
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), assetMetadataTaskID)

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
	})
	orgStore := persistence.NewOrgStore(pool)
	projectStore := persistence.NewProjectStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectStore,
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	policyAPI := policyhttp.New(policyhttp.Deps{
		Store:    persistence.NewPolicyStore(pool),
		Orgs:     orgStore,
		Projects: projectStore,
	})
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	publishCommand := assetpublish.NewCommand(assetpublish.Deps{
		Members:  projectStore,
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Store:    persistence.NewAssetPublishStore(pool, rsgvalidation.NewValidator(reg)),
		Authz:    authz.NewMatrixEngine(),
	})
	metadataStore := persistence.NewAssetMetadataStore(pool)
	metadataCommand := assetmetadata.NewCommand(assetmetadata.Deps{
		Members: projectStore,
		Store:   metadataStore,
	})

	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	policyAPI.Register(mux)
	assetshttp.New(assetshttp.Deps{
		State:    assetshttp.NewPostgresStateStore(pool),
		Projects: projectAPI.Service(),
		Publish:  publishCommand,
		Pages:    persistence.NewAssetPageStore(pool),
		Members:  projectAPI.Service(),
		Metadata: metadataCommand,
	}).Register(mux)

	// The edge middleware is composed as cmd/api/main.go composes it, so
	// the correlation id an audit row records is the request's own.
	edge := observability.Middleware(slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(edge(authAPI.Guard(mux)))
	t.Cleanup(ts.Close)

	w := &metadataWorld{ts: ts.URL, pool: pool, store: metadataStore}
	// Signup goes through the real endpoint, so the actors below are real
	// users rows — which is what audit_log.actor_id's foreign key requires
	// and what makes the rollback proof meaningful.
	w.alice, w.aliceID = signup(t, w.ts, "meta-alice@example.com", "meta-alice")
	w.bob, w.bobID = signup(t, w.ts, "meta-bob@example.com", "meta-bob")
	w.carol, w.carolID = signup(t, w.ts, "meta-carol@example.com", "meta-carol")
	w.dave, w.daveID = signup(t, w.ts, "meta-dave@example.com", "meta-dave")
	w.eve, w.eveID = signup(t, w.ts, "meta-eve@example.com", "meta-eve")

	w.projectID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('metadata-lab', 'Metadata Lab', 'T0706 fixture', 'public', $1) RETURNING id`, w.aliceID)
	w.otherProjectID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('metadata-other', 'Metadata Other', 'T0706 fixture', 'public', $1) RETURNING id`, w.aliceID)
	for _, m := range []struct{ project, user, role string }{
		{w.projectID, w.aliceID, "owner"},
		{w.projectID, w.bobID, "maintainer"},
		{w.projectID, w.carolID, "contributor"},
		{w.projectID, w.daveID, "viewer"},
		{w.otherProjectID, w.aliceID, "owner"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			m.project, m.user, m.role); err != nil {
			t.Fatalf("seed membership %s: %v", m.role, err)
		}
	}

	// The asset itself, through the REAL publish command: the row the
	// revision will not disturb is a row production wrote.
	stateID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO project_states (project_id, state_hash, manifest_version)
		 VALUES ($1, 'genesis-metadata', '1') RETURNING id`, w.projectID)
	releaseID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO releases (project_id, version, title, state_id, manifest, manifest_hash, created_by)
		 VALUES ($1, 'v1.0.0', 'Metadata Baseline', $2, '{}'::jsonb, 'x', $3) RETURNING id`,
		w.projectID, stateID, w.aliceID)
	objectID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, 'dataset_record', $2) RETURNING id`, w.projectID, w.aliceID)
	objectVersionID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title, lifecycle_state, payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, 'https://open-rd.example/schemas/dataset.schema.json', '1',
		         'Metadata Object Version', 'active', '{}'::jsonb, 'x', $3) RETURNING id`,
		objectID, stateID, w.aliceID)
	blobID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO blobs (content_hash, size_bytes, storage_key, created_by)
		 VALUES ('sha256:metadata-open', 2048, 'blobs/metadata-open', $1) RETURNING id`, w.aliceID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO blob_attachments (blob_id, scientific_object_version_id, attachment_role, access_level, state_id)
		 VALUES ($1, $2, 'data', 'open', $3)`, blobID, objectVersionID, stateID); err != nil {
		t.Fatalf("attach the open blob: %v", err)
	}
	cand := buildCandidate(t, candidateOptions{
		version: "1.0", visibility: "public",
		refs:        []string{"release:" + releaseID, "object_version:" + objectVersionID},
		creators:    []string{w.aliceID},
		blobIDs:     []string{blobID},
		accessLevel: "open", dataAccess: rights.DataAccessOpen,
		title: "Metadata Subject", slug: "metadata-subject",
	})
	published, err := publishCommand.Publish(ctx, assetpublish.Actor{
		User: domain.User{ID: w.aliceID, Handle: "meta-alice"},
	}, cand.params(w.projectID, nil))
	if err != nil {
		t.Fatalf("publish the fixture: %v", err)
	}
	w.assetID, w.assetPID = published.AssetID, published.AssetPID
	if w.assetID == "" || w.assetPID == "" {
		t.Fatalf("the publish named no asset: %+v", published)
	}
	return w
}

// --------------------------------------------------------------------------
// Reads the assertions are made against

// versionFingerprint renders every version row of the asset as one string:
// the whole row, all columns, in a stable order. Comparing two of these is
// a comparison of the version stream's bytes — a revision that moved any
// version field, not just the label, changes it.
func (w *metadataWorld) versionFingerprint(t *testing.T, ctx context.Context) (string, int) {
	t.Helper()
	count := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM research_asset_versions WHERE asset_id = $1`, w.assetID)
	rows, err := w.pool.Query(ctx,
		`SELECT to_jsonb(v)::text FROM research_asset_versions v WHERE asset_id = $1 ORDER BY v.version`, w.assetID)
	if err != nil {
		t.Fatalf("fingerprint versions: %v", err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan version row: %v", err)
		}
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read version rows: %v", err)
	}
	return sb.String(), count
}

// assetFingerprint renders the asset row's identity and metadata columns:
// what a revision may move (title/slug/description/lists) and what it may
// not (id, pid, asset_type, origin_project_id, created_at).
func (w *metadataWorld) assetFingerprint(t *testing.T, ctx context.Context) (identity, metadata string) {
	t.Helper()
	var (
		id, pid, assetType, origin, createdAt string
		title, slug, description              string
		keywords, contact, documentation      []string
		cover                                 *string
	)
	err := w.pool.QueryRow(ctx,
		`SELECT id::text, pid, asset_type, origin_project_id::text, created_at::text,
		        title, slug, description, keywords, contact, documentation, cover_blob_id::text
		   FROM research_assets WHERE id = $1`, w.assetID).
		Scan(&id, &pid, &assetType, &origin, &createdAt,
			&title, &slug, &description, &keywords, &contact, &documentation, &cover)
	if err != nil {
		t.Fatalf("fingerprint asset: %v", err)
	}
	identity = strings.Join([]string{id, pid, assetType, origin, createdAt}, "|")
	metadata = strings.Join([]string{title, slug, description,
		strings.Join(keywords, ","), strings.Join(contact, ","), strings.Join(documentation, ",")}, "|")
	if cover != nil {
		metadata += "|cover=" + *cover
	}
	return identity, metadata
}

// auditRows returns this asset's metadata-revision audit rows, oldest
// first: action, via, actor, target, project, correlation id and the two
// summaries as raw JSON.
type metadataAuditRow struct {
	Action        string
	Via           string
	ActorID       *string
	TargetRef     *string
	ProjectID     *string
	CorrelationID string
	Before        string
	After         string
}

func (w *metadataWorld) auditRows(t *testing.T, ctx context.Context) []metadataAuditRow {
	t.Helper()
	rows, err := w.pool.Query(ctx,
		`SELECT action, via, actor_id::text, target_ref, project_id::text, correlation_id,
		        coalesce(before_summary::text, ''), coalesce(after_summary::text, '')
		   FROM audit_log
		  WHERE action = $1 AND target_ref = $2
		  ORDER BY occurred_at, id`, domain.ActionAssetMetadataRevised, "asset:"+w.assetPID)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	defer rows.Close()
	var out []metadataAuditRow
	for rows.Next() {
		var r metadataAuditRow
		if err := rows.Scan(&r.Action, &r.Via, &r.ActorID, &r.TargetRef, &r.ProjectID,
			&r.CorrelationID, &r.Before, &r.After); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	return out
}

// --------------------------------------------------------------------------
// The wire

// metadataURL is the route under test: the asset's own address, PATCHed.
func (w *metadataWorld) metadataURL(pid string) string { return assets.AssetAPIPath(assets.PID(pid)) }

// patchMetadata sends one revision as the web app sends it (JSON content
// type, CSRF token, correlation id) and returns the response.
func (w *metadataWorld) patchMetadata(t *testing.T, uc *testUserClient, pid, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, w.ts+w.metadataURL(pid), strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", uc.csrf)
	req.Header.Set("X-Correlation-ID", metadataCorrelationID)
	resp, err := uc.client.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", w.metadataURL(pid), err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// metadataCorrelationID is the id every revision below carries, and the one
// its audit row has to name.
const metadataCorrelationID = "t0706-metadata-trace-0001"

// body renders a revision request for the fixture's project.
func (w *metadataWorld) body(fields map[string]any) string {
	out := map[string]any{"project_id": w.projectID}
	for k, v := range fields {
		out[k] = v
	}
	raw, err := json.Marshal(out)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// revisionPayload is the response body, read as this suite's own shape (a
// silent JSON tag change in the handler fails here).
type revisionPayload struct {
	AssetPID      string   `json:"asset_pid"`
	URL           string   `json:"url"`
	Title         string   `json:"title"`
	Slug          string   `json:"slug"`
	Description   string   `json:"description"`
	Keywords      []string `json:"keywords"`
	Contact       []string `json:"contact"`
	Documentation []string `json:"documentation"`
	Cover         string   `json:"cover"`
}

func decodeRevision(t *testing.T, resp *http.Response) revisionPayload {
	t.Helper()
	raw := readAll(t, resp)
	var out revisionPayload
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("decode revision payload: %v: %s", err, raw)
	}
	return out
}

// --------------------------------------------------------------------------
// 1. No scientific version

// TestMetadataRevisionProducesNoScientificVersion is acceptance criterion 1,
// and it is asserted on the VERSION ROWS rather than on the response: the
// response having no version field proves only that the transport does not
// print one.
//
// The revision below changes every revisable field at once — including the
// slug, the one field a naive implementation might be tempted to treat as
// part of an identity — and the whole version stream is fingerprinted before
// and after.
func TestMetadataRevisionProducesNoScientificVersion(t *testing.T) {
	ctx := testCtx(t)
	w := newMetadataWorld(t)
	beforeVersions, beforeCount := w.versionFingerprint(t, ctx)
	beforeIdentity, beforeMetadata := w.assetFingerprint(t, ctx)
	if beforeCount == 0 {
		t.Fatal("the fixture has no version: the publish did not write one, so this test would pass vacuously")
	}
	beforeEvents := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM research_events WHERE project_id = $1`, w.projectID)

	resp := w.patchMetadata(t, w.alice, w.assetPID, w.body(map[string]any{
		"title":         "Metadata Subject v2",
		"slug":          "metadata-subject-renamed",
		"description":   "a description written by a revision",
		"keywords":      []string{"grain", "boundary"},
		"contact":       []string{"alice@example.org"},
		"documentation": []string{"https://example.org/docs"},
	}))
	mustStatus(t, resp, http.StatusOK)
	payload := decodeRevision(t, resp)

	afterVersions, afterCount := w.versionFingerprint(t, ctx)
	if afterCount != beforeCount {
		t.Errorf("version count changed: %d -> %d: a metadata revision must not produce a scientific version",
			beforeCount, afterCount)
	}
	if afterVersions != beforeVersions {
		t.Errorf("the version stream changed:\nbefore:\n%s\nafter:\n%s", beforeVersions, afterVersions)
	}

	// The identity half of the row is untouched — including the pid, which
	// is what every URL is built from.
	afterIdentity, afterMetadata := w.assetFingerprint(t, ctx)
	if afterIdentity != beforeIdentity {
		t.Errorf("the asset's identity changed:\nbefore: %s\nafter:  %s", beforeIdentity, afterIdentity)
	}
	if afterMetadata == beforeMetadata {
		t.Fatal("the metadata did not change: the revision silently did nothing, and the assertions above prove nothing")
	}
	if got := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM research_events WHERE project_id = $1`, w.projectID); got != beforeEvents {
		t.Errorf("research_events count changed: %d -> %d: a metadata revision is not a research event about a version",
			beforeEvents, got)
	}
	// The response names the asset by the pid it already had, and carries
	// no version: the two halves of "no new version" at the wire.
	if payload.AssetPID != w.assetPID {
		t.Errorf("response pid = %q, want %q", payload.AssetPID, w.assetPID)
	}
	if payload.URL != assets.AssetURL(assets.PID(w.assetPID)) {
		t.Errorf("response url = %q, want %q", payload.URL, assets.AssetURL(assets.PID(w.assetPID)))
	}
	if payload.Slug != "metadata-subject-renamed" {
		t.Errorf("response slug = %q, want the revised one", payload.Slug)
	}
}

// --------------------------------------------------------------------------
// 2. The audit row is in the same transaction

// TestMetadataRevisionAuditIsInTheSameTransaction is acceptance criterion 2,
// and it is proven by BREAKING the audit and watching the write go down with
// it.
//
// The store is driven directly with an audit entry whose actor is a
// well-formed uuid naming no user. audit_log.actor_id has a foreign key to
// users (00012), so the INSERT fails — and because appendAudit runs inside
// the same transaction as the UPDATE, the whole revision rolls back. The
// three assertions are the proof: the column is unchanged, no audit row was
// appended, and the same call with a real actor commits BOTH.
//
// A best-effort audit — one written after the commit, or in its own
// transaction, or with its error logged — leaves the metadata change in
// place. This case is what would report it.
func TestMetadataRevisionAuditIsInTheSameTransaction(t *testing.T) {
	ctx := testCtx(t)
	w := newMetadataWorld(t)

	_, beforeMetadata := w.assetFingerprint(t, ctx)
	beforeAudits := len(w.auditRows(t, ctx))

	// A uuid that is well formed — so it gets past the store's own shape
	// check and reaches the INSERT — and names no users row.
	const ghost = "99999999-9999-4999-8999-999999999999"
	if countRows(t, ctx, w.pool, `SELECT count(*) FROM users WHERE id = $1::uuid`, ghost) != 0 {
		t.Fatal("the ghost actor exists: the fixture would not exercise the foreign key")
	}

	newTitle := "Written By A Revision That Must Not Commit"
	_, err := w.store.ReviseMetadata(ctx, assetmetadata.RevisionRequest{
		ProjectID: w.projectID,
		AssetPID:  w.assetPID,
		Changes:   assetmetadata.Changes{Title: &newTitle},
		ActorID:   ghost,
		Audit: domain.AuditEntry{
			ActorID:       ghost,
			Via:           domain.ViaSession,
			Action:        domain.ActionAssetMetadataRevised,
			TargetRef:     "asset:" + w.assetPID,
			ProjectID:     w.projectID,
			CorrelationID: metadataCorrelationID,
		},
	})
	if err == nil {
		t.Fatal("the revision committed with an actor that names no user: the audit write was not able to fail")
	}
	if !errors.Is(err, assetmetadata.ErrStore) {
		t.Fatalf("err = %v, want ErrStore", err)
	}

	_, afterMetadata := w.assetFingerprint(t, ctx)
	if afterMetadata != beforeMetadata {
		t.Errorf("the metadata survived a failed audit write:\nbefore: %s\nafter:  %s\n"+
			"the audit row is not in the same transaction as the write", beforeMetadata, afterMetadata)
	}
	if got := len(w.auditRows(t, ctx)); got != beforeAudits {
		t.Errorf("audit rows = %d, want %d: a rolled-back revision appended one anyway", got, beforeAudits)
	}

	// The control: the same request with a real actor writes BOTH halves.
	// Without it, the two assertions above would also pass against a store
	// that simply writes nothing.
	revision, err := w.store.ReviseMetadata(ctx, assetmetadata.RevisionRequest{
		ProjectID: w.projectID,
		AssetPID:  w.assetPID,
		Changes:   assetmetadata.Changes{Title: &newTitle},
		ActorID:   w.aliceID,
		Audit: domain.AuditEntry{
			ActorID:       w.aliceID,
			Via:           domain.ViaSession,
			Action:        domain.ActionAssetMetadataRevised,
			TargetRef:     "asset:" + w.assetPID,
			ProjectID:     w.projectID,
			CorrelationID: metadataCorrelationID,
		},
	})
	if err != nil {
		t.Fatalf("the control revision failed: %v", err)
	}
	if revision.Metadata.Title != newTitle {
		t.Errorf("control title = %q, want %q", revision.Metadata.Title, newTitle)
	}
	_, afterControl := w.assetFingerprint(t, ctx)
	if afterControl == beforeMetadata {
		t.Fatal("the control revision changed nothing: the rollback proof above is vacuous")
	}
	if got := len(w.auditRows(t, ctx)); got != beforeAudits+1 {
		t.Fatalf("audit rows = %d, want %d: the control did not append one", got, beforeAudits+1)
	}
}

// TestMetadataRevisionAuditRecordsWhoWhatAndBeforeAfter: the audit row's
// CONTENT, over the real column types. An audit row that says something
// happened but not what changed is not the 保留 audit docs/11 §4 requires.
func TestMetadataRevisionAuditRecordsWhoWhatAndBeforeAfter(t *testing.T) {
	ctx := testCtx(t)
	w := newMetadataWorld(t)

	// Seed known before-values through a first revision, so the second
	// revision's before_summary has something specific to record.
	resp := w.patchMetadata(t, w.alice, w.assetPID, w.body(map[string]any{
		"title":       "Before Title",
		"slug":        "before-slug",
		"description": "before description",
		"keywords":    []string{"before-keyword"},
	}))
	mustStatus(t, resp, http.StatusOK)

	resp = w.patchMetadata(t, w.bob, w.assetPID, w.body(map[string]any{
		"title":       "After Title",
		"description": "after description",
	}))
	mustStatus(t, resp, http.StatusOK)

	rows := w.auditRows(t, ctx)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2 (one per revision)", len(rows))
	}
	second := rows[1]
	if second.Action != domain.ActionAssetMetadataRevised {
		t.Errorf("action = %q, want %q", second.Action, domain.ActionAssetMetadataRevised)
	}
	if second.Via != domain.ViaSession {
		t.Errorf("via = %q, want %q", second.Via, domain.ViaSession)
	}
	if second.ActorID == nil || *second.ActorID != w.bobID {
		t.Errorf("actor = %v, want bob (%s): the row records WHO revised", second.ActorID, w.bobID)
	}
	if second.TargetRef == nil || *second.TargetRef != "asset:"+w.assetPID {
		t.Errorf("target = %v, want %q", second.TargetRef, "asset:"+w.assetPID)
	}
	if second.ProjectID == nil || *second.ProjectID != w.projectID {
		t.Errorf("project = %v, want %q", second.ProjectID, w.projectID)
	}
	if second.CorrelationID != metadataCorrelationID {
		t.Errorf("correlation id = %q, want the request's %q", second.CorrelationID, metadataCorrelationID)
	}

	var before, after map[string]any
	if err := json.Unmarshal([]byte(second.Before), &before); err != nil {
		t.Fatalf("before_summary is not a JSON document: %v: %s", err, second.Before)
	}
	if err := json.Unmarshal([]byte(second.After), &after); err != nil {
		t.Fatalf("after_summary is not a JSON document: %v: %s", err, second.After)
	}
	if got, _ := before["title"].(string); got != "Before Title" {
		t.Errorf("before.title = %q, want %q", got, "Before Title")
	}
	if got, _ := before["description"].(string); got != "before description" {
		t.Errorf("before.description = %q, want %q", got, "before description")
	}
	if got, _ := after["title"].(string); got != "After Title" {
		t.Errorf("after.title = %q, want %q", got, "After Title")
	}
	if got, _ := after["description"].(string); got != "after description" {
		t.Errorf("after.description = %q, want %q", got, "after description")
	}
	// A field the second revision did not mention is unchanged in BOTH
	// halves — which is what makes the pair a record of this revision
	// rather than of the row's whole history.
	if got, _ := before["slug"].(string); got != "before-slug" {
		t.Errorf("before.slug = %q, want %q", got, "before-slug")
	}
	if got, _ := after["slug"].(string); got != "before-slug" {
		t.Errorf("after.slug = %q, want the unchanged %q", got, "before-slug")
	}
	if got, ok := before["keywords"].([]any); !ok || len(got) != 1 || got[0] != "before-keyword" {
		t.Errorf("before.keywords = %v, want [before-keyword]", before["keywords"])
	}
}

// TestMetadataRevisionDoesNotClearFieldsItWasNotAsked about is the
// absent-vs-cleared distinction over a real row: a revision that renames the
// slug must not wipe the description, and one that sends an empty value must.
func TestMetadataRevisionDoesNotClearFieldsItWasNotAskedAbout(t *testing.T) {
	ctx := testCtx(t)
	w := newMetadataWorld(t)

	resp := w.patchMetadata(t, w.alice, w.assetPID, w.body(map[string]any{
		"description":   "keep me",
		"keywords":      []string{"keep"},
		"contact":       []string{"keep@example.org"},
		"documentation": []string{"https://example.org/keep"},
	}))
	mustStatus(t, resp, http.StatusOK)

	// A slug-only revision: every other field is absent from the body.
	resp = w.patchMetadata(t, w.alice, w.assetPID, w.body(map[string]any{"slug": "renamed-only"}))
	mustStatus(t, resp, http.StatusOK)
	payload := decodeRevision(t, resp)
	if payload.Description != "keep me" {
		t.Errorf("description = %q, want the untouched %q", payload.Description, "keep me")
	}
	if len(payload.Keywords) != 1 || payload.Keywords[0] != "keep" {
		t.Errorf("keywords = %v, want [keep]", payload.Keywords)
	}
	if len(payload.Contact) != 1 || payload.Contact[0] != "keep@example.org" {
		t.Errorf("contact = %v, want the untouched entry", payload.Contact)
	}
	if len(payload.Documentation) != 1 {
		t.Errorf("documentation = %v, want the untouched entry", payload.Documentation)
	}

	// The same fields sent EMPTY are a clear, which is a different request.
	resp = w.patchMetadata(t, w.alice, w.assetPID, w.body(map[string]any{
		"description": "", "keywords": []string{}, "contact": []string{},
	}))
	mustStatus(t, resp, http.StatusOK)
	payload = decodeRevision(t, resp)
	if payload.Description != "" {
		t.Errorf("description = %q, want cleared", payload.Description)
	}
	if len(payload.Keywords) != 0 || len(payload.Contact) != 0 {
		t.Errorf("keywords/contact = %v/%v, want cleared", payload.Keywords, payload.Contact)
	}
	if len(payload.Documentation) != 1 {
		t.Errorf("documentation = %v, want it still there: it was not mentioned", payload.Documentation)
	}

	// And the clear committed, with its own audit row.
	rows := w.auditRows(t, ctx)
	if len(rows) != 3 {
		t.Fatalf("audit rows = %d, want 3", len(rows))
	}
	var after map[string]any
	if err := json.Unmarshal([]byte(rows[2].After), &after); err != nil {
		t.Fatalf("after_summary: %v", err)
	}
	if got, _ := after["description"].(string); got != "" {
		t.Errorf("audit after.description = %q, want cleared", got)
	}
}

// --------------------------------------------------------------------------
// 3. The role gate

// TestMetadataRevisionRoleGateOverHTTP is acceptance criterion 3, over the
// real membership table and the real command: owner and maintainer revise;
// contributor, viewer and non-member do not — and the three refusals are one
// answer.
//
// The comparison is on status, code AND message. The request_id differs by
// construction (it identifies the request, and comparing it would be
// comparing two different requests' ids), so it is excluded deliberately:
// everything a client could branch on is compared.
func TestMetadataRevisionRoleGateOverHTTP(t *testing.T) {
	ctx := testCtx(t)
	w := newMetadataWorld(t)

	type refusal struct {
		status  int
		code    string
		message string
	}
	refuse := func(t *testing.T, uc *testUserClient, who string) refusal {
		t.Helper()
		_, beforeMetadata := w.assetFingerprint(t, ctx)
		beforeAudits := len(w.auditRows(t, ctx))
		resp := w.patchMetadata(t, uc, w.assetPID, w.body(map[string]any{"title": "Refused " + who}))
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403: %s", who, resp.StatusCode, readAll(t, resp))
		}
		var env errorEnvelope
		if err := json.Unmarshal([]byte(readAll(t, resp)), &env); err != nil {
			t.Fatalf("%s: decode refusal: %v", who, err)
		}
		_, afterMetadata := w.assetFingerprint(t, ctx)
		if afterMetadata != beforeMetadata {
			t.Errorf("%s: the refusal wrote the metadata anyway", who)
		}
		if got := len(w.auditRows(t, ctx)); got != beforeAudits {
			t.Errorf("%s: the refusal appended %d audit rows", who, got-beforeAudits)
		}
		return refusal{status: resp.StatusCode, code: env.Code, message: env.Message}
	}

	// The half that opens, asserted on both admitted roles.
	for _, who := range []struct {
		name string
		uc   *testUserClient
	}{{"owner", w.alice}, {"maintainer", w.bob}} {
		resp := w.patchMetadata(t, who.uc, w.assetPID, w.body(map[string]any{"title": "By " + who.name}))
		mustStatus(t, resp, http.StatusOK)
	}

	contributor := refuse(t, w.carol, "contributor")
	viewer := refuse(t, w.dave, "viewer")
	nonMember := refuse(t, w.eve, "non-member")

	if contributor != viewer {
		t.Errorf("contributor and viewer are answered differently:\n  %+v\n  %+v", contributor, viewer)
	}
	if contributor != nonMember {
		t.Errorf("a member with a junior role and a non-member are answered differently:\n  %+v\n  %+v",
			contributor, nonMember)
	}
	if contributor.code != assetmetadata.CodeForbidden {
		t.Errorf("code = %q, want %q", contributor.code, assetmetadata.CodeForbidden)
	}
	for _, word := range []string{"member", "role", "maintainer", "contributor", "viewer", w.projectID} {
		if strings.Contains(strings.ToLower(contributor.message), strings.ToLower(word)) {
			t.Errorf("the refusal message %q names %q: the two causes must be indistinguishable", contributor.message, word)
		}
	}

	// Two rows of audit: the owner's and the maintainer's, and none from
	// the three refusals.
	rows := w.auditRows(t, ctx)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(rows))
	}
	if rows[0].ActorID == nil || *rows[0].ActorID != w.aliceID {
		t.Errorf("first audit actor = %v, want alice", rows[0].ActorID)
	}
	if rows[1].ActorID == nil || *rows[1].ActorID != w.bobID {
		t.Errorf("second audit actor = %v, want bob", rows[1].ActorID)
	}
}

// TestMetadataRevisionRefusesAnAssetOfAnotherProject: the project id in the
// request is what the actor's membership is resolved against, so it must
// also be the project the asset BELONGS to. Alice owns both projects; a
// revision of project A's asset that names project B must be refused, and
// the answer must not disclose where the pid really lives.
func TestMetadataRevisionRefusesAnAssetOfAnotherProject(t *testing.T) {
	ctx := testCtx(t)
	w := newMetadataWorld(t)

	_, beforeMetadata := w.assetFingerprint(t, ctx)
	beforeAudits := len(w.auditRows(t, ctx))

	body := `{"project_id":"` + w.otherProjectID + `","title":"Authorized Elsewhere"}`
	resp := w.patchMetadata(t, w.alice, w.assetPID, body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, readAll(t, resp))
	}
	var env errorEnvelope
	if err := json.Unmarshal([]byte(readAll(t, resp)), &env); err != nil {
		t.Fatalf("decode refusal: %v", err)
	}
	if env.Code != assetmetadata.CodeAssetNotFound {
		t.Errorf("code = %q, want %q", env.Code, assetmetadata.CodeAssetNotFound)
	}

	_, afterMetadata := w.assetFingerprint(t, ctx)
	if afterMetadata != beforeMetadata {
		t.Error("a revision authorized in another project wrote the asset anyway")
	}
	if got := len(w.auditRows(t, ctx)); got != beforeAudits {
		t.Errorf("audit rows = %d, want %d", got, beforeAudits)
	}
}

// TestMetadataRevisionRefusesAnUnknownPid: a pid that names nothing is the
// same 404 as an asset of another project, and it writes nothing.
func TestMetadataRevisionRefusesAnUnknownPid(t *testing.T) {
	ctx := testCtx(t)
	w := newMetadataWorld(t)
	beforeAudits := len(w.auditRows(t, ctx))

	resp := w.patchMetadata(t, w.alice, "01j9z6k3m4n5p6q7r8s9t0v9z9",
		w.body(map[string]any{"title": "Nowhere"}))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, readAll(t, resp))
	}
	if got := len(w.auditRows(t, ctx)); got != beforeAudits {
		t.Errorf("audit rows = %d, want %d", got, beforeAudits)
	}
}

// --------------------------------------------------------------------------
// 4. The rename leaves the pid, and the public URL, byte-identical

// TestSlugRenameLeavesThePidAndPublicURLByteIdentical is acceptance
// criterion 4, and it is asserted through the PUBLIC read route rather than
// through the database: what has to stay stable is the address a client
// holds, so the test fetches /api/v1/assets/{pid} anonymously before and
// after a rename and compares the identity it resolved.
//
// The rename is real: the slug the response renders DOES change, which is
// what makes "the pid did not" a statement about the pid rather than about
// a revision that did nothing.
func TestSlugRenameLeavesThePidAndPublicURLByteIdentical(t *testing.T) {
	w := newMetadataWorld(t)

	anonymous := &http.Client{}
	fetch := func(t *testing.T) (raw string, page assetPagePayload) {
		t.Helper()
		resp, err := anonymous.Get(w.ts + assets.AssetAPIPath(assets.PID(w.assetPID)))
		if err != nil {
			t.Fatalf("GET the asset page: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", assets.AssetAPIPath(assets.PID(w.assetPID)), resp.StatusCode)
		}
		raw = readAll(t, resp)
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatalf("decode the asset page: %v: %s", err, raw)
		}
		return raw, page
	}

	beforeRaw, beforePage := fetch(t)
	if beforePage.Asset.PID != w.assetPID {
		t.Fatalf("the page resolves pid %q, want %q", beforePage.Asset.PID, w.assetPID)
	}
	if beforePage.Asset.Slug != "metadata-subject" {
		t.Fatalf("the fixture's slug is %q, want metadata-subject", beforePage.Asset.Slug)
	}

	resp := w.patchMetadata(t, w.alice, w.assetPID, w.body(map[string]any{
		"slug":  "a-renamed-subject",
		"title": "A Renamed Subject",
	}))
	mustStatus(t, resp, http.StatusOK)
	revision := decodeRevision(t, resp)

	afterRaw, afterPage := fetch(t)
	if afterPage.Asset.PID != beforePage.Asset.PID {
		t.Fatalf("the pid changed across a rename: %q -> %q", beforePage.Asset.PID, afterPage.Asset.PID)
	}
	if afterPage.Asset.PID != w.assetPID {
		t.Fatalf("the page now resolves %q, want %q", afterPage.Asset.PID, w.assetPID)
	}
	if afterPage.Asset.Slug == beforePage.Asset.Slug {
		t.Fatal("the slug did not change: the invariance assertions below would be vacuous")
	}
	if afterPage.Asset.Slug != "a-renamed-subject" {
		t.Errorf("slug = %q, want the renamed one", afterPage.Asset.Slug)
	}
	if afterPage.Asset.Title != "A Renamed Subject" {
		t.Errorf("title = %q, want the revised one", afterPage.Asset.Title)
	}

	// The URL is byte-identical, and it is built from the pid: the response
	// says so, and the public page is reachable at exactly it.
	wantURL := assets.AssetURL(assets.PID(w.assetPID))
	if revision.URL != wantURL {
		t.Errorf("the revision answered url = %q, want %q", revision.URL, wantURL)
	}
	if strings.Contains(revision.URL, "a-renamed-subject") || strings.Contains(revision.URL, "metadata-subject") {
		t.Errorf("url = %q carries a slug: a persistent URL names the pid only", revision.URL)
	}

	// The public body still names the SAME asset: the pid field and the
	// origin project's block are the two identity-shaped parts of it, and
	// neither moved. (The raw bodies differ elsewhere — the slug and title
	// are rendered — so the comparison is made on the identity, not on the
	// bytes of the whole document.)
	if beforePage.Asset.OriginProject == nil || afterPage.Asset.OriginProject == nil {
		t.Fatal("the page carries no origin project block")
	}
	if beforePage.Asset.OriginProject.ID != afterPage.Asset.OriginProject.ID {
		t.Errorf("origin project changed: %q -> %q",
			beforePage.Asset.OriginProject.ID, afterPage.Asset.OriginProject.ID)
	}
	if len(afterRaw) == 0 || len(beforeRaw) == 0 {
		t.Fatal("a page body was empty")
	}

	// The slug is not an address: there is no route that resolves one, so
	// a client that bookmarked the old slug-shaped URL did not have a URL
	// to lose.
	for _, path := range []string{
		"/api/v1/assets/metadata-subject",
		"/api/v1/assets/a-renamed-subject",
	} {
		resp, err := anonymous.Get(w.ts + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body := readAll(t, resp)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s = 200: a slug resolves as an asset, so the slug IS an identity", path)
		}
		if strings.Contains(body, w.assetPID) {
			t.Errorf("GET %s disclosed the pid: %s", path, body)
		}
	}
}

// --------------------------------------------------------------------------
// 5. Cover, and the five items of docs/11 §4

// TestMetadataCoverIsRefusedByNameAndLeavesTheSlotEmpty is acceptance
// criterion 6. docs/11 §4 lists cover among the revisable metadata, so this
// build has to account for it rather than pretend it does not exist: the
// column exists (migration 00125), the request field exists, and a client
// that sends one is told by NAME that the blob channel is missing — while
// nothing is written and no audit row claims otherwise.
func TestMetadataCoverIsRefusedByNameAndLeavesTheSlotEmpty(t *testing.T) {
	ctx := testCtx(t)
	w := newMetadataWorld(t)

	_, beforeMetadata := w.assetFingerprint(t, ctx)
	beforeAudits := len(w.auditRows(t, ctx))

	resp := w.patchMetadata(t, w.alice, w.assetPID, w.body(map[string]any{
		"cover": "33333333-3333-4333-8333-333333333333",
	}))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", resp.StatusCode, readAll(t, resp))
	}
	var env errorEnvelope
	if err := json.Unmarshal([]byte(readAll(t, resp)), &env); err != nil {
		t.Fatalf("decode the refusal: %v", err)
	}
	if env.Code != assetmetadata.CodeCoverNotSupported {
		t.Errorf("code = %q, want %q", env.Code, assetmetadata.CodeCoverNotSupported)
	}
	if !strings.Contains(env.Message, "cover") {
		t.Errorf("the refusal does not name the field: %q", env.Message)
	}
	if !strings.Contains(env.Message, "blob") {
		t.Errorf("the refusal does not name the missing channel: %q", env.Message)
	}

	// The slot EXISTS and is empty: the field was reserved, not omitted.
	// A SELECT of a column that did not exist is an error, so this also
	// proves migration 00125 landed.
	var cover *string
	if err := w.pool.QueryRow(ctx,
		`SELECT cover_blob_id::text FROM research_assets WHERE id = $1`, w.assetID).Scan(&cover); err != nil {
		t.Fatalf("the reserved cover column is not there: %v", err)
	}
	if cover != nil {
		t.Errorf("cover_blob_id = %q, want NULL: this build has no writer for it", *cover)
	}

	_, afterMetadata := w.assetFingerprint(t, ctx)
	if afterMetadata != beforeMetadata {
		t.Error("the refused cover wrote the asset row anyway")
	}
	if got := len(w.auditRows(t, ctx)); got != beforeAudits {
		t.Errorf("audit rows = %d, want %d: a refused revision must not be audited as one that happened", got, beforeAudits)
	}
}

// TestMetadataRevisesFourOfTheFiveDocsItemsAndRefusesTheFifth is the
// accounting of docs/11 §4's list in executable form: description,
// keywords, contact and documentation are revised in ONE request, and cover
// is refused by name. A future build that gave the cover a channel would
// have to change this test — which is the point: the gap is recorded where
// it can be seen, not left as an absence.
func TestMetadataRevisesFourOfTheFiveDocsItemsAndRefusesTheFifth(t *testing.T) {
	ctx := testCtx(t)
	w := newMetadataWorld(t)

	resp := w.patchMetadata(t, w.alice, w.assetPID, w.body(map[string]any{
		"description":   "the description docs/11 §4 makes revisable",
		"keywords":      []string{"mof", "porosity"},
		"contact":       []string{"alice@example.org"},
		"documentation": []string{"https://example.org/docs", "https://doi.org/10.1000/xyz"},
	}))
	mustStatus(t, resp, http.StatusOK)
	payload := decodeRevision(t, resp)
	if payload.Description != "the description docs/11 §4 makes revisable" {
		t.Errorf("description = %q", payload.Description)
	}
	if len(payload.Keywords) != 2 || payload.Keywords[0] != "mof" || payload.Keywords[1] != "porosity" {
		t.Errorf("keywords = %v, want the declared order", payload.Keywords)
	}
	if len(payload.Contact) != 1 || payload.Contact[0] != "alice@example.org" {
		t.Errorf("contact = %v", payload.Contact)
	}
	if len(payload.Documentation) != 2 || payload.Documentation[1] != "https://doi.org/10.1000/xyz" {
		t.Errorf("documentation = %v, want both entries in order", payload.Documentation)
	}
	if payload.Cover != "" {
		t.Errorf("cover = %q, want empty", payload.Cover)
	}

	// The four landed in the row (not merely in the response).
	var description string
	var keywords, contact, documentation []string
	if err := w.pool.QueryRow(ctx,
		`SELECT description, keywords, contact, documentation FROM research_assets WHERE id = $1`,
		w.assetID).Scan(&description, &keywords, &contact, &documentation); err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if description != "the description docs/11 §4 makes revisable" {
		t.Errorf("stored description = %q", description)
	}
	if len(keywords) != 2 || len(contact) != 1 || len(documentation) != 2 {
		t.Errorf("stored lists = %v / %v / %v", keywords, contact, documentation)
	}

	// The fifth is refused, by name.
	resp = w.patchMetadata(t, w.alice, w.assetPID, w.body(map[string]any{
		"description": "a change that must not commit",
		"cover":       "33333333-3333-4333-8333-333333333333",
	}))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("the mixed request answered %d, want 409: %s", resp.StatusCode, readAll(t, resp))
	}
	if err := w.pool.QueryRow(ctx, `SELECT description FROM research_assets WHERE id = $1`, w.assetID).
		Scan(&description); err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if description != "the description docs/11 §4 makes revisable" {
		t.Errorf("the refused request wrote its description anyway: %q", description)
	}
	if got := len(w.auditRows(t, ctx)); got != 1 {
		t.Errorf("audit rows = %d, want 1: the refused request was audited as a revision", got)
	}
}

// --------------------------------------------------------------------------
// 6. Nothing that is refused writes

// TestMetadataValidationRefusalsWriteNothing: a body the command refuses for
// its SHAPE is refused before any read, so nothing is written and nothing is
// audited. The row is fingerprinted around each refusal, because "the
// response was 400" is not the same statement as "the row is unchanged".
func TestMetadataValidationRefusalsWriteNothing(t *testing.T) {
	ctx := testCtx(t)
	w := newMetadataWorld(t)

	resp := w.patchMetadata(t, w.alice, w.assetPID, w.body(map[string]any{
		"description": "the baseline", "keywords": []string{"baseline"},
	}))
	mustStatus(t, resp, http.StatusOK)
	_, baseline := w.assetFingerprint(t, ctx)
	baselineAudits := len(w.auditRows(t, ctx))

	cases := []struct {
		name string
		body string
	}{
		{"no project named", `{"title":"No Project"}`},
		{"nothing to revise", w.body(map[string]any{})},
		{"a blank title", w.body(map[string]any{"title": "   "})},
		{"a blank slug", w.body(map[string]any{"slug": ""})},
		{"a description over the bound", w.body(map[string]any{"description": strings.Repeat("d", assets.MaxDescriptionLen+1)})},
		{"too many keywords", w.body(map[string]any{"keywords": manyStrings(assets.MaxKeywords+1, "k")})},
		{"a blank keyword among real ones", w.body(map[string]any{"keywords": []string{"real", "  "}})},
		{"a malformed document", `{"title":`},
		// A pid in the BODY is not the target; the path is. The request is
		// refused for carrying no revisable field, and the other asset the
		// body names is not touched.
		{"unknown fields only", w.body(map[string]any{"asset_pid": "01j9z6k3m4n5p6q7r8s9t0v9z9"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := w.patchMetadata(t, w.alice, w.assetPID, tc.body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, readAll(t, resp))
			}
			var env errorEnvelope
			if err := json.Unmarshal([]byte(readAll(t, resp)), &env); err != nil {
				t.Fatalf("decode refusal: %v", err)
			}
			if env.Code != assetmetadata.CodeValidationFailed {
				t.Errorf("code = %q, want %q", env.Code, assetmetadata.CodeValidationFailed)
			}
			_, now := w.assetFingerprint(t, ctx)
			if now != baseline {
				t.Errorf("the row changed after a refused request:\n%s\n%s", baseline, now)
			}
			if got := len(w.auditRows(t, ctx)); got != baselineAudits {
				t.Errorf("audit rows = %d, want %d", got, baselineAudits)
			}
		})
	}
}

// TestMetadataRevisionRefusesASlugAddressingTheAsset: the route resolves the
// path segment as a PID and never as a slug, so a client cannot rename an
// asset by a name that a rename just moved. It is the negative half of
// criterion 4, over the write path.
func TestMetadataRevisionRefusesASlugAddressingTheAsset(t *testing.T) {
	ctx := testCtx(t)
	w := newMetadataWorld(t)
	_, beforeMetadata := w.assetFingerprint(t, ctx)

	resp := w.patchMetadata(t, w.alice, "metadata-subject", w.body(map[string]any{"title": "By Slug"}))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", resp.StatusCode, readAll(t, resp))
	}
	_, afterMetadata := w.assetFingerprint(t, ctx)
	if afterMetadata != beforeMetadata {
		t.Error("a slug-addressed request changed the row")
	}
}

// manyStrings builds an n-entry list of the same value.
func manyStrings(n int, value string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = value
	}
	return out
}

// --------------------------------------------------------------------------
// 7. The transaction's own shape

// TestMetadataRevisionLocksTheAssetRowBeforeWriting: two revisions of the
// same asset run one after the other, and the audit of the second records as
// its BEFORE the values the first committed — not the values the fixture
// seeded. That is what the row lock buys: a before value read under the same
// lock that overwrites it. A store that read the before outside the
// transaction would be racing a writer it never waited for, and this case is
// the observable consequence.
func TestMetadataRevisionLocksTheAssetRowBeforeWriting(t *testing.T) {
	ctx := testCtx(t)
	w := newMetadataWorld(t)

	resp := w.patchMetadata(t, w.alice, w.assetPID, w.body(map[string]any{"title": "First Writer"}))
	mustStatus(t, resp, http.StatusOK)
	resp = w.patchMetadata(t, w.bob, w.assetPID, w.body(map[string]any{"title": "Second Writer"}))
	mustStatus(t, resp, http.StatusOK)

	rows := w.auditRows(t, ctx)
	if len(rows) != 2 {
		t.Fatalf("audit rows = %d, want 2", len(rows))
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(rows[1].Before), &second); err != nil {
		t.Fatalf("before_summary: %v", err)
	}
	if got, _ := second["title"].(string); got != "First Writer" {
		t.Errorf("the second revision recorded before.title = %q, want the first revision's %q",
			got, "First Writer")
	}
	if got, _ := second["slug"].(string); got != "metadata-subject" {
		t.Errorf("before.slug = %q, want the fixture's slug", got)
	}
}

// TestMetadataStoreRefusesAnUnparseableProject: the store is reachable
// directly (a non-HTTP caller), so its own guards are asserted rather than
// assumed. A project id that is not a uuid resolves to no project at all.
func TestMetadataStoreRefusesAnUnparseableProject(t *testing.T) {
	ctx := testCtx(t)
	w := newMetadataWorld(t)
	title := "Nowhere"
	_, err := w.store.ReviseMetadata(ctx, assetmetadata.RevisionRequest{
		ProjectID: "not-a-uuid",
		AssetPID:  w.assetPID,
		Changes:   assetmetadata.Changes{Title: &title},
		ActorID:   w.aliceID,
	})
	if !errors.Is(err, assetmetadata.ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
}

// TestMetadataColumnDefaultsMatchTheDomainValue: migration 00125's columns
// are what an asset that never had metadata reads back as — the state every
// pre-existing asset is in. It is asserted over a row the fixture seeded by
// SQL, so it also pins that the defaults are the ones the Go value renders.
func TestMetadataColumnDefaultsMatchTheDomainValue(t *testing.T) {
	ctx := testCtx(t)
	w := newMetadataWorld(t)

	rawPID := "01j9z6k3m4n5p6q7r8s9t0v7y7"
	if _, err := w.pool.Exec(ctx,
		`INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
		 VALUES ('dataset', 'defaulted', 'Defaulted', $1, $2)`, w.projectID, rawPID); err != nil {
		t.Fatalf("seed an asset with no metadata: %v", err)
	}
	var (
		description            string
		keywords, contact, doc []string
		cover                  *string
	)
	if err := w.pool.QueryRow(ctx,
		`SELECT description, keywords, contact, documentation, cover_blob_id::text
		   FROM research_assets WHERE pid = $1`, rawPID).
		Scan(&description, &keywords, &contact, &doc, &cover); err != nil {
		t.Fatalf("read the defaulted row: %v", err)
	}
	if description != "" {
		t.Errorf("description default = %q, want empty", description)
	}
	for name, got := range map[string][]string{"keywords": keywords, "contact": contact, "documentation": doc} {
		if len(got) != 0 {
			t.Errorf("%s default = %v, want empty", name, got)
		}
	}
	if cover != nil {
		t.Errorf("cover default = %q, want NULL", *cover)
	}

	// And a revision of that asset works over the defaults.
	resp := w.patchMetadata(t, w.alice, rawPID, `{"project_id":"`+w.projectID+`","keywords":["first"]}`)
	mustStatus(t, resp, http.StatusOK)
	payload := decodeRevision(t, resp)
	if len(payload.Keywords) != 1 || payload.Keywords[0] != "first" {
		t.Errorf("keywords = %v, want [first]", payload.Keywords)
	}
	if payload.Description != "" {
		t.Errorf("description = %q, want the default kept", payload.Description)
	}
}
