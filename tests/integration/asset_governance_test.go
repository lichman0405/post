// Task T0711 required test "asset governance tests" — the storage and
// command half of docs/11 §6 ("分离 Rights Holder、Custodian、Maintainer、
// Creator、Contributor、Originating Project。Ownership transfer 是 append-only
// governance event；Creator/history 永久保留"), against a REAL PostgreSQL
// (docs/66 §3: task/run-scoped namespaced database, no mocks) and the REAL
// permission matrix.
//
// What this file is the acceptance for, in the task's own order:
//
//  1. the creator list a publish DECLARES is stored, and reads back
//     item-by-item in the order it was declared — the gap
//     internal/assets/page.go named ("the gate validates the creator ids a
//     publish declares but no table stores them").
//  2. the six roles of docs/11 §6 live in THREE separate shapes and are not
//     merged into one "owner": the version-scoped credits (creator,
//     contributor, custodian, maintainer), the asset-scoped holder chain
//     (rights holder), and the originating project on the asset row itself.
//  3. a transfer APPENDS: after a second transfer the earlier holding is
//     still readable, and the table refuses an UPDATE or a DELETE.
//  4. the creator list survives a transfer verbatim — a transfer changes
//     who holds, never who wrote.
//  5. a transfer changes none of the asset's identity: id, version(s),
//     origin project and creators are four separate assertions, not one.
//  6. a holder is a user OR an organization, recorded as a kind AND an id,
//     and no read path hands back an id without the kind.
//  7. the permission is owner-only and fails closed: owner allowed,
//     maintainer denied, non-member denied, agent denied — with nothing
//     written by any of the denials.
//  8. the transfer is audited append-only, and the audit of the first
//     transfer is still readable after the second.
//
// The publish is driven through the production store
// (persistence.AssetPublishStore.Publish — the same value cmd/api wires),
// the governance change through the production command
// (assetrights.Command over persistence.AssetRightsStore and the real
// authz matrix), and the reads through the production reader
// (assetrights.HolderReader / persistence.AssetPageStore). Nothing here
// re-implements a rule the product owns.

package integration

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/application/assetrights"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

const assetGovernanceTaskID = "T0711"

// governanceWorld is the composed surface: one migrated database, the
// production publish store, the production governance command and reader,
// and the fixture's ids.
type governanceWorld struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	store    *persistence.AssetRightsStore
	cmd      *assetrights.Command
	publish  *assetpublish.Command
	projects *persistence.ProjectStore

	aliceID string // owner of the project, publisher, first holder
	bobID   string // project maintainer
	carolID string // authenticated, NOT a member — and a declared creator
	acmeID  string // organization (a holder that is not a person)
	otherID string // second organization, never a holder

	projectID string
	// foreignProject is a second project the same users own: a governance
	// request authorized in it must not be able to write the first
	// project's asset.
	foreignProjectID string

	assetID   string
	assetPID  string
	versionID string

	// The publishable fixture's pieces, kept so a second publication can be
	// built from the same real rows.
	releaseID       string
	objectVersionID string
	openBlobID      string
}

