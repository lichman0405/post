package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// discover is the builder's READ-ONLY view of the database. It exists so a
// second run of the builder can find what the first one made instead of
// colliding with it: the product's write routes are create-only for objects
// and evidence (there is no "get by my key"), and the demo's identity is the
// marker the builder stamped into every payload.
//
// Nothing here writes. The build report names this path as "database (read)"
// for every item it resolves, so the distinction the task asks for — product
// API versus direct DB write — stays legible.
type discover struct{ pool *pgxpool.Pool }

func newDiscover(ctx context.Context, url string) (*discover, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	return &discover{pool: pool}, nil
}

func (d *discover) Close() { d.pool.Close() }

func (d *discover) Ping(ctx context.Context) error { return d.pool.Ping(ctx) }

// objectRecord is one seeded object as the database holds it now.
type objectRecord struct {
	ObjectID  string
	VersionID string
	VersionNo int
	BranchID  string
	StateID   string
	Title     string
}

// objectBySeedKey finds the object whose payload carries the seed marker.
// The marker is the plan key, which is unique across the whole plan — so
// this is what makes a re-run idempotent.
func (d *discover) objectBySeedKey(ctx context.Context, projectID, seedKey string) (objectRecord, bool, error) {
	var rec objectRecord
	err := d.pool.QueryRow(ctx, `
		select o.id, v.id, v.version_no, coalesce(v.branch_id::text, ''), v.state_id::text, v.title
		from scientific_objects o
		join scientific_object_versions v
		  on v.object_id = o.id and v.version_no = o.current_version_no
		where o.project_id = $1
		  and v.payload -> 'metadata' ->> 'seed_key' = $2
		limit 1`, projectID, seedKey).Scan(&rec.ObjectID, &rec.VersionID, &rec.VersionNo, &rec.BranchID, &rec.StateID, &rec.Title)
	if err != nil {
		if isNoRows(err) {
			return objectRecord{}, false, nil
		}
		return objectRecord{}, false, err
	}
	return rec, true, nil
}

// versionWithPayload reports whether the object already has a version in a
// state's lineage whose payload contains every field of the patch. It is how
// the builder recognises the one version it wrote out of order — the
// divergent protocol version it puts on main after the branch has already
// moved the same fields — without re-writing it on a second run.
func (d *discover) versionWithPayload(ctx context.Context, stateID, objectID string, patch map[string]any) (string, bool, error) {
	raw, err := json.Marshal(patch)
	if err != nil {
		return "", false, err
	}
	var versionID string
	err = d.pool.QueryRow(ctx, `
		with recursive lineage(id) as (
			select $1::uuid
			union
			select ps.parent_state_id from project_states ps
			join lineage l on ps.id = l.id
			where ps.parent_state_id is not null
		)
		select v.id::text
		from scientific_object_versions v
		where v.object_id = $2 and v.state_id in (select id from lineage)
		  and v.payload @> $3::jsonb
		order by v.version_no desc
		limit 1`, stateID, objectID, string(raw)).Scan(&versionID)
	if err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return versionID, true, nil
}