// newGovernanceWorld migrates a namespaced database, seeds the fixture and
// PUBLISHES version 1.0 of the asset through the production publish store,
// declaring two creators (alice, carol) in that order.
func newGovernanceWorld(t *testing.T) *governanceWorld {
	t.Helper()
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), assetGovernanceTaskID)

	w := &governanceWorld{ctx: ctx, pool: pool, projects: persistence.NewProjectStore(pool)}

	w.aliceID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('gov-alice', 'Alice') RETURNING id`)
	w.bobID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('gov-bob', 'Bob') RETURNING id`)
	w.carolID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('gov-carol', 'Carol') RETURNING id`)
	w.acmeID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO organizations (slug, name) VALUES ('acme-lab', 'Acme Lab') RETURNING id`)
	w.otherID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO organizations (slug, name) VALUES ('beta-lab', 'Beta Lab') RETURNING id`)

	w.projectID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('governance-lab', 'Governance Lab', 'T0711 fixture', 'public', $1) RETURNING id`, w.aliceID)
	w.foreignProjectID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('governance-other', 'Governance Other', 'T0711 fixture', 'public', $1) RETURNING id`, w.aliceID)
	for _, m := range []struct {
		project, user, role string
	}{
		{w.projectID, w.aliceID, "owner"},
		{w.projectID, w.bobID, "maintainer"},
		{w.foreignProjectID, w.aliceID, "owner"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
			m.project, m.user, m.role); err != nil {
			t.Fatalf("seed membership %s: %v", m.role, err)
		}
	}

	// The rows a dataset publish needs to resolve: a release to pin as an
	// origin ref, an openly attached blob the manifest names, and the
	// project state both hang from.
	stateID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO project_states (project_id, state_hash, manifest_version)
		 VALUES ($1, 'genesis-governance', '1') RETURNING id`, w.projectID)
	releaseID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO releases (project_id, version, title, state_id, manifest, manifest_hash, created_by)
		 VALUES ($1, 'v1.0.0', 'Governance Baseline', $2, '{}'::jsonb, 'x', $3) RETURNING id`,
		w.projectID, stateID, w.aliceID)
	objectID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, 'dataset_record', $2) RETURNING id`, w.projectID, w.aliceID)
	objectVersionID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title, lifecycle_state, payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, 'https://open-rd.example/schemas/dataset.schema.json', '1',
		         'Governance Object Version', 'active', '{}'::jsonb, 'x', $3) RETURNING id`,
		objectID, stateID, w.aliceID)
	blobID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO blobs (content_hash, size_bytes, storage_key, created_by)
		 VALUES ('sha256:governance-open', 2048, 'blobs/governance-open', $1) RETURNING id`, w.aliceID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO blob_attachments (blob_id, scientific_object_version_id, attachment_role, access_level, state_id)
		 VALUES ($1, $2, 'data', 'open', $3)`, blobID, objectVersionID, stateID); err != nil {
		t.Fatalf("attach the open blob: %v", err)
	}
	w.releaseID, w.objectVersionID, w.openBlobID = releaseID, objectVersionID, blobID

	// The publish itself: the REAL command over the real store, composed as
	// cmd/api composes it (T0705's surface, T0705 left unmodified). The
	// request names no pid, so the pid the asset carries is the one the
	// command minted — which is what makes the credits below a record of
	// THIS publication rather than of a fixture row.
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	w.publish = assetpublish.NewCommand(assetpublish.Deps{
		Members: w.projects,
		Policies: policyhttp.New(policyhttp.Deps{
			Store: persistence.NewPolicyStore(pool), Orgs: persistence.NewOrgStore(pool), Projects: w.projects,
		}).Service(),
		Rules: policy.NewRuleEvaluator(),
		Store: persistence.NewAssetPublishStore(pool, rsgvalidation.NewValidator(reg)),
		Authz: authz.NewMatrixEngine(),
	})
	cand := buildCandidate(t, candidateOptions{
		version: "1.0", visibility: "public",
		refs:     []string{"release:" + releaseID, "object_version:" + objectVersionID},
		creators: []string{w.aliceID, w.carolID},
		blobIDs:  []string{blobID}, accessLevel: "open", dataAccess: rights.DataAccessOpen,
		title: "Governance Subject", slug: "governance-subject",
	})
	published, err := w.publish.Publish(ctx, assetpublish.Actor{
		User: domain.User{ID: w.aliceID, Handle: "gov-alice"},
	}, cand.params(w.projectID, nil))
	if err != nil {
		t.Fatalf("publish the governance fixture: %v", err)
	}
	w.assetID, w.assetPID, w.versionID = published.AssetID, published.AssetPID, published.ID

	w.store = persistence.NewAssetRightsStore(pool)
	w.cmd = assetrights.NewCommand(assetrights.Deps{
		Members: persistence.NewProjectStore(pool),
		Store:   w.store,
		Authz:   authz.NewMatrixEngine(),
	})
	return w
}

// transfer drives the production command for one holder change.
func (w *governanceWorld) transfer(t *testing.T, actorID string, isAgent bool, projectID string, holder domain.Party) (assetrights.HolderChange, error) {
	t.Helper()
	return w.cmd.ChangeRightsHolder(w.ctx, assetrights.Actor{
		User: domain.User{ID: actorID}, IsAgent: isAgent,
	}, assetrights.ChangeParams{ProjectID: projectID, AssetPID: w.assetPID, Holder: holder})
}

// history reads the whole chain through the production reader.
func (w *governanceWorld) history(t *testing.T) []assetrights.HolderEvent {
	t.Helper()
	events, err := w.store.HolderHistory(w.ctx, w.assetPID)
	if err != nil {
		t.Fatalf("read holder history: %v", err)
	}
	return events
}

// eventCount counts the chain's rows directly, so an assertion about what a
// REFUSED change did or did not write does not depend on the reader under
// test.
func (w *governanceWorld) eventCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(w.ctx,
		`SELECT count(*) FROM asset_rights_holder_events WHERE asset_id = $1`, w.assetID).Scan(&n); err != nil {
		t.Fatalf("count holder events: %v", err)
	}
	return n
}

// --------------------------------------------------------------------------
// 1. the declared creator list is stored, and reads back item-by-item

func TestAssetGovernancePublishStoresDeclaredCreators(t *testing.T) {
	w := newGovernanceWorld(t)

	// The stored rows, in the order the publish declared them. The position
	// column is what makes "item by item" a stored fact rather than a
	// coincidence of insertion order.
	rows, err := w.pool.Query(w.ctx,
		`SELECT role, party_kind, party_id::text, position, recorded_by::text
		 FROM asset_version_parties WHERE asset_version_id = $1 ORDER BY position`, w.versionID)
	if err != nil {
		t.Fatalf("read credit rows: %v", err)
	}
	defer rows.Close()
	type credit struct {
		role, kind, partyID, recordedBy string
		position                        int
	}
	var got []credit
	for rows.Next() {
		var c credit
		if err := rows.Scan(&c.role, &c.kind, &c.partyID, &c.position, &c.recordedBy); err != nil {
			t.Fatalf("scan credit row: %v", err)
		}
		got = append(got, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read credit rows: %v", err)
	}
	want := []credit{
		{"creator", "user", w.aliceID, w.aliceID, 0},
		{"creator", "user", w.carolID, w.aliceID, 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored credits = %+v, want the two declared creators in declaration order: %+v", got, want)
	}

	// ... and the same list reads back through the production READER the
	// page uses (persistence.AssetPageStore), item by item, from the pid.
	pageStore := persistence.NewAssetPageStore(w.pool)
	state, ok, err := pageStore.LoadAssetPage(w.ctx, assets.PID(w.assetPID))
	if err != nil || !ok {
		t.Fatalf("load asset page state: ok=%v err=%v", ok, err)
	}
	parties := state.Parties[w.versionID]
	if len(parties) != 2 {
		t.Fatalf("page reader returned %d credited parties, want the 2 declared: %+v", len(parties), parties)
	}
	for i, wantID := range []string{w.aliceID, w.carolID} {
		if parties[i].PartyID != wantID || parties[i].Kind != "user" || parties[i].Role != "creator" {
			t.Errorf("page reader party[%d] = %+v, want the declared creator %s (kind user, role creator)", i, parties[i], wantID)
		}
	}
	// The identity resolves through the users map, which is what the page
	// renders as the name beside the credit.
	if _, ok := state.Users[w.carolID]; !ok {
		t.Errorf("the page reader did not resolve the declared creator %s: %+v", w.carolID, state.Users)
	}
}

// TestAssetGovernancePaddedCreatorIDsStoreTheCanonicalUUID: a publication
// whose creator ids carry the whitespace a JSON body may bring is
// ACCEPTED, and the credit rows it writes are the canonical uuids —
// byte-identical to the rows of the same publication written without the
// padding.
//
// Both halves have to agree about what a creator id is for this to hold:
// the gate admits the padded spelling (validCreatorIDs trims and
// case-folds before it validates) and the store normalizes with the same
// function (internal/persistence.insertVersionCredits calls
// assets.CanonicalCreatorID), so what lands in the uuid column is the id
// itself. Without the store's normalization the preview blesses the
// candidate and the publish then fails as a store error — a 503 on a
// request that succeeded before 00082 existed (T0711 review, round 8).
func TestAssetGovernancePaddedCreatorIDsStoreTheCanonicalUUID(t *testing.T) {
	w := newGovernanceWorld(t)

	// One version's credit rows, as text, the way creditRows renders them.
	rowsOf := func(versionID string) []string {
		t.Helper()
		rows, err := w.pool.Query(w.ctx,
			`SELECT role, party_kind, party_id::text, position
			 FROM asset_version_parties WHERE asset_version_id = $1 ORDER BY role, position`, versionID)
		if err != nil {
			t.Fatalf("read the credit rows of %s: %v", versionID, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var role, kind, partyID string
			var position int
			if err := rows.Scan(&role, &kind, &partyID, &position); err != nil {
				t.Fatalf("scan credit row: %v", err)
			}
			out = append(out, role+"|"+kind+"|"+partyID+"|"+strconv.Itoa(position))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the credit rows of %s: %v", versionID, err)
		}
		return out
	}

	// publishVersion declares one creator and returns the version it wrote.
	publishVersion := func(version string, creatorIDs []string) string {
		t.Helper()
		cand := buildCandidate(t, candidateOptions{
			pid: w.assetPID, version: version, visibility: "public",
			refs:     []string{"release:" + w.releaseID, "object_version:" + w.objectVersionID},
			creators: creatorIDs,
			blobIDs:  []string{w.openBlobID}, accessLevel: "open", dataAccess: rights.DataAccessOpen,
		})
		published, err := w.publish.Publish(w.ctx, assetpublish.Actor{
			User: domain.User{ID: w.aliceID, Handle: "gov-alice"},
		}, cand.params(w.projectID, nil))
		if err != nil {
			t.Fatalf("publish %s with creator_ids %q: %v", version, creatorIDs, err)
		}
		return published.ID
	}

	// The control first, then the same declaration in the spelling a caller
	// may send: padded, and in another case.
	clean := publishVersion("1.1", []string{w.aliceID})
	padded := publishVersion("1.2", []string{"  " + strings.ToUpper(w.aliceID) + "  "})

	want := []string{"creator|user|" + w.aliceID + "|0"}
	if got := rowsOf(padded); !reflect.DeepEqual(got, want) {
		t.Errorf("the padded publication stored %v, want the canonical uuid %v", got, want)
	}
	if got, control := rowsOf(padded), rowsOf(clean); !reflect.DeepEqual(got, control) {
		t.Errorf("the padded publication stored %v and the plain one %v — one declaration in two spellings must store one row, identically", got, control)
	}

	// ... and the same value through the production reader the page uses.
	state, ok, err := persistence.NewAssetPageStore(w.pool).LoadAssetPage(w.ctx, assets.PID(w.assetPID))
	if err != nil || !ok {
		t.Fatalf("load asset page state: ok=%v err=%v", ok, err)
	}
	parties := state.Parties[padded]
	if len(parties) != 1 || parties[0].PartyID != w.aliceID || parties[0].Kind != "user" || parties[0].Role != "creator" {
		t.Errorf("the page reader returned %+v for the padded publication, want one creator credit on %s", parties, w.aliceID)
	}
}

// TestAssetGovernanceCreditsAreWrittenOnlyByThePublishTransaction: a
// publish that the gate REFUSES writes no credit, so "the version exists"
// and "its declared creators are stored" are never separately true.
func TestAssetGovernanceCreditsAreWrittenOnlyByThePublishTransaction(t *testing.T) {
	w := newGovernanceWorld(t)
	before := w.rows(t, `SELECT count(*) FROM asset_version_parties`)

	// A second publication of the same asset, valid in every respect but
	// one: the caller declares no creator. The candidate is built valid and
	// then emptied, so what is under test is the refusal and not the
	// fixture — the gate must refuse it (assets.Gate's validCreatorIDs, the
	// half of the rule the publish already had), and the transaction must
	// therefore never reach the credit insert.
	cand := buildCandidate(t, candidateOptions{
		pid: w.assetPID, version: "9.9", visibility: "public",
		refs:     []string{"release:" + w.releaseID, "object_version:" + w.objectVersionID},
		creators: []string{w.aliceID},
		blobIDs:  []string{w.openBlobID}, accessLevel: "open", dataAccess: rights.DataAccessOpen,
	})
	if _, err := assets.Gate(cand.cand); err != nil {
		t.Fatalf("the fixture candidate is not publishable, so the refusal below would prove nothing: %v", err)
	}
	cand.cand.CreatorIDs = nil
	cand.cand.Version = "9.9"
	_, err := w.publish.Publish(w.ctx, assetpublish.Actor{
		User: domain.User{ID: w.aliceID},
	}, cand.params(w.projectID, nil))
	if err == nil {
		t.Fatal("the publish of a version with no creator was accepted: the gate's creator rule did not fire")
	}
	// The refusal is the publish's own, and it names the missing creator
	// among the blockers it decided on — so what stopped this publication
	// was the creator rule and not some other objection.
	var refused *assetpublish.PublishRefused
	if !errors.As(err, &refused) {
		t.Fatalf("the refusal = %v, want the publish's own refusal carrying the preview it blocked on", err)
	}
	named := false
	for _, blocker := range refused.Preview.PublishBlockers {
		if blocker.Code == assets.CodeNoContributors {
			named = true
		}
	}
	if !named {
		t.Errorf("the refusal does not name %s: %+v", assets.CodeNoContributors, refused.Preview.PublishBlockers)
	}
	if after := w.rows(t, `SELECT count(*) FROM asset_version_parties`); after != before {
		t.Errorf("a refused publish changed the credit rows: %d before, %d after", before, after)
	}
}

// rows counts rows in this fixture's database (the package's own countRows,
// over the world's pool and context).
func (w *governanceWorld) rows(t *testing.T, sql string, args ...any) int {
	t.Helper()
	return countRows(t, w.ctx, w.pool, sql, args...)
}

// --------------------------------------------------------------------------
// 2. the six roles of docs/11 §6 are stored separately

func TestAssetGovernanceRolesAreSeparateNotOneOwner(t *testing.T) {
	w := newGovernanceWorld(t)

	// A CONTRIBUTOR on the same version, written where a future writer
	// would write it. The table stores the role, so the same party can be
	// both — and the two rows are distinct rows with distinct roles, which
	// is what "分离 Creator、Contributor" means in storage.
	contributorID := mustQueryUUID(t, w.ctx, w.pool,
		`INSERT INTO asset_version_parties (asset_version_id, role, party_kind, party_id, position, recorded_by)
		 VALUES ($1, 'contributor', 'user', $2, 0, $3) RETURNING id`, w.versionID, w.bobID, w.aliceID)

	var creatorRows, contributorRows int
	if err := w.pool.QueryRow(w.ctx,
		`SELECT count(*) FILTER (WHERE role = 'creator'), count(*) FILTER (WHERE role = 'contributor')
		 FROM asset_version_parties WHERE asset_version_id = $1`, w.versionID).
		Scan(&creatorRows, &contributorRows); err != nil {
		t.Fatalf("count credits by role: %v", err)
	}
	if creatorRows != 2 || contributorRows != 1 {
		t.Fatalf("stored roles: creator=%d contributor=%d, want 2 and 1 — the roles must not be merged into one", creatorRows, contributorRows)
	}
	if contributorID == "" {
		t.Fatal("the contributor row has no id")
	}

	// The RIGHTS HOLDER is not one of those rows: it is a separate,
	// asset-scoped chain. The separation is structural, not a convention:
	// the chain table has no version column at all, so a holder can never
	// be read as a version's credit.
	var versionColumns int
	if err := w.pool.QueryRow(w.ctx,
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_name = 'asset_rights_holder_events' AND column_name LIKE '%version%'`).Scan(&versionColumns); err != nil {
		t.Fatalf("inspect the holder table: %v", err)
	}
	if versionColumns != 0 {
		t.Errorf("asset_rights_holder_events has %d version-shaped columns, want 0: the holder is asset-scoped (docs/11 §6)", versionColumns)
	}
	if _, err := w.transfer(t, w.aliceID, false, w.projectID, domain.Party{Kind: domain.PartyOrganization, ID: w.acmeID}); err != nil {
		t.Fatalf("designate the organization as holder: %v", err)
	}
	holder, err := w.store.CurrentHolder(w.ctx, w.assetPID)
	if err != nil || holder == nil {
		t.Fatalf("read current holder: %+v, %v", holder, err)
	}
	if holder.Holder.Party.Kind != domain.PartyOrganization {
		t.Fatalf("the holder is %q, want the organization — a rights holder is not a version credit", holder.Holder.Party.Kind)
	}
	// The rights holder appears in no credit row, and the creators appear
	// in no holder row: the two tables do not overlap.
	var holderInCredits int
	if err := w.pool.QueryRow(w.ctx,
		`SELECT count(*) FROM asset_version_parties WHERE party_id = $1`, w.acmeID).Scan(&holderInCredits); err != nil {
		t.Fatalf("count the holder among the credits: %v", err)
	}
	if holderInCredits != 0 {
		t.Errorf("the organization is credited %d times as a version party, want 0", holderInCredits)
	}
	var creatorsInHolderEvents int
	if err := w.pool.QueryRow(w.ctx,
		`SELECT count(*) FROM asset_rights_holder_events WHERE holder_id = ANY($1::uuid[])`,
		[]string{w.aliceID, w.carolID}).Scan(&creatorsInHolderEvents); err != nil {
		t.Fatalf("count the creators among the holder events: %v", err)
	}
	if creatorsInHolderEvents != 0 {
		t.Errorf("a declared creator holds the asset %d times, want 0 — Creator is not Rights Holder", creatorsInHolderEvents)
	}

	// The ORIGINATING PROJECT is neither: it is the asset row's own
	// origin_project_id, it is of kind project (which neither governance
	// table admits), and the command refuses to record it as a holder.
	var origin, projectKindParties int
	if err := w.pool.QueryRow(w.ctx,
		`SELECT count(*) FROM research_assets WHERE id = $1 AND origin_project_id = $2`,
		w.assetID, w.projectID).Scan(&origin); err != nil {
		t.Fatalf("read the originating project: %v", err)
	}
	if origin != 1 {
		t.Fatalf("the asset's origin_project_id is not %s: the originating project is a role of the asset row itself", w.projectID)
	}
	if err := w.pool.QueryRow(w.ctx,
		`SELECT (SELECT count(*) FROM asset_version_parties WHERE party_id = $1)
		      + (SELECT count(*) FROM asset_rights_holder_events WHERE holder_id = $1)`,
		w.projectID).Scan(&projectKindParties); err != nil {
		t.Fatalf("count the project among the parties: %v", err)
	}
	if projectKindParties != 0 {
		t.Errorf("the originating project appears %d times as a party, want 0", projectKindParties)
	}
	if _, err := w.transfer(t, w.aliceID, false, w.projectID, domain.Party{Kind: domain.PartyProject, ID: w.projectID}); !errors.Is(err, assetrights.ErrValidation) {
		t.Errorf("designating the originating project as rights holder = %v, want ErrValidation: a project is its own role, not a holder", err)
	}
}

// --------------------------------------------------------------------------
// 3 + 4 + 8. transfer appends; the earlier holding stays readable; the
// creator list and the audit survive

func TestAssetGovernanceTransferAppendsAndKeepsHistory(t *testing.T) {
	w := newGovernanceWorld(t)

	if got := w.history(t); len(got) != 0 {
		t.Fatalf("a freshly published asset has %d holder events, want none: the publish does not invent an owner", len(got))
	}
	current, err := w.store.CurrentHolder(w.ctx, w.assetPID)
	if err != nil {
		t.Fatalf("read current holder: %v", err)
	}
	if current != nil {
		t.Fatalf("current holder = %+v, want nil: an asset nobody has held is answered as unheld, not with a default", current)
	}

	// First designation: a person. There is no previous holder, and the
	// chain says so with an absence rather than a guess.
	first, err := w.transfer(t, w.aliceID, false, w.projectID, domain.Party{Kind: domain.PartyUser, ID: w.aliceID})
	if err != nil {
		t.Fatalf("designate alice as holder: %v", err)
	}
	if first.Ordinal != 1 || first.Previous != nil {
		t.Errorf("first event = ordinal %d with previous %+v, want ordinal 1 and no previous holder", first.Ordinal, first.Previous)
	}

	// Second: the organization. Its PREVIOUS half is the person who held it
	// before — recorded, not overwritten.
	second, err := w.transfer(t, w.aliceID, false, w.projectID, domain.Party{Kind: domain.PartyOrganization, ID: w.acmeID})
	if err != nil {
		t.Fatalf("transfer to the organization: %v", err)
	}
	if second.Ordinal != 2 {
		t.Fatalf("second event ordinal = %d, want 2", second.Ordinal)
	}
	if second.Previous == nil || second.Previous.Party.Kind != domain.PartyUser || second.Previous.Party.ID != w.aliceID {
		t.Fatalf("second event previous = %+v, want the person who held it before (kind user, id %s)", second.Previous, w.aliceID)
	}

	// Third: back to a person. After this the FIRST holding (alice, user)
	// must still be readable — that is the acceptance sentence "转移之后，
	// 转移之前的持有关系仍要读得出来", and it is checked through the chain
	// rather than through a column that was overwritten.
	if _, err := w.transfer(t, w.aliceID, false, w.projectID, domain.Party{Kind: domain.PartyUser, ID: w.bobID}); err != nil {
		t.Fatalf("transfer to bob: %v", err)
	}
	history := w.history(t)
	if len(history) != 3 {
		t.Fatalf("history has %d events, want 3: %+v", len(history), history)
	}
	for i, want := range []struct {
		kind domain.PartyKind
		id   string
	}{
		{domain.PartyUser, w.aliceID},
		{domain.PartyOrganization, w.acmeID},
		{domain.PartyUser, w.bobID},
	} {
		if history[i].Ordinal != i+1 || history[i].Holder.Party.Kind != want.kind || history[i].Holder.Party.ID != want.id {
			t.Errorf("history[%d] = %+v, want ordinal %d holding %s %s", i, history[i], i+1, want.kind, want.id)
		}
	}
	// The superseded holdings are readable with their identities, not just
	// as ids: the first holder's row resolves to the user's handle.
	if got := history[0].Holder.Handle; got != "gov-alice" {
		t.Errorf("the first holder's identity = %q, want the user row's handle gov-alice", got)
	}
	if got := history[1].Holder.DisplayName; got != "Acme Lab" {
		t.Errorf("the organization holder's identity = %q, want the organization row's name", got)
	}
	if got := history[2].Previous; got == nil || got.Party.ID != w.acmeID {
		t.Errorf("the last event's previous = %+v, want the organization it superseded", got)
	}

	// The creator list is untouched by all three transfers, verbatim —
	// including the recorded_by and recorded_at of each credit row.
	credits := w.creditRows(t)
	wantCredits := []string{
		"creator|user|" + w.aliceID + "|0|" + w.aliceID,
		"creator|user|" + w.carolID + "|1|" + w.aliceID,
	}
	if len(credits) != len(wantCredits) {
		t.Fatalf("credits after three transfers = %+v, want the two declared at publish", credits)
	}
	for i, want := range wantCredits {
		if credits[i] != want {
			t.Errorf("credits[%d] = %q, want %q — a transfer does not change who created the version", i, credits[i], want)
		}
	}

	// The audit: one append-only row per transfer, written with the change
	// (docs/26 §5), with the before/after of the state it moved.
	auditRows, err := w.pool.Query(w.ctx,
		`SELECT actor_id::text, action, target_ref, before_summary, after_summary, correlation_id
		 FROM audit_log WHERE action = $1 ORDER BY occurred_at, id`, domain.ActionAssetRightsHolderChanged)
	if err != nil {
		t.Fatalf("read the audit rows: %v", err)
	}
	defer auditRows.Close()
	type auditRow struct {
		actor, action, target, correlation string
		before, after                      string
	}
	var audits []auditRow
	for auditRows.Next() {
		var row auditRow
		var before, after []byte
		if err := auditRows.Scan(&row.actor, &row.action, &row.target, &before, &after, &row.correlation); err != nil {
			t.Fatalf("scan the audit row: %v", err)
		}
		row.before, row.after = string(before), string(after)
		audits = append(audits, row)
	}
	if err := auditRows.Err(); err != nil {
		t.Fatalf("read the audit rows: %v", err)
	}
	if len(audits) != 3 {
		t.Fatalf("audit rows = %d, want one per transfer (3): %+v", len(audits), audits)
	}
	// Each row says what the change moved: the party that held before (an
	// ABSENT summary for the first designation, which had no predecessor,
	// and never a fabricated one) and the party that holds after.
	moved := []struct{ before, after string }{
		{"", w.aliceID},
		{w.aliceID, w.acmeID},
		{w.acmeID, w.bobID},
	}
	for i, row := range audits {
		if row.actor != w.aliceID || row.action != domain.ActionAssetRightsHolderChanged ||
			row.target != "asset:"+w.assetPID || row.correlation == "" {
			t.Errorf("audit row %d = %+v, want the acting user, the action, the asset pid and a correlation id", i, row)
		}
		if moved[i].before == "" {
			if row.before != "" {
				t.Errorf("audit row %d's before summary = %s, want none: an asset that had never been held has no previous holder", i, row.before)
			}
		} else if !strings.Contains(row.before, `"holder_id": "`+moved[i].before+`"`) {
			t.Errorf("audit row %d's before summary = %s, want the holder it superseded (%s)", i, row.before, moved[i].before)
		}
		if !strings.Contains(row.after, `"holder_id": "`+moved[i].after+`"`) {
			t.Errorf("audit row %d's after summary = %s, want the holder it wrote (%s)", i, row.after, moved[i].after)
		}
	}
	// The first transfer's audit is still readable after the third — the
	// audit is a log, not a current-state column.
	if !strings.Contains(audits[0].after, w.aliceID) || !strings.Contains(audits[1].after, w.acmeID) {
		t.Errorf("the earlier audits do not name their own holders: %+v / %+v", audits[0], audits[1])
	}

	// Both tables refuse an UPDATE and a DELETE: append-only is a database
	// guarantee (00014/00015's triggers, 00082's own), not a convention the
	// application keeps.
	for _, sql := range []string{
		`UPDATE asset_rights_holder_events SET holder_kind = holder_kind WHERE asset_id = $1`,
		`DELETE FROM asset_rights_holder_events WHERE asset_id = $1`,
	} {
		if _, err := w.pool.Exec(w.ctx, sql, w.assetID); err == nil {
			t.Errorf("%s was accepted: the holder chain must be append-only", sql)
		}
	}
	if _, err := w.pool.Exec(w.ctx,
		`UPDATE audit_log SET action = 'rewritten' WHERE action = $1`, domain.ActionAssetRightsHolderChanged); err == nil {
		t.Error("an UPDATE of the rights-holder audit row was accepted: the audit must be append-only")
	}
}