// currentVersionNo reads an object's current version number — the CAS value
// the :version route demands.
func (d *discover) currentVersionNo(ctx context.Context, objectID string) (int, error) {
	var n int
	if err := d.pool.QueryRow(ctx,
		`select current_version_no from scientific_objects where id = $1`, objectID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// relationExists reports whether a relation of this type already joins the
// two OBJECTS in this project, whatever versions it pins.
//
// The key is the object pair, not the version pair, because a re-run cannot
// reconstruct the version pair the first run wrote: a relation is pinned to
// the versions that were current when it was written (internal/domain: edges
// are version-pinned), and later stages revise some of those objects — the
// hypothesis revisions move hyp-h1 to a second version, and the merge
// re-materialises the branch's relation with re-pointed endpoints — while the
// relation row keeps naming the version of its own time. Asked by version
// pair, the question is "does the relation I would write today exist", which
// is false on a re-run for a relation that is in fact there.
//
// The looser question is the one the seed can answer, and it is the one it
// means: a second relation of the same type between the same two objects
// would restate the demo's own statement rather than add one. Relations the
// plan wants in two different places (the same pair on two branches) are not
// something the plan declares — and if it did, this check would answer the
// looser question and skip the second one, which is a limitation to know
// about rather than a silent one (see ops/seed-demo.md).
func (d *discover) relationExists(ctx context.Context, projectID, relType, sourceObjectID, targetObjectID string) (bool, error) {
	var one int
	err := d.pool.QueryRow(ctx, `
		select 1
		from relations r
		join relation_versions rv on rv.relation_id = r.id
		join scientific_object_versions sv on sv.id = rv.source_object_version_id
		join scientific_object_versions tv on tv.id = rv.target_object_version_id
		where r.project_id = $1 and rv.relation_type = $2
		  and sv.object_id = $3 and tv.object_id = $4
		limit 1`, projectID, relType, sourceObjectID, targetObjectID).Scan(&one)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// evidenceExists reports whether this assertion is already recorded: the same
// relation, between the same two objects, in this project.
//
// The key is the object pair for the same reason relationExists' is: an
// assertion pins the versions that were current when it was made, and a
// re-run reads the versions that are current now — the H1/H2 revisions move
// them. Asked by version triple, the check would report "missing" for an
// assertion that is present, and the builder would try to write it again.
func (d *discover) evidenceExists(ctx context.Context, projectID, targetObjectID, evidenceObjectID, relation string) (bool, error) {
	var one int
	err := d.pool.QueryRow(ctx, `
		select 1 from evidence_assertions ea
		join scientific_object_versions tv on tv.id = ea.target_object_version_id
		join scientific_object_versions ev on ev.id = ea.evidence_object_version_id
		where ea.project_id = $1 and tv.object_id = $2
		  and ev.object_id = $3 and ea.relation_type = $4
		limit 1`, projectID, targetObjectID, evidenceObjectID, relation).Scan(&one)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// branchNode is a branch as the database holds it.
type branchNode struct {
	ID         string
	Name       string
	Visibility string
	StateID    string
	Lifecycle  string
}

func (d *discover) branchByName(ctx context.Context, projectID, name string) (branchNode, bool, error) {
	var b branchNode
	err := d.pool.QueryRow(ctx, `
		select id, name, visibility, coalesce(base_state_id::text, ''), lifecycle_state
		from branches where project_id = $1 and name = $2`, projectID, name).
		Scan(&b.ID, &b.Name, &b.Visibility, &b.StateID, &b.Lifecycle)
	if err != nil {
		if isNoRows(err) {
			return branchNode{}, false, nil
		}
		return branchNode{}, false, err
	}
	return b, true, nil
}

// branchHeadStateID reads a branch's current head state: the base a new
// branch is cut from.
func (d *discover) branchHeadStateID(ctx context.Context, branchID string) (string, error) {
	var state string
	err := d.pool.QueryRow(ctx, `
		select coalesce(b.base_state_id::text, '')
		from branches b where b.id = $1`, branchID).Scan(&state)
	if err != nil {
		return "", err
	}
	if state != "" {
		return state, nil
	}
	// A branch that has never been written to may still have a base state
	// recorded in its git_ref lineage; fall back to the latest state of the
	// project so the caller can still cut a branch from it.
	err = d.pool.QueryRow(ctx, `
		select id::text from project_states where project_id = (select project_id from branches where id = $1)
		order by created_at desc limit 1`, branchID).Scan(&state)
	if err != nil {
		return "", err
	}
	return state, nil
}

// pullRequestFor finds a PR between two branches, whatever state it is in.
type prRecord struct {
	Number int64
	State  string
	ID     string
}

func (d *discover) pullRequestFor(ctx context.Context, projectID, sourceBranchID, targetBranchID string) (prRecord, bool, error) {
	var pr prRecord
	err := d.pool.QueryRow(ctx, `
		select number, state, id from pull_requests
		where project_id = $1 and source_branch_id = $2 and target_branch_id = $3
		order by number limit 1`, projectID, sourceBranchID, targetBranchID).
		Scan(&pr.Number, &pr.State, &pr.ID)
	if err != nil {
		if isNoRows(err) {
			return prRecord{}, false, nil
		}
		return prRecord{}, false, err
	}
	return pr, true, nil
}

// releaseByVersion reads a release row's id — the value an origin pin names
// (assets.KindRelease's value is the release's own uuid).
func (d *discover) releaseByVersion(ctx context.Context, projectID, version string) (string, bool, error) {
	var id string
	err := d.pool.QueryRow(ctx, `select id::text from releases where project_id = $1 and version = $2`,
		projectID, version).Scan(&id)
	if err != nil {
		if isNoRows(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return id, true, nil
}

// publicationOf reads the name a knowledge object version is published under,
// if it is published. The read is what makes the publish step re-runnable
// without guessing: the product refuses a second publication of one version
// ("a version is published to the network once, and a change is a new
// version", owner ruling L3-20260916-1) and answers that refusal as a
// KNOWLEDGE_PUBLISH_BLOCKED report whose reasons mention already_published —
// a refusal to READ, not to match on.
func (d *discover) publicationOf(ctx context.Context, objectVersionID string) (publicVersion, pid string, found bool, err error) {
	err = d.pool.QueryRow(ctx,
		`select public_version, pid from knowledge_publications where object_version_id = $1`,
		objectVersionID).Scan(&publicVersion, &pid)
	if err != nil {
		if isNoRows(err) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	return publicVersion, pid, true, nil
}

// assetVersionExists reports whether the asset already carries this version.
// A published asset version is immutable (the gate refuses the second publish
// with ASSET_VERSION_IMMUTABLE), so a re-run that finds one has nothing left
// to do for that asset — and re-publishing would be the wrong repair anyway:
// the demo's asset version IS the one the first run published.
func (d *discover) assetVersionExists(ctx context.Context, assetID, version string) (bool, error) {
	var exists bool
	err := d.pool.QueryRow(ctx, `
		select exists (select 1 from research_asset_versions where asset_id = $1 and version = $2)`,
		assetID, version).Scan(&exists)
	return exists, err
}

// assetBySlug finds an asset created by an earlier run of this builder.
func (d *discover) assetBySlug(ctx context.Context, projectID, slug string) (assetID, pid string, found bool, err error) {
	err = d.pool.QueryRow(ctx, `select id::text, pid from research_assets where origin_project_id = $1 and slug = $2`,
		projectID, slug).Scan(&assetID, &pid)
	if err != nil {
		if isNoRows(err) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	return assetID, pid, true, nil
}

// reviewKinds returns the kinds of review recorded on a PR, with decisions.
func (d *discover) reviewKinds(ctx context.Context, prID string) (map[string]string, error) {
	rows, err := d.pool.Query(ctx, `select review_kind, decision from reviews where pull_request_id = $1`, prID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, dec string
		if err := rows.Scan(&k, &dec); err != nil {
			return nil, err
		}
		out[k] = dec
	}
	return out, rows.Err()
}

func isNoRows(err error) bool {
	return err != nil && err.Error() == "no rows in result set"
}

var _ = fmt.Sprintf