// creditRows renders every credit row of the fixture's version as one
// comparable string, positioning included.
func (w *governanceWorld) creditRows(t *testing.T) []string {
	t.Helper()
	rows, err := w.pool.Query(w.ctx,
		`SELECT role, party_kind, party_id::text, position, recorded_by::text
		 FROM asset_version_parties WHERE asset_version_id = $1 ORDER BY role, position`, w.versionID)
	if err != nil {
		t.Fatalf("read credit rows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var role, kind, partyID, recordedBy string
		var position int
		if err := rows.Scan(&role, &kind, &partyID, &position, &recordedBy); err != nil {
			t.Fatalf("scan credit row: %v", err)
		}
		out = append(out, role+"|"+kind+"|"+partyID+"|"+strconv.Itoa(position)+"|"+recordedBy)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read credit rows: %v", err)
	}
	return out
}

// --------------------------------------------------------------------------
// 5. a transfer moves no identity: four independent assertions

func TestAssetGovernanceTransferChangesNoIdentity(t *testing.T) {
	w := newGovernanceWorld(t)

	snapshot := func() (string, string, string, []string, []string) {
		t.Helper()
		var id, pid, origin string
		if err := w.pool.QueryRow(w.ctx,
			`SELECT id::text, pid, origin_project_id::text FROM research_assets WHERE pid = $1`, w.assetPID).
			Scan(&id, &pid, &origin); err != nil {
			t.Fatalf("read the asset row: %v", err)
		}
		rows, err := w.pool.Query(w.ctx,
			`SELECT version, visibility, integrity_hash, published_by::text, origin_refs::text
			 FROM research_asset_versions WHERE asset_id = $1 ORDER BY version`, w.assetID)
		if err != nil {
			t.Fatalf("read the version rows: %v", err)
		}
		defer rows.Close()
		var versions []string
		for rows.Next() {
			var version, visibility, hash, publishedBy, refs string
			if err := rows.Scan(&version, &visibility, &hash, &publishedBy, &refs); err != nil {
				t.Fatalf("scan the version row: %v", err)
			}
			versions = append(versions, strings.Join([]string{version, visibility, hash, publishedBy, refs}, "|"))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("read the version rows: %v", err)
		}
		return id, pid, origin, versions, w.creditRows(t)
	}

	beforeID, beforePID, beforeOrigin, beforeVersions, beforeCredits := snapshot()
	if len(beforeVersions) == 0 {
		t.Fatal("the fixture has no version row: the assertion would measure nothing")
	}

	if _, err := w.transfer(t, w.aliceID, false, w.projectID, domain.Party{Kind: domain.PartyOrganization, ID: w.acmeID}); err != nil {
		t.Fatalf("transfer the asset: %v", err)
	}

	afterID, afterPID, afterOrigin, afterVersions, afterCredits := snapshot()

	// Four independent assertions, on purpose: one test covering all four
	// would pass while any three regressed.
	if afterID != beforeID {
		t.Errorf("asset id changed across the transfer: %q -> %q", beforeID, afterID)
	}
	if afterPID != beforePID {
		t.Errorf("asset pid changed across the transfer: %q -> %q", beforePID, afterPID)
	}
	if afterOrigin != beforeOrigin {
		t.Errorf("origin project changed across the transfer: %q -> %q", beforeOrigin, afterOrigin)
	}
	if !reflect.DeepEqual(afterVersions, beforeVersions) {
		t.Errorf("versions changed across the transfer:\n before %+v\n after  %+v", beforeVersions, afterVersions)
	}
	if !reflect.DeepEqual(afterCredits, beforeCredits) {
		t.Errorf("declared creators changed across the transfer:\n before %+v\n after  %+v", beforeCredits, afterCredits)
	}

	// And the row count: the transfer appends, so no asset row was created
	// and none was removed.
	var assetRows int
	if err := w.pool.QueryRow(w.ctx,
		`SELECT count(*) FROM research_assets WHERE pid = $1`, w.assetPID).Scan(&assetRows); err != nil {
		t.Fatalf("count the asset rows: %v", err)
	}
	if assetRows != 1 {
		t.Errorf("the pid now names %d asset rows, want 1", assetRows)
	}
}

// --------------------------------------------------------------------------
// 6. a holder is a user or an organization, kind + id, on every read path

func TestAssetGovernanceHolderIsAPersonOrAnOrganization(t *testing.T) {
	w := newGovernanceWorld(t)

	// A USER holder.
	userChange, err := w.transfer(t, w.aliceID, false, w.projectID, domain.Party{Kind: domain.PartyUser, ID: w.bobID})
	if err != nil {
		t.Fatalf("transfer to a user: %v", err)
	}
	if userChange.Holder.Party.Kind != domain.PartyUser || userChange.Holder.Party.ID != w.bobID {
		t.Fatalf("user holder = %+v, want kind user and the user's id", userChange.Holder.Party)
	}
	if userChange.Holder.Handle != "gov-bob" || userChange.Holder.DisplayName != "Bob" {
		t.Errorf("user holder identity = %+v, want the user row's handle and display name", userChange.Holder)
	}

	// An ORGANIZATION holder — the L3 ruling: 权利人可以是组织；人和组织分开记.
	orgChange, err := w.transfer(t, w.aliceID, false, w.projectID, domain.Party{Kind: domain.PartyOrganization, ID: w.acmeID})
	if err != nil {
		t.Fatalf("transfer to an organization: %v", err)
	}
	if orgChange.Holder.Party.Kind != domain.PartyOrganization || orgChange.Holder.Party.ID != w.acmeID {
		t.Fatalf("organization holder = %+v, want kind organization and the organization's id", orgChange.Holder.Party)
	}
	if orgChange.Holder.Handle != "acme-lab" || orgChange.Holder.DisplayName != "Acme Lab" {
		t.Errorf("organization holder identity = %+v, want the organization row's slug and name", orgChange.Holder)
	}
	// The person it replaced is still there, and still a person.
	if orgChange.Previous == nil || orgChange.Previous.Party.Kind != domain.PartyUser {
		t.Fatalf("the organization's previous holder = %+v, want the person (the kind is part of the record)", orgChange.Previous)
	}

	// The reverse example: NO read path hands back an id without the kind.
	// Every holder a reader can see carries both halves, and the pair is
	// stored as a pair — the columns are all-or-nothing.
	history := w.history(t)
	if len(history) != 2 {
		t.Fatalf("history = %+v, want the two events", history)
	}
	for i, event := range history {
		for label, identity := range map[string]*assetrights.PartyIdentity{
			"holder":   &event.Holder,
			"previous": event.Previous,
		} {
			if identity == nil {
				continue
			}
			if identity.Party.Kind == "" || identity.Party.ID == "" {
				t.Errorf("history[%d].%s = %+v: an identity without both its kind and its id is not a party", i, label, identity)
			}
			if !identity.Party.IsIdentity() {
				t.Errorf("history[%d].%s is a %q: a holder is a user or an organization", i, label, identity.Party.Kind)
			}
		}
	}
	var kindless int
	if err := w.pool.QueryRow(w.ctx,
		`SELECT count(*) FROM asset_rights_holder_events
		 WHERE holder_kind IS NULL OR holder_id IS NULL
		    OR (previous_holder_kind IS NULL) <> (previous_holder_id IS NULL)`).Scan(&kindless); err != nil {
		t.Fatalf("inspect the chain rows: %v", err)
	}
	if kindless != 0 {
		t.Errorf("%d chain rows carry half a party (an id without a kind, or a kind without an id)", kindless)
	}

	// The kind is consulted, not guessed from the id: a user id claimed as
	// an organization resolves to nothing and is refused, rather than
	// stored as a dangling reference no later reader could repair.
	_, err = w.transfer(t, w.aliceID, false, w.projectID, domain.Party{Kind: domain.PartyOrganization, ID: w.bobID})
	if !errors.Is(err, assetrights.ErrPartyNotFound) {
		t.Errorf("a user id claimed as an organization = %v, want ErrPartyNotFound (the kind names the table)", err)
	}
	_, err = w.transfer(t, w.aliceID, false, w.projectID, domain.Party{Kind: domain.PartyUser, ID: w.acmeID})
	if !errors.Is(err, assetrights.ErrPartyNotFound) {
		t.Errorf("an organization id claimed as a user = %v, want ErrPartyNotFound (the kind names the table)", err)
	}
	if got := w.eventCount(t); got != 2 {
		t.Errorf("the refused party lookups appended events: %d rows, want the 2 written", got)
	}
}

// --------------------------------------------------------------------------
// 7. the permission is owner-only and fails closed

func TestAssetGovernancePermissionIsOwnerOnlyFailClosed(t *testing.T) {
	w := newGovernanceWorld(t)

	// The owner may.
	if _, err := w.transfer(t, w.aliceID, false, w.projectID, domain.Party{Kind: domain.PartyUser, ID: w.aliceID}); err != nil {
		t.Fatalf("the project owner was refused: %v", err)
	}
	written := w.eventCount(t)
	if written != 1 {
		t.Fatalf("the owner's change wrote %d events, want 1", written)
	}

	// The maintainer may NOT: specs/policies/permissions-matrix.csv:13 is
	// `change_rights_holder,deny,deny,deny,deny,deny,allow,deny` — a plain
	// deny in the maintainer column, unlike the publish row's conditional.
	if _, err := w.transfer(t, w.bobID, false, w.projectID, domain.Party{Kind: domain.PartyUser, ID: w.bobID}); !errors.Is(err, assetrights.ErrForbidden) {
		t.Errorf("the maintainer's change = %v, want ErrForbidden", err)
	}
	// ... and the denial does not depend on the asset existing: a bogus pid
	// is refused the same way, so the refusal is not an oracle for which
	// pids are real.
	foreign := assetrights.ChangeParams{
		ProjectID: w.projectID,
		AssetPID:  "01j9z6k3m4n5p6q7r8s9t0v1w9",
		Holder:    domain.Party{Kind: domain.PartyUser, ID: w.bobID},
	}
	if _, err := w.cmd.ChangeRightsHolder(w.ctx, assetrights.Actor{User: domain.User{ID: w.bobID}}, foreign); !errors.Is(err, assetrights.ErrForbidden) {
		t.Errorf("the maintainer's change of an unknown pid = %v, want ErrForbidden (the denial precedes the lookup)", err)
	}

	// An authenticated NON-member may not either.
	if _, err := w.transfer(t, w.carolID, false, w.projectID, domain.Party{Kind: domain.PartyUser, ID: w.carolID}); !errors.Is(err, assetrights.ErrForbidden) {
		t.Errorf("the non-member's change = %v, want ErrForbidden", err)
	}

	// An agent may not, even as the owner: the domain backstop, which the
	// matrix cannot waive (specs/mcp/tools.json:
	// forbidden_default_agent_actions).
	if _, err := w.transfer(t, w.aliceID, true, w.projectID, domain.Party{Kind: domain.PartyUser, ID: w.bobID}); !errors.Is(err, assetrights.ErrAgentNotPermitted) {
		t.Errorf("the agent's change = %v, want ErrAgentNotPermitted", err)
	}

	// Nothing any refusal touched: the chain is still the one event the
	// owner's change wrote, and the audit has exactly one row.
	if got := w.eventCount(t); got != written {
		t.Errorf("a refused change appended an event: %d rows, want %d", got, written)
	}
	if got := w.rows(t,
		`SELECT count(*) FROM audit_log WHERE action = $1`, domain.ActionAssetRightsHolderChanged); got != 1 {
		t.Errorf("a refused change appended an audit row: %d rows, want 1", got)
	}
}

// TestAssetGovernanceAnonymousClassIsDeniedByTheMatrix is the THIRD arm of
// the fail-closed criterion (owner may, maintainer denied, ANONYMOUS
// denied), asked at the level where an anonymous caller exists.
//
// An anonymous caller cannot be handed to the command: assetrights.Command
// resolves the caller's class as authz.ClassOf(true, role, isAgent) — a
// value that reached the command came through the transport, and this
// action has no route yet, so "authenticated" is the only thing a caller
// of the command can be. The property "an anonymous caller may not change
// an asset's rights holder" is therefore a property of the MATRIX, and
// this test asks the matrix the question a transport would ask: it takes
// the class an unauthenticated request resolves to (the real ClassOf, not
// a string written here), and asks the real engine.
//
// What it catches is a future wiring that hands the command an
// unauthenticated caller — or a matrix edit that opens column 1 — while
// the rest of the suite, which only ever uses authenticated actors, would
// stay green.
func TestAssetGovernanceAnonymousClassIsDeniedByTheMatrix(t *testing.T) {
	class := authz.ClassOf(false, nil, false)
	if class != authz.ActorPublicAnonymous {
		t.Fatalf("an unauthenticated caller resolves to %q, want %q", class, authz.ActorPublicAnonymous)
	}

	engine := authz.NewMatrixEngine()
	decision, err := engine.Authorize(testCtx(t), authz.Request{
		Action: authz.ActionChangeRightsHolder,
		Class:  class,
	})
	if err != nil {
		t.Fatalf("the matrix could not decide: %v", err)
	}
	if decision.Permits() {
		t.Errorf("the anonymous class permits %s: %+v", authz.ActionChangeRightsHolder, decision)
	}
	if !decision.Denies() {
		t.Errorf("the anonymous class does not DENY %s: %+v (a conditional would still be fail-open here)", authz.ActionChangeRightsHolder, decision)
	}

	// The control that makes the refusal above mean something: the same
	// engine, the same class, asked about an action the anonymous column
	// DOES allow. Without it, a test whose engine answered "deny" to
	// everything would read exactly like this one.
	control, err := engine.Authorize(testCtx(t), authz.Request{Action: authz.ActionReadPublicProject, Class: class})
	if err != nil {
		t.Fatalf("the matrix could not decide the control: %v", err)
	}
	if !control.Permits() {
		t.Fatalf("the control action %s is not permitted for the anonymous class: %+v (the refusal above would prove nothing)", authz.ActionReadPublicProject, control)
	}

	// ... and the same question for every column of the row, so the test
	// states row 13 as a whole rather than one cell of it: the OWNER is the
	// only class the action admits (specs/policies/permissions-matrix.csv
	// line 13: ...deny,deny,allow,deny).
	for _, c := range authz.ActorClasses() {
		d, err := engine.Authorize(testCtx(t), authz.Request{Action: authz.ActionChangeRightsHolder, Class: c})
		if err != nil {
			t.Fatalf("the matrix could not decide for %q: %v", c, err)
		}
		if want := c == authz.ActorOwner; d.Permits() != want {
			t.Errorf("class %q permits %s = %v, want %v", c, authz.ActionChangeRightsHolder, d.Permits(), want)
		}
	}
}

// TestAssetGovernanceDuplicateCreatorIDIsRefusedNotStored: a publish that
// credits the same user twice is refused by the GATE, by name, before the
// transaction opens.
//
// Why this matters as an end-to-end test and not only as a gate unit test:
// 00082's asset_version_parties has UNIQUE (asset_version_id, role,
// party_id), so a duplicate declaration reaches the database as a
// constraint violation on the SECOND row — after the preview said the
// publish was fine and after the transaction opened. What the caller meets
// then is a store failure, not a refusal they can act on, and the version
// they were told was publishable turns out not to be. The gate refuses it
// instead, with a code of its own, and this test is the proof that the
// refusal reaches the publish: a refusal carrying the code, and no row
// written anywhere.
func TestAssetGovernanceDuplicateCreatorIDIsRefusedNotStored(t *testing.T) {
	w := newGovernanceWorld(t)
	before := w.rows(t, `SELECT count(*) FROM asset_version_parties`)
	versions := w.rows(t, `SELECT count(*) FROM research_asset_versions`)

	upper := strings.ToUpper(w.aliceID)
	if upper == w.aliceID {
		t.Fatal("fixture: the user id must have a case to change")
	}
	// Three spellings of the same repeat: the one the caller wrote twice,
	// the one a text comparison reads as two different users (a uuid is one
	// value whatever case its text is in, so it would land on the unique
	// index as an opaque store error), and the one that is both — padded
	// and in another case — which is the pair the gate must still answer as
	// one user, not as a second credit for the store to trip over.
	for i, ids := range [][]string{
		{w.aliceID, w.aliceID},
		{w.aliceID, upper},
		{w.aliceID, " " + upper + " "},
	} {
		cand := buildCandidate(t, candidateOptions{
			pid: w.assetPID, version: "9." + strconv.Itoa(8-i), visibility: "public",
			refs:     []string{"release:" + w.releaseID, "object_version:" + w.objectVersionID},
			creators: []string{w.aliceID, w.carolID},
			blobIDs:  []string{w.openBlobID}, accessLevel: "open", dataAccess: rights.DataAccessOpen,
		})
		if _, err := assets.Gate(cand.cand); err != nil {
			t.Fatalf("the fixture candidate is not publishable, so the refusal below would prove nothing: %v", err)
		}
		cand.cand.CreatorIDs = ids
		_, err := w.publish.Publish(w.ctx, assetpublish.Actor{
			User: domain.User{ID: w.aliceID},
		}, cand.params(w.projectID, nil))
		if err == nil {
			t.Fatalf("the publish of a version crediting %v was accepted", ids)
		}
		var refused *assetpublish.PublishRefused
		if !errors.As(err, &refused) {
			t.Fatalf("the refusal of %v = %v, want the publish's own refusal (a store error here means the duplicate reached the unique index)", ids, err)
		}
		named := false
		for _, blocker := range refused.Preview.PublishBlockers {
			if blocker.Code == assets.CodeDuplicateCreatorID {
				named = true
			}
		}
		if !named {
			t.Errorf("the refusal of %v does not name %s: %+v", ids, assets.CodeDuplicateCreatorID, refused.Preview.PublishBlockers)
		}
	}

	if after := w.rows(t, `SELECT count(*) FROM asset_version_parties`); after != before {
		t.Errorf("a refused publish changed the credit rows: %d before, %d after", before, after)
	}
	if after := w.rows(t, `SELECT count(*) FROM research_asset_versions`); after != versions {
		t.Errorf("a refused publish wrote a version row: %d before, %d after", versions, after)
	}
}

// TestAssetGovernanceProjectPairingIsEnforced: the authorization is
// resolved for the project the request names, so an asset of ANOTHER
// project cannot be written by naming a project the caller does own.
func TestAssetGovernanceProjectPairingIsEnforced(t *testing.T) {
	w := newGovernanceWorld(t)

	_, err := w.transfer(t, w.aliceID, false, w.foreignProjectID, domain.Party{Kind: domain.PartyUser, ID: w.aliceID})
	if !errors.Is(err, assetrights.ErrAssetNotFound) {
		t.Errorf("a change authorized in another project = %v, want ErrAssetNotFound: the asset's origin project is checked, not assumed", err)
	}
	if got := w.eventCount(t); got != 0 {
		t.Errorf("the mismatched project wrote %d events, want 0", got)
	}

	// The control, and the line the comparison draws: the asset's OWN
	// project written in another case is still that project. A uuid has one
	// value and several spellings, and the pairing is decided between the
	// request's parsed uuid and the row's — so a caller who spells their
	// own project id in caps is not answered "asset not found" (the
	// membership read parses it the same way, so the authorization already
	// resolved the project these rows belong to).
	upperProject := strings.ToUpper(w.projectID)
	if upperProject == w.projectID {
		t.Fatal("fixture: the project id must have a case to change")
	}
	if _, err := w.transfer(t, w.aliceID, false, upperProject, domain.Party{Kind: domain.PartyUser, ID: w.aliceID}); err != nil {
		t.Errorf("a change naming the asset's own project in another case = %v, want it accepted: it is the same uuid", err)
	}
	if got := w.eventCount(t); got != 1 {
		t.Errorf("the accepted change left %d events, want 1", got)
	}
}

// TestAssetGovernanceNoChangeIsRefusedNotAppended: a change to the holder
// the asset already has is a no-op, and the chain must not record an event
// that says nothing happened.
func TestAssetGovernanceNoChangeIsRefusedNotAppended(t *testing.T) {
	w := newGovernanceWorld(t)

	if _, err := w.transfer(t, w.aliceID, false, w.projectID, domain.Party{Kind: domain.PartyUser, ID: w.aliceID}); err != nil {
		t.Fatalf("designate the holder: %v", err)
	}
	if _, err := w.transfer(t, w.aliceID, false, w.projectID, domain.Party{Kind: domain.PartyUser, ID: w.aliceID}); !errors.Is(err, assetrights.ErrNoChange) {
		t.Errorf("a change to the current holder = %v, want ErrNoChange", err)
	}
	if _, err := w.transfer(t, w.aliceID, false, w.projectID, domain.Party{Kind: domain.PartyUser, ID: w.aliceID}); !errors.Is(err, assetrights.ErrNoChange) {
		t.Errorf("a repeated change to the current holder = %v, want ErrNoChange", err)
	}
	if got := w.eventCount(t); got != 1 {
		t.Errorf("the chain holds %d events after two no-op changes, want the 1 that happened", got)
	}

	// The database refuses the same row independently of the command: a
	// second event with the previous holder equal to the new holder is a
	// CHECK violation, so a future writer that skipped the command's
	// refusal cannot record a non-transfer either.
	if _, err := w.pool.Exec(w.ctx,
		`INSERT INTO asset_rights_holder_events
			(asset_id, ordinal, holder_kind, holder_id, previous_holder_kind, previous_holder_id, recorded_by)
		 VALUES ($1, 99, 'user', $2, 'user', $2, $2)`, w.assetID, w.aliceID); err == nil {
		t.Error("the database accepted a chain row whose previous holder IS the new holder")
	}
	// ... and a previous holder that is half-recorded is refused too.
	if _, err := w.pool.Exec(w.ctx,
		`INSERT INTO asset_rights_holder_events
			(asset_id, ordinal, holder_kind, holder_id, previous_holder_kind, previous_holder_id, recorded_by)
		 VALUES ($1, 98, 'user', $2, 'user', NULL, $2)`, w.assetID, w.aliceID); err == nil {
		t.Error("the database accepted a chain row with a previous holder kind and no id")
	}
}
