package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/rights"
)

// Path labels. Every checklist item in the build report carries one, because
// "which route made this row" is the question the demo seed has to answer
// honestly: a demo whose data was INSERTed by a script demonstrates nothing
// about the product.
const (
	pathAPI    = "product API (HTTP)"
	pathDBRead = "database (read-only discovery of an earlier run)"
)

type buildItem struct {
	Name   string `json:"name"`
	Status string `json:"status"` // created | reused | failed | skipped
	Path   string `json:"path"`
	Detail string `json:"detail,omitempty"`
}

type buildReport struct {
	Plan        string            `json:"plan"`
	APIBase     string            `json:"api_base"`
	ProjectID   string            `json:"project_id"`
	ProjectSlug string            `json:"project_slug"`
	Users       map[string]string `json:"user_ids"`
	Projects    map[string]string `json:"demo_project_ids"`
	Counts      map[string]int    `json:"counts"`
	Items       []buildItem       `json:"items"`
	Unavailable []string          `json:"unavailable"`
	Notes       []string          `json:"notes"`
}

type builder struct {
	api      *client
	external *client
	plan     *Plan
	db       *discover
	report   *buildReport
	refs     *refTable

	projectID      string
	userClients    map[string]*client
	projectClients map[string]*client
	projectActors  map[string]string
	orgIDs         map[string]string
	// forkProjectID is the project the external group's fork created. It is
	// what tells the two sessions apart (see apiFor): the demo project and
	// the fork have different members, so a write into one cannot be made
	// with the other's session.
	forkProjectID string
	branches      map[string]branchNode
	// expectRefusal is set while the builder is making a write the plan wants
	// the product to refuse (the protocol scientific conflict), so the
	// refusal is recorded as its own outcome rather than as a failure.
	expectRefusal bool
	// releases maps a release version ("R1.0") to its row id. A published
	// research asset pins the release it came from (internal/assets' gate:
	// origin_refs must name a release or a state, or the publish is refused
	// as unpinned), so the id has to survive from the release stage to the
	// asset stage.
	releases  map[string]string
	assetRefs map[string]assets.DependencyPin
}

type buildConfig struct {
	APIBase  string
	DBURL    string
	PlanPath string
	External bool
}

// runBuild performs the whole seed. A stage that cannot run records an item
// and, where the product may refuse for a reason outside the seed's control
// (a disabled fork route, a release gate), the build continues so one
// unavailable item does not hide the state of every other item. The verifier
// is what turns a shortfall into a red.
func runBuild(ctx context.Context, cfg buildConfig) (*buildReport, error) {
	plan, err := LoadPlan(cfg.PlanPath)
	if err != nil {
		return nil, err
	}
	db, err := newDiscover(ctx, cfg.DBURL)
	if err != nil {
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		return nil, fmt.Errorf("database %s is not reachable: %w", cfg.DBURL, err)
	}
	c, err := newClient(cfg.APIBase)
	if err != nil {
		return nil, err
	}
	b := &builder{
		api:            c,
		plan:           plan,
		db:             db,
		refs:           newRefTable(),
		branches:       map[string]branchNode{},
		releases:       map[string]string{},
		userClients:    map[string]*client{},
		projectClients: map[string]*client{},
		projectActors:  map[string]string{},
		orgIDs:         map[string]string{},
		assetRefs:      map[string]assets.DependencyPin{},
		report: &buildReport{
			Plan:     cfg.PlanPath,
			APIBase:  cfg.APIBase,
			Users:    map[string]string{},
			Projects: map[string]string{},
			Counts:   map[string]int{},
			// The lists start empty so the machine-readable report carries []
			// rather than null when nothing failed or nothing was skipped.
			Items:       []buildItem{},
			Unavailable: []string{},
			Notes:       []string{},
		},
	}

	type stage struct {
		name string
		run  func(context.Context) error
	}
	stages := []stage{
		{"accounts and project", b.stageProject},
		{"review routing", b.ensureReviewRouting},
		{"main line content", b.stageMain},
		{"humidity-40rh campaign and its merge", b.stage40RH},
		{"release R0.1", b.stageReleaseR01},
		{"conclusions on main", b.stageConclusions},
		{"humidity-70rh campaign and its merge", b.stage70RH},
		{"protocol-activation-180c branch and its conflict", b.stageProtocolConflict},
		{"mechanism-water-binding branch and selective publication", b.stageMechanism},
		{"release R1.0", b.stageReleaseR10},
		{"research assets", b.stageAssets},
		{"independent collaboration projects", b.stageCollaborationProjects},
	}
	if cfg.External {
		stages = append(stages, stage{"external contribution", b.stageExternal})
	} else {
		b.report.Unavailable = append(b.report.Unavailable,
			"external contribution: skipped by --external=0; the fork route needs the Gitea provisioning keys")
	}
	stages = append(stages, stage{"English presentation content", b.stageEnglishPresentation})
	for _, st := range stages {
		if err := st.run(ctx); err != nil {
			// A stage's own error is fatal: it means the builder could not
			// tell "unavailable" from "broken", and continuing would write a
			// demo whose shape the operator cannot trust.
			b.note("stage %q stopped: %v", st.name, err)
			return b.report, fmt.Errorf("stage %s: %w", st.name, err)
		}
	}
	b.report.ProjectID = b.projectID
	b.report.ProjectSlug = b.plan.Project.Slug
	b.report.Projects["source"] = b.projectID
	return b.report, nil
}

// stageEnglishPresentation appends English versions to existing synthetic
// records. Their earlier versions remain available as part of the audit trail.
func (b *builder) stageEnglishPresentation(ctx context.Context) error {
	var current struct {
		Purpose string `json:"purpose"`
	}
	if err := b.api.get("/api/v1/projects/"+b.projectID, &current); err != nil {
		return err
	}
	if current.Purpose != b.plan.Project.Purpose {
		if err := b.api.do("PATCH", "/api/v1/projects/"+b.projectID,
			map[string]any{"purpose": b.plan.Project.Purpose}, nil); err != nil {
			return err
		}
		b.item("English project purpose", "updated", pathAPI, "")
	}
	for _, revision := range b.plan.PresentationRevisions {
		if err := b.ensureDivergentVersion(ctx, revision); err != nil {
			return err
		}
	}
	if err := b.ensureRelations(ctx, b.projectID, "main", b.plan.PresentationRelations); err != nil {
		return err
	}
	return nil
}

func (b *builder) note(format string, args ...any) {
	b.report.Notes = append(b.report.Notes, fmt.Sprintf(format, args...))
}

func (b *builder) item(name, status, path, detail string) {
	b.report.Items = append(b.report.Items, buildItem{Name: name, Status: status, Path: path, Detail: detail})
}

// softItem records a failure without stopping the build. Used only where the
// product itself may refuse for a reason outside the seed's control; the
// verifier counts what actually landed.
func (b *builder) softItem(name, path string, err error) {
	b.item(name, "failed", path, err.Error())
	b.note("%s failed: %v", name, err)
}

// ---------------------------------------------------------------- accounts

func (b *builder) stageProject(ctx context.Context) error {
	owner := b.planUser("owner")
	if owner == nil {
		return fmt.Errorf("plan has no user with key owner")
	}
	ownerID, err := b.api.login(*owner)
	if err != nil {
		return err
	}
	b.refs.users["owner"] = ownerID
	b.report.Users["owner"] = ownerID
	b.userClients["owner"] = b.api
	b.item("user "+owner.Handle, "ok", pathAPI, "POST /api/v1/auth/signup or /login; user_id="+ownerID)

	orgIDs := map[string]string{}
	orgs := append([]PlanOrg{}, b.plan.Organizations...)
	orgs = append(orgs, b.plan.CollaborationDemo.Organizations...)
	for _, o := range orgs {
		if o.Key == "external" {
			continue // created by the external user in the external stage
		}
		id, status, err := b.ensureOrg(ctx, b.api, o)
		if err != nil {
			return err
		}
		orgIDs[o.Key] = id
		b.orgIDs[o.Key] = id
		b.item("organization "+o.Slug, status, pathAPI, "organization_id="+id)
	}
	if err := b.stageCollaborationAccounts(); err != nil {
		return err
	}
	if err := b.ensureOrganizationMemberships(orgIDs); err != nil {
		return err
	}

	var list struct {
		Projects []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"projects"`
	}
	if err := b.api.get("/api/v1/projects?limit=200", &list); err != nil {
		return fmt.Errorf("list projects: %w", err)
	}
	for _, p := range list.Projects {
		if p.Slug == b.plan.Project.Slug {
			b.projectID = p.ID
			break
		}
	}
	if b.projectID != "" {
		b.item("project "+b.plan.Project.Slug, "reused", pathAPI,
			"found by slug via GET /api/v1/projects; project_id="+b.projectID)
	} else {
		var created struct {
			Project struct {
				ID string `json:"id"`
			} `json:"project"`
		}
		body := map[string]any{
			"slug":       b.plan.Project.Slug,
			"name":       b.plan.Project.Name,
			"purpose":    b.plan.Project.Purpose,
			"visibility": b.plan.Project.Visibility,
		}
		if org := orgIDs[b.plan.Project.Org]; org != "" {
			body["organization_id"] = org
		}
		if err := b.api.post("/api/v1/projects", body, &created); err != nil {
			return fmt.Errorf("create project: %w", err)
		}
		b.projectID = created.Project.ID
		b.item("project "+b.plan.Project.Slug, "created", pathAPI, "project_id="+b.projectID)
	}

	if err := b.ensurePolicy(ctx); err != nil {
		return err
	}

	// The canonical line. base_ref "" asks the product for the project's
	// latest state and creates the genesis one when the project is empty —
	// which is exactly the "empty database" starting point.
	_, err = b.ensureBranch(ctx, b.planBranch("main"), "")
	return err
}

func (b *builder) planUser(key string) *PlanUser {
	for i := range b.plan.Users {
		if b.plan.Users[i].Key == key {
			return &b.plan.Users[i]
		}
	}
	for i := range b.plan.CollaborationDemo.Users {
		if b.plan.CollaborationDemo.Users[i].Key == key {
			return &b.plan.CollaborationDemo.Users[i]
		}
	}
	return nil
}

func (b *builder) planOrg(key string) *PlanOrg {
	for i := range b.plan.Organizations {
		if b.plan.Organizations[i].Key == key {
			return &b.plan.Organizations[i]
		}
	}
	for i := range b.plan.CollaborationDemo.Organizations {
		if b.plan.CollaborationDemo.Organizations[i].Key == key {
			return &b.plan.CollaborationDemo.Organizations[i]
		}
	}
	return nil
}

// stageCollaborationAccounts creates and authenticates every non-owner,
// non-fork account through the normal login/signup routes. Keeping one
// isolated cookie jar per account is what lets later project records carry
// the correct real actor identity.
func (b *builder) stageCollaborationAccounts() error {
	for _, user := range b.plan.CollaborationDemo.Users {
		c, err := newClient(b.api.base)
		if err != nil {
			return err
		}
		id, err := c.login(user)
		if err != nil {
			return err
		}
		b.userClients[user.Key] = c
		b.refs.users[user.Key] = id
		b.report.Users[user.Key] = id
		b.item("user "+user.Handle, "ok", pathAPI, "normal authenticated account; user_id="+id)
	}
	return nil
}

func (b *builder) ensureOrganizationMemberships(orgIDs map[string]string) error {
	for _, user := range b.plan.CollaborationDemo.Users {
		orgID := orgIDs[user.Org]
		if orgID == "" {
			return fmt.Errorf("demo user %s names unknown organization %q", user.Key, user.Org)
		}
		var current struct {
			Members []struct {
				UserID string `json:"user_id"`
			} `json:"members"`
		}
		path := "/api/v1/organizations/" + orgID + "/members"
		if err := b.api.get(path, &current); err != nil {
			return fmt.Errorf("list members for organization %s: %w", user.Org, err)
		}
		userID := b.refs.users[user.Key]
		found := false
		for _, member := range current.Members {
			if member.UserID == userID {
				found = true
				break
			}
		}
		if found {
			b.item("organization membership "+user.Handle, "reused", pathDBRead, "organization="+user.Org)
			continue
		}
		role := user.OrgRole
		if role == "" {
			role = "contributor"
		}
		if err := b.api.post(path, map[string]any{
			"handle": user.Handle, "role": role, "affiliation_start": "2025-01-01", "verified": false,
		}, nil); err != nil {
			return fmt.Errorf("add %s to organization %s: %w", user.Handle, user.Org, err)
		}
		b.item("organization membership "+user.Handle, "created", pathAPI, "organization="+user.Org+" role="+role+" synthetic affiliation unverified")
	}
	return nil
}

func (b *builder) planBranch(name string) PlanBranch {
	for _, br := range b.plan.Branches {
		if br.Name == name {
			return br
		}
	}
	// main is not in the plan's branch list: it is the project's canonical
	// line, created implicitly by the first branch write.
	return PlanBranch{Name: name, Visibility: "public", Purpose: "canonical research line (synthetic demo)"}
}

// ensureOrg creates the plan's organization, or finds the one an earlier run
// created.
//
// The read is the LIST route, not GET /organizations/{orgId}: that route's
// path segment is the organization's uuid (the handler reads it as orgId and
// the service resolves it through GetOrganization by id), so asking it for a
// slug answers 404 ORG_NOT_FOUND — for an organization that exists and that
// the list route shows. The list route is the one addressed by identity the
// caller actually holds (the slug), which is why it is the one a re-run uses.
func (b *builder) ensureOrg(ctx context.Context, c *client, o PlanOrg) (string, string, error) {
	var list struct {
		Organizations []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"organizations"`
	}
	if err := c.get("/api/v1/organizations", &list); err != nil {
		return "", "", fmt.Errorf("list organizations: %w", err)
	}
	for _, existing := range list.Organizations {
		if existing.Slug == o.Slug {
			return existing.ID, "reused", nil
		}
	}
	// The create route answers with the organization NESTED beside the
	// membership it also created (POST /organizations returns
	// {"organization": …, "membership": …}), unlike the list route, whose
	// rows are flat. Reading it flat would silently yield an empty id, and an
	// empty id is not harmless here: the project would be created without an
	// organization and land in the creator's personal namespace instead of
	// the lab's.
	var got struct {
		Organization struct {
			ID string `json:"id"`
		} `json:"organization"`
	}
	if err := c.post("/api/v1/organizations", map[string]any{
		"slug":        o.Slug,
		"name":        o.Name,
		"description": o.Description,
	}, &got); err != nil {
		return "", "", fmt.Errorf("create organization %s: %w", o.Slug, err)
	}
	if got.Organization.ID == "" {
		return "", "", fmt.Errorf("create organization %s: the route answered without an organization id", o.Slug)
	}
	return got.Organization.ID, "created", nil
}

// ensurePolicy sets the governance policy the demo's merges and releases run
// under. An absent main_protected rule is not a permission (the platform
// refuses to merge under a policy that is silent about main), so the seed
// states it — through the product's own policy route.
func (b *builder) ensurePolicy(ctx context.Context) error {
	var eff struct {
		EffectivePolicy json.RawMessage `json:"effective_policy"`
	}
	if err := b.api.get("/api/v1/projects/"+b.projectID+"/policy/effective", &eff); err == nil {
		var m map[string]any
		if json.Unmarshal(eff.EffectivePolicy, &m) == nil && m["main_protected"] == true {
			b.item("project policy main_protected", "reused", pathAPI, "already in force")
			return nil
		}
	}
	var created struct {
		Version string `json:"version"`
	}
	err := b.api.do(http.MethodPut, "/api/v1/projects/"+b.projectID+"/policy", map[string]any{
		"version": "seed-demo-v1",
		"policy":  map[string]any{"main_protected": true},
	}, &created)
	if err != nil {
		var ae *apiError
		if asAPIError(err, &ae) && (ae.Status == http.StatusConflict || ae.Status == http.StatusBadRequest) {
			b.item("project policy main_protected", "reused", pathAPI, "a version with this name already exists: "+ae.Error())
			return nil
		}
		return fmt.Errorf("set project policy: %w", err)
	}
	b.item("project policy main_protected", "created", pathAPI, "PUT /api/v1/projects/{id}/policy")
	return nil
}

// ensureBranch finds a branch by name or creates it from baseRef (a state id,
// which is what the route's base_ref takes).
func (b *builder) ensureBranch(ctx context.Context, br PlanBranch, baseRef string) (branchNode, error) {
	node, ok, err := b.db.branchByName(ctx, b.projectID, br.Name)
	if err != nil {
		return branchNode{}, err
	}
	if ok {
		b.branches[br.Name] = node
		b.item("branch "+br.Name, "reused", pathDBRead, "branch_id="+node.ID)
		return node, nil
	}
	var created struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Visibility string `json:"visibility"`
		BaseState  string `json:"base_state_id"`
		Lifecycle  string `json:"lifecycle_state"`
	}
	body := map[string]any{
		"name":       br.Name,
		"visibility": br.Visibility,
		"purpose":    br.Purpose,
	}
	if baseRef != "" {
		body["base_ref"] = baseRef
	}
	if err := b.apiFor(b.projectID).post("/api/v1/projects/"+b.projectID+"/branches", body, &created); err != nil {
		return branchNode{}, fmt.Errorf("create branch %s: %w", br.Name, err)
	}
	node = branchNode{ID: created.ID, Name: created.Name, Visibility: created.Visibility, StateID: created.BaseState, Lifecycle: created.Lifecycle}
	b.branches[br.Name] = node
	b.item("branch "+br.Name, "created", pathAPI, "branch_id="+node.ID+" base_state="+created.BaseState)
	return node, nil
}

// headOf reads a branch's current head state — the base a new branch is cut
// from. It is re-read every time because a merge moves it.
func (b *builder) headOf(ctx context.Context, name string) (string, error) {
	node, ok := b.branches[name]
	if !ok {
		return "", fmt.Errorf("branch %s has not been created yet", name)
	}
	return b.db.branchHeadStateID(ctx, node.ID)
}

// ---------------------------------------------------------------- objects

// markedPayload stamps the synthetic marker on a payload. There is no
// synthetic column in the database, so the marker rides in the payload:
// tags[0] and metadata.seed_key. One query selects the whole seed by it.
func (b *builder) markedPayload(key string, payload map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range payload {
		out[k] = v
	}
	tags := []any{b.plan.Marker.Tag}
	if existing, ok := out["tags"].([]any); ok {
		tags = append(tags, existing...)
	}
	out["tags"] = tags
	out["metadata"] = map[string]any{
		b.plan.Marker.MetadataKey: key,
		"seed_plan":               b.plan.Marker.Namespace,
	}
	if meta, ok := payload["metadata"].(map[string]any); ok {
		merged := out["metadata"].(map[string]any)
		for k, v := range meta {
			merged[k] = v
		}
	}
	return out
}

// ensureObject creates one object on a branch, or finds the one an earlier
// run created. The object route's response is flat, so both ids come back
// from one call.
// apiFor returns the session that is permitted to write projectID's
// scientific state.
//
// The permission is not global: write_scientific_state is resolved per
// project, and for an actor who is not a member of that project the matrix
// answers own_fork_only — internal/application/rsg's requireWrite then asks
// whether the project is that actor's OWN fork (ForkGate.OwnedFork, a read of
// the project_forks lineage the fork route wrote). So the parent's state is
// written by the demo owner and the fork's state by the external user,
// because those are the two memberships the plan created: writing the fork's
// objects with the owner's session is refused as forbidden, exactly as
// writing the parent's with the external session would be.
//
// That is a product rule, not a demo convenience: an experiment in the
// external group's fork is authored by the external group.
func (b *builder) apiFor(projectID string) *client {
	if b.external != nil && b.forkProjectID != "" && projectID == b.forkProjectID {
		return b.external
	}
	if c := b.projectClients[projectID]; c != nil {
		return c
	}
	return b.api
}

// actorFor names the actor a write into projectID is attributed to, on the
// same rule as apiFor. It is the blob fixture's created_by — a row the
// product would have stamped with the session that made it.
func (b *builder) actorFor(projectID string) string {
	if b.external != nil && b.forkProjectID != "" && projectID == b.forkProjectID {
		if id := b.refs.users[b.plan.External.User]; id != "" {
			return id
		}
	}
	if id := b.projectActors[projectID]; id != "" {
		return id
	}
	return b.refs.users["owner"]
}

func (b *builder) ensureObject(ctx context.Context, projectID, branchName string, o PlanObject) error {
	branch, ok := b.branches[branchName]
	if !ok {
		return fmt.Errorf("object %s names branch %s, which does not exist", o.Key, branchName)
	}
	// The file comes first, whoever owns the object: a payload that names a
	// blob has to name one that exists, because the merge's integrity engine
	// resolves every blob reference against the proposal's manifest.
	blobField, blobID, err := b.ensureObjectFile(ctx, o, b.actorFor(projectID))
	if err != nil {
		return err
	}
	if blobID != "" {
		// Recorded so a later plan reference can name the object's file:
		// "@blob:ds-iso-40" is the asset manifest's blob_ids entry.
		b.refs.setBlob(o.Key, blobID)
	}
	rec, found, err := b.db.objectBySeedKey(ctx, projectID, o.Key)
	if err != nil {
		return err
	}
	if found {
		b.refs.set(o.Key, rec.ObjectID, rec.VersionID)
		b.report.Counts[o.Type]++
		b.item("object "+o.Type+" "+o.Key, "reused", pathDBRead, "object_version_id="+rec.VersionID)
		if blobID != "" {
			if err := b.attachObjectFile(ctx, o, blobID, rec.VersionID, branchName); err != nil {
				return err
			}
		}
		return nil
	}
	marked := b.markedPayload(o.Key, o.Payload)
	if blobID != "" {
		marked[blobField] = []any{blobID}
	}
	payload, err := b.refs.Resolve(marked)
	if err != nil {
		return fmt.Errorf("object %s: %w", o.Key, err)
	}
	var created struct {
		ID        string `json:"id"`
		VersionID string `json:"version_id"`
		VersionNo int    `json:"version_no"`
	}
	if err := b.apiFor(projectID).post("/api/v1/projects/"+projectID+"/branches/"+branch.ID+"/objects", map[string]any{
		"object_type": o.Type,
		"payload":     payload,
	}, &created); err != nil {
		return fmt.Errorf("create %s %s on %s: %w", o.Type, o.Key, branchName, err)
	}
	b.refs.set(o.Key, created.ID, created.VersionID)
	b.report.Counts[o.Type]++
	b.item("object "+o.Type+" "+o.Key, "created", pathAPI,
		"object_id="+created.ID+" version_id="+created.VersionID)
	if blobID != "" {
		if err := b.attachObjectFile(ctx, o, blobID, created.VersionID, branchName); err != nil {
			return err
		}
	}
	return nil
}

// ensureVersionAt brings an object to at least version n by patching it. The
// protocol's "3 versions" item is this path.
func (b *builder) ensureVersionAt(ctx context.Context, projectID, branchName, key string, wanted int, patch map[string]any) error {
	branch, ok := b.branches[branchName]
	if !ok {
		return fmt.Errorf("protocol version of %s names branch %s, which does not exist", key, branchName)
	}
	objectID, err := b.refs.object(key)
	if err != nil {
		return err
	}
	current, err := b.db.currentVersionNo(ctx, objectID)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("version %d of %s", wanted, key)
	if current >= wanted {
		if rec, found, err := b.db.objectBySeedKey(ctx, projectID, key); err == nil && found {
			b.refs.set(key, rec.ObjectID, rec.VersionID)
		}
		b.item(name, "reused", pathDBRead, fmt.Sprintf("already at version %d", current))
		return nil
	}
	resolved, err := b.refs.Resolve(patch)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	var created struct {
		VersionNo int `json:"version_no"`
	}
	if err := b.apiFor(projectID).post("/api/v1/projects/"+projectID+"/branches/"+branch.ID+
		"/objects/"+objectID+":version", map[string]any{
		"expected_version": current,
		"patch":            resolved,
	}, &created); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if rec, found, err := b.db.objectBySeedKey(ctx, projectID, key); err == nil && found {
		b.refs.set(key, rec.ObjectID, rec.VersionID)
	}
	b.item(name, "created", pathAPI, fmt.Sprintf("version_no=%d", created.VersionNo))
	return nil
}

// ensureDivergentVersion writes the plan's "the target answers back" version:
// a NEW version on the target branch that moves the same fields the source
// branch already moved, to different values. It is what makes a scientific
// conflict exist at all — internal/rsg/conflict classifies a protocol object's
// diverging top-level field as a conflict only when BOTH sides moved it away
// from the version they share, and a branch that moved a field nobody else
// touched merges cleanly (docs/09 §7).
//
// It exists next to ensureVersionAt rather than as a flag on it because the
// two ask different questions. ensureVersionAt says "bring this object to at
// least version n" and is idempotent by version NUMBER — correct for the
// protocol's version history, where the demo wants three versions on main. A
// divergent write has no target number: the branch has already taken the next
// one, and the point is that the two sides disagree, so idempotency has to be
// by CONTENT (does the target's lineage already hold a version carrying this
// patch?). Version-number idempotency here would silently skip the write —
// and the demo would report a conflict it never created.
func (b *builder) ensureDivergentVersion(ctx context.Context, v PlanVersion) error {
	branch, ok := b.branches[v.Branch]
	if !ok {
		return fmt.Errorf("divergent version of %s names branch %s, which does not exist", v.Object, v.Branch)
	}
	objectID, err := b.refs.object(v.Object)
	if err != nil {
		return err
	}
	name := "divergent version of " + v.Object + " on " + v.Branch
	head, err := b.headOf(ctx, v.Branch)
	if err != nil {
		return err
	}
	resolved, err := b.refs.Resolve(v.Patch)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	patch, ok := resolved.(map[string]any)
	if !ok {
		return fmt.Errorf("%s: the patch is not an object", name)
	}
	existing, found, err := b.db.versionWithPayload(ctx, head, objectID, patch)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if found {
		b.item(name, "reused", pathDBRead, "version_id="+existing)
		return nil
	}
	current, err := b.db.currentVersionNo(ctx, objectID)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	var created struct {
		VersionNo int `json:"version_no"`
	}
	if err := b.api.post("/api/v1/projects/"+b.projectID+"/branches/"+branch.ID+
		"/objects/"+objectID+":version", map[string]any{
		"expected_version": current,
		"patch":            patch,
	}, &created); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if rec, found, err := b.db.objectBySeedKey(ctx, b.projectID, v.Object); err == nil && found {
		b.refs.set(v.Object, rec.ObjectID, rec.VersionID)
	}
	b.item(name, "created", pathAPI, fmt.Sprintf("version_no=%d", created.VersionNo))
	return nil
}

func (b *builder) ensureRelation(ctx context.Context, projectID, branchName string, r PlanRelation) error {
	branch, ok := b.branches[branchName]
	if !ok {
		return fmt.Errorf("relation names branch %s, which does not exist", branchName)
	}
	sourceVersion, err := b.refs.version(r.Source)
	if err != nil {
		return fmt.Errorf("relation %s->%s: %w", r.Source, r.Target, err)
	}
	targetVersion, err := b.refs.version(r.Target)
	if err != nil {
		return fmt.Errorf("relation %s->%s: %w", r.Source, r.Target, err)
	}
	sourceObject, err := b.refs.object(r.Source)
	if err != nil {
		return fmt.Errorf("relation %s->%s: %w", r.Source, r.Target, err)
	}
	targetObject, err := b.refs.object(r.Target)
	if err != nil {
		return fmt.Errorf("relation %s->%s: %w", r.Source, r.Target, err)
	}
	name := "relation " + r.Type + " " + r.Source + "->" + r.Target
	exists, err := b.db.relationExists(ctx, projectID, r.Type, sourceObject, targetObject)
	if err != nil {
		return err
	}
	if exists {
		b.item(name, "reused", pathDBRead, "")
		return nil
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := b.apiFor(projectID).post("/api/v1/projects/"+projectID+"/branches/"+branch.ID+"/relations", map[string]any{
		"relation_type":            r.Type,
		"source_object_version_id": sourceVersion,
		"target_object_version_id": targetVersion,
		"payload":                  map[string]any{},
	}, &created); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	b.item(name, "created", pathAPI, "")
	return nil
}

func (b *builder) ensureEvidence(ctx context.Context, projectID, branchName string, e PlanEvidence) error {
	branch, ok := b.branches[branchName]
	if !ok {
		return fmt.Errorf("evidence %s names branch %s, which does not exist", e.Key, branchName)
	}
	targetVersion, err := b.refs.version(e.Target)
	if err != nil {
		return fmt.Errorf("evidence %s: %w", e.Key, err)
	}
	evidenceVersion, err := b.refs.version(e.Evidence)
	if err != nil {
		return fmt.Errorf("evidence %s: %w", e.Key, err)
	}
	targetObject, err := b.refs.object(e.Target)
	if err != nil {
		return fmt.Errorf("evidence %s: %w", e.Key, err)
	}
	evidenceObject, err := b.refs.object(e.Evidence)
	if err != nil {
		return fmt.Errorf("evidence %s: %w", e.Key, err)
	}
	name := "evidence " + e.Relation + " " + e.Target + "<-" + e.Evidence
	exists, err := b.db.evidenceExists(ctx, projectID, targetObject, evidenceObject, e.Relation)
	if err != nil {
		return err
	}
	if exists {
		b.item(name, "reused", pathDBRead, "")
		return nil
	}
	var created struct {
		ID string `json:"id"`
	}
	body := map[string]any{
		"target_version_ref":   targetVersion,
		"evidence_version_ref": evidenceVersion,
		"relation":             e.Relation,
		"evidence_type":        e.EvidenceType,
		"directness":           e.Directness,
		"inference_nature":     e.InferenceNature,
		"reasoning_note":       e.ReasoningNote,
	}
	if err := b.apiFor(projectID).post("/api/v1/projects/"+projectID+"/branches/"+branch.ID+"/evidence-assertions", body, &created); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	b.item(name, "created", pathAPI, "evidence_assertion_id="+created.ID)
	return nil
}

func (b *builder) ensureObjects(ctx context.Context, projectID, branchName string, objects []PlanObject) error {
	for _, o := range objects {
		if err := b.ensureObject(ctx, projectID, branchName, o); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) ensureRelations(ctx context.Context, projectID, branchName string, rels []PlanRelation) error {
	for _, r := range rels {
		if err := b.ensureRelation(ctx, projectID, branchName, r); err != nil {
			return err
		}
	}
	return nil
}

func (b *builder) ensureEvidenceList(ctx context.Context, projectID, branchName string, evs []PlanEvidence) error {
	for _, e := range evs {
		if err := b.ensureEvidence(ctx, projectID, branchName, e); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- stages

// stageMain writes the baseline line on main: the question, the two
// hypotheses, the materials, samples, experiments, calculations, datasets and
// references, plus the protocol's own version history.
//
// It deliberately writes NO claim, finding or evidence. Those are the
// conclusions, and stageConclusions writes them after the first reviewed
// merge — see the Plan.MainConclusions comment for why the order is forced
// rather than chosen.
func (b *builder) stageMain(ctx context.Context) error {
	if err := b.ensureObjects(ctx, b.projectID, "main", b.plan.MainObjects); err != nil {
		return err
	}
	for i, v := range b.plan.ProtocolVersions {
		if err := b.ensureVersionAt(ctx, b.projectID, v.Branch, v.Object, i+2, v.Patch); err != nil {
			return err
		}
	}
	return b.ensureRelations(ctx, b.projectID, "main", b.plan.MainRelations)
}

// stage40RH builds the 40% RH campaign, opens the clean-merge PR, gets it
// reviewed and merges it. This merge is load-bearing twice over: a release can
// only be cut on a reviewed head, and it is what puts approved reviews into
// main's lineage so that everything written afterwards — on main or on a
// branch cut from it — can be published and can carry evidence.
func (b *builder) stage40RH(ctx context.Context) error {
	const branchName = "humidity-40rh"
	base, err := b.headOf(ctx, "main")
	if err != nil {
		return err
	}
	content := b.plan.BranchesContent[branchName]
	if _, err := b.ensureBranch(ctx, b.planBranch(branchName), base); err != nil {
		return err
	}
	if err := b.ensureObjects(ctx, b.projectID, branchName, content.Objects); err != nil {
		return err
	}
	if err := b.ensureRelations(ctx, b.projectID, branchName, content.Relations); err != nil {
		return err
	}
	pr, err := b.ensurePR(ctx, b.planPR("pr-clean-merge"))
	if err != nil {
		return err
	}
	if err := b.reviewPR(ctx, pr, "clean merge"); err != nil {
		b.softItem("reviews for the clean-merge PR", pathAPI, err)
	}
	// A refusal here is fatal, not a soft item: every claim the demo makes
	// after this point rests on main having a reviewed lineage.
	return b.mergePR(ctx, pr, "clean merge")
}

func (b *builder) stageReleaseR01(ctx context.Context) error {
	return b.ensureRelease(ctx, b.planRelease("R0.1"))
}

// stageConclusions writes what the project concluded from the campaigns —
// claim C1 (dry gas, main), claim C2 (40% RH, from the merged campaign) and
// finding F1 — and then publishes the versions the evidence will point at,
// because this product admits an evidence assertion only against published
// knowledge (internal/application/rsg/evidence.go: an unpublished target is
// EVIDENCE_REF_UNAVAILABLE). The order inside the stage is that rule:
// object → publish → assert.
//
// H1 and H2 are restated here as new versions before they are published, for
// the same reason: version 1 was written before the first merge and is not
// publishable.
func (b *builder) stageConclusions(ctx context.Context) error {
	if err := b.ensureObjects(ctx, b.projectID, "main", b.plan.MainConclusions); err != nil {
		return err
	}
	if err := b.ensureRelations(ctx, b.projectID, "main", b.plan.ConclusionRelations); err != nil {
		return err
	}
	for _, rev := range b.plan.HypothesisRevisions {
		if err := b.ensureVersionAt(ctx, b.projectID, rev.Branch, rev.Object, 2, rev.Patch); err != nil {
			return err
		}
	}
	if err := b.publishVersions(ctx, "hyp-h1", "hyp-h2", "claim-c1", "claim-c2"); err != nil {
		return err
	}
	return b.ensureEvidenceList(ctx, b.projectID, "main", b.plan.MainEvidence)
}

// stage70RH builds the 70% RH campaign, merges it — a second conflict-free
// merge, which docs/34 allows (it asks for at least one) — and then publishes
// its claim so the branch's own evidence can be asserted on main.
func (b *builder) stage70RH(ctx context.Context) error {
	const branchName = "humidity-70rh"
	base, err := b.headOf(ctx, "main")
	if err != nil {
		return err
	}
	content := b.plan.BranchesContent[branchName]
	if _, err := b.ensureBranch(ctx, b.planBranch(branchName), base); err != nil {
		return err
	}
	if err := b.ensureObjects(ctx, b.projectID, branchName, content.Objects); err != nil {
		return err
	}
	if err := b.ensureRelations(ctx, b.projectID, branchName, content.Relations); err != nil {
		return err
	}
	pr, err := b.ensurePR(ctx, b.planPR("pr-humidity-70rh"))
	if err != nil {
		return err
	}
	if err := b.reviewPR(ctx, pr, "humidity-70rh"); err != nil {
		b.softItem("reviews for the humidity-70rh PR", pathAPI, err)
	}
	if err := b.mergePR(ctx, pr, "humidity-70rh"); err != nil {
		return err
	}
	if err := b.publishVersions(ctx, "claim-c3", "find-f3"); err != nil {
		return err
	}
	return b.ensureEvidenceList(ctx, b.projectID, "main", content.Evidence)
}

// stageProtocolConflict builds the branch that disagrees with main about the
// activation protocol, then makes main disagree back: the same protocol
// fields move on both sides, which is a scientific conflict the merge must
// refuse (docs/34 §PR: "一个 Protocol scientific conflict").
func (b *builder) stageProtocolConflict(ctx context.Context) error {
	const branchName = "protocol-activation-180c"
	content := b.plan.BranchesContent[branchName]
	base, err := b.headOf(ctx, "main")
	if err != nil {
		return err
	}
	if _, err := b.ensureBranch(ctx, b.planBranch(branchName), base); err != nil {
		return err
	}
	const protocolVersion = 4 // versions 1..3 already exist on main
	if content.ProtocolVersion != nil {
		if err := b.ensureVersionAt(ctx, b.projectID, branchName, content.ProtocolVersion.Object, protocolVersion, content.ProtocolVersion.Patch); err != nil {
			return err
		}
	}
	pr, err := b.ensurePR(ctx, b.planPR("pr-protocol-conflict"))
	if err != nil {
		return err
	}
	// Now move main's version of the same object the other way. The branch
	// was cut before this write, so the two disagree about the same fields
	// and no merge order fixes that. The PR stays open: a scientific conflict
	// is a human's decision (CLAUDE.md §9 invariant 11), and the demo shows
	// it rather than resolving it.
	if content.MainDivergent != nil {
		if err := b.ensureDivergentVersion(ctx, *content.MainDivergent); err != nil {
			return err
		}
	}
	if err := b.reviewPR(ctx, pr, "protocol conflict"); err != nil {
		b.softItem("reviews for the protocol-conflict PR", pathAPI, err)
	}
	// Attempt the merge so the demo records the product's own refusal.
	if err := b.mergePRExpectingRefusal(ctx, pr, "protocol conflict"); err == nil {
		b.note("the protocol-conflict PR merged: the two sides did not diverge on the same field, so no scientific conflict was produced")
	}
	return nil
}

// stageMechanism builds the private mechanism branch, opens its PR, records
// the reviews, publishes three of its claims and its contested finding, and
// asserts the branch's evidence — and stops there. The branch is NOT merged:
// selective publication means the network got the conclusions the branch
// argues while the measurements underneath them (the aged material, the two
// calculations, the aged PXRD dataset) stay on a private branch, which is
// CLAUDE.md §9.6/§9.7 in one picture.
func (b *builder) stageMechanism(ctx context.Context) error {
	const branchName = "mechanism-water-binding"
	content := b.plan.BranchesContent[branchName]
	base, err := b.headOf(ctx, "main")
	if err != nil {
		return err
	}
	if _, err := b.ensureBranch(ctx, b.planBranch(branchName), base); err != nil {
		return err
	}
	if err := b.ensureObjects(ctx, b.projectID, branchName, content.Objects); err != nil {
		return err
	}
	if err := b.ensureRelations(ctx, b.projectID, branchName, content.Relations); err != nil {
		return err
	}
	pr, err := b.ensurePR(ctx, b.planPR("pr-selective-publication"))
	if err != nil {
		return err
	}
	if err := b.reviewPR(ctx, pr, "selective publication"); err != nil {
		b.softItem("reviews for the selective-publication PR", pathAPI, err)
	}
	if err := b.publishVersions(ctx, "claim-c4", "claim-c5", "claim-c6", "find-f2"); err != nil {
		return err
	}
	return b.ensureEvidenceList(ctx, b.projectID, branchName, content.Evidence)
}

func (b *builder) stageReleaseR10(ctx context.Context) error {
	return b.ensureRelease(ctx, b.planRelease("R1.0"))
}

// stageAssets publishes one asset of each of the four V1 types. The manifest
// is built with the product's own assets package, so the canonical JSON and
// the integrity hash are the ones the product computes — a second
// implementation of the canonical form in a seed script would be a way for
// the demo to "work" while proving nothing.
func (b *builder) stageAssets(ctx context.Context) error {
	originProject, ok := assets.NewOriginRef(assets.KindProject, b.projectID)
	if !ok {
		return fmt.Errorf("project id %q is not a usable origin ref", b.projectID)
	}
	releasePin, ok := b.releaseOriginRef()
	if !ok {
		return fmt.Errorf("source project has no release pin")
	}
	for _, a := range b.plan.Assets {
		if err := b.ensureAsset(ctx, a, originProject, releasePin); err != nil {
			return err
		}
	}
	return nil
}

// stageCollaborationProjects creates each independently owned research
// space, then writes its objects through that owner's own authenticated API
// session. The public projects become visible together while their project
// and object histories retain separate accountable creators.
func (b *builder) stageCollaborationProjects(ctx context.Context) error {
	for _, demo := range b.plan.CollaborationDemo.Projects {
		owner := b.planUser(demo.Owner)
		if owner == nil || b.userClients[demo.Owner] == nil {
			return fmt.Errorf("collaboration project %s names an unavailable owner %q", demo.Key, demo.Owner)
		}
		orgID := b.orgIDs[demo.Org]
		if orgID == "" {
			return fmt.Errorf("collaboration project %s names unknown organization %q", demo.Key, demo.Org)
		}
		api := b.userClients[demo.Owner]
		var listed struct {
			Projects []struct {
				ID   string `json:"id"`
				Slug string `json:"slug"`
			} `json:"projects"`
		}
		if err := api.get("/api/v1/projects?limit=200", &listed); err != nil {
			return fmt.Errorf("list projects for %s: %w", owner.Handle, err)
		}
		projectID := ""
		for _, existing := range listed.Projects {
			if existing.Slug == demo.Slug {
				projectID = existing.ID
				break
			}
		}
		status := "reused"
		if projectID == "" {
			var created struct {
				Project struct {
					ID string `json:"id"`
				} `json:"project"`
			}
			if err := api.post("/api/v1/projects", map[string]any{
				"slug": demo.Slug, "name": demo.Name, "purpose": demo.Purpose,
				"visibility": "public", "organization_id": orgID,
			}, &created); err != nil {
				return fmt.Errorf("create collaboration project %s: %w", demo.Slug, err)
			}
			projectID, status = created.Project.ID, "created"
		}
		if projectID == "" {
			return fmt.Errorf("collaboration project %s returned an empty id", demo.Slug)
		}
		b.report.Projects[demo.Key] = projectID
		b.projectClients[projectID] = api
		b.projectActors[projectID] = b.refs.users[demo.Owner]
		b.item("project "+demo.Slug, status, pathAPI, "project_id="+projectID+" owner="+owner.Handle)

		previousProject := b.projectID
		previousMain, hadMain := b.branches["main"]
		b.projectID = projectID
		branch, err := b.ensureBranch(ctx, PlanBranch{
			Name: "main", Visibility: "public", Purpose: "canonical line for the synthetic collaboration demo",
		}, "")
		if err != nil {
			b.projectID = previousProject
			return fmt.Errorf("collaboration project %s main branch: %w", demo.Slug, err)
		}
		b.branches["main"] = branch
		if err := b.ensureObjects(ctx, projectID, "main", demo.Objects); err != nil {
			b.projectID = previousProject
			if hadMain {
				b.branches["main"] = previousMain
			} else {
				delete(b.branches, "main")
			}
			return fmt.Errorf("collaboration project %s objects: %w", demo.Slug, err)
		}
		if err := b.ensureRelations(ctx, projectID, "main", demo.Relations); err != nil {
			b.projectID = previousProject
			if hadMain {
				b.branches["main"] = previousMain
			} else {
				delete(b.branches, "main")
			}
			return fmt.Errorf("collaboration project %s relations: %w", demo.Slug, err)
		}
		if len(demo.Assets) > 0 {
			head, err := b.headOf(ctx, "main")
			if err != nil {
				return err
			}
			statePin, ok := assets.NewOriginRef(assets.KindState, head)
			if !ok {
				return fmt.Errorf("collaboration project %s has invalid main state", demo.Slug)
			}
			projectPin, _ := assets.NewOriginRef(assets.KindProject, projectID)
			for _, a := range demo.Assets {
				if err := b.ensureAsset(ctx, a, projectPin, statePin); err != nil {
					return fmt.Errorf("collaboration project %s asset: %w", demo.Slug, err)
				}
			}
		}
		b.projectID = previousProject
		if hadMain {
			b.branches["main"] = previousMain
		} else {
			delete(b.branches, "main")
		}
	}
	return nil
}

// assetVersion is the version every asset this demo publishes carries. The
// demo publishes one version of each asset; the constant exists so the
// re-run's read and the first run's write cannot drift apart.
const assetVersion = "1.0.0"

func (b *builder) ensureAsset(ctx context.Context, a PlanAsset, originProject, sourcePin assets.OriginRef) error {
	versionID, err := b.refs.version(a.Object)
	if err != nil {
		return fmt.Errorf("asset %s: %w", a.Key, err)
	}
	originVersion, ok := assets.NewOriginRef(assets.KindObjectVersion, versionID)
	if !ok {
		return fmt.Errorf("asset %s: object version %q is not a usable origin ref", a.Key, versionID)
	}
	// A published version must pin the accepted source it came from: the
	// asset gate refuses a candidate whose origin refs name no release and
	// no state (ASSET_UNPINNED_SOURCE, docs/11 §3). The demo has both — the
	// asset is published from the release the demo cuts — so it names the
	// release, which is the stronger of the two pins.
	refs := []string{string(originProject), string(originVersion)}
	refs = append(refs, string(sourcePin))
	// The manifest's metadata is plan text with the same references every
	// other payload carries: a dataset asset's blob_ids has to name the file
	// the seed actually wrote, and that id is only known at run time.
	resolvedMeta, err := b.refs.Resolve(a.ManifestMetadata)
	if err != nil {
		return fmt.Errorf("asset %s manifest metadata: %w", a.Key, err)
	}
	metadata, ok := resolvedMeta.(map[string]any)
	if !ok {
		return fmt.Errorf("asset %s manifest metadata is not an object", a.Key)
	}
	pins := make([]assets.DependencyPin, 0, len(a.Dependencies))
	for _, dep := range a.Dependencies {
		pin, ok := b.assetRefs[dep]
		if !ok {
			return fmt.Errorf("asset %s depends on asset %s before it has been published", a.Key, dep)
		}
		pins = append(pins, pin)
	}
	manifest := assets.Manifest{
		Version:        assets.ManifestFormatVersion,
		AssetType:      assets.Type(a.AssetType),
		Metadata:       assets.Metadata(metadata),
		DependencyPins: pins,
	}
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("asset %s manifest: %w", a.Key, err)
	}
	raw, err := manifest.CanonicalJSON()
	if err != nil {
		return fmt.Errorf("asset %s: canonical manifest: %w", a.Key, err)
	}
	hash, err := manifest.Hash()
	if err != nil {
		return fmt.Errorf("asset %s: manifest hash: %w", a.Key, err)
	}
	rightsRaw, err := json.Marshal(seedRights())
	if err != nil {
		return err
	}
	body := map[string]any{
		"asset_pid":      "",
		"asset_type":     a.AssetType,
		"version":        assetVersion,
		"manifest":       json.RawMessage(raw),
		"rights":         json.RawMessage(rightsRaw),
		"origin_refs":    refs,
		"visibility":     "public",
		"integrity_hash": hash,
		"creator_ids":    b.assetCreatorIDs(a),
		"title":          a.Title,
		"slug":           a.Slug,
	}
	if assetID, pid, found, err := b.db.assetBySlug(ctx, b.projectID, a.Slug); err != nil {
		return err
	} else if found {
		// The asset exists. Whether anything is left to publish depends on
		// whether THIS version is already there: a published asset version is
		// immutable, and the gate refuses a second publish of it with
		// ASSET_VERSION_IMMUTABLE. On a re-run the demo's version is the one
		// the first run published, so the asset is done.
		published, err := b.db.assetVersionExists(ctx, assetID, assetVersion)
		if err != nil {
			return fmt.Errorf("asset %s: %w", a.Key, err)
		}
		if published {
			if pin, ok := assets.NewDependencyPin(assets.PID(pid), assetVersion); ok {
				b.assetRefs[a.Key] = pin
			}
			b.item("asset "+a.AssetType+" "+a.Slug, "reused", pathDBRead,
				"pid="+pid+" version="+assetVersion+" is already published (a published asset version is immutable)")
			return nil
		}
		// A version of an existing asset takes the pid and no slug/title
		// (the slug names the asset, which already exists).
		body["asset_pid"] = pid
		delete(body, "slug")
		delete(body, "title")
	}
	// The 201 body names the stored version by its PUBLIC identities, and the
	// asset's is `asset_pid` (assetshttp's publishedPayload: the internal
	// uuids are deliberately not on the wire). Reading `pid` here yielded an
	// empty string, and the item then printed "pid=" — the reader's only clue
	// that the two runs published the SAME asset, since the re-run reports the
	// pid it found. The second run's pid is the check: it has to match.
	var created struct {
		AssetPID string `json:"asset_pid"`
		Version  string `json:"version"`
	}
	if err := b.apiFor(b.projectID).post("/api/v1/projects/"+b.projectID+"/assets:publish", body, &created); err != nil {
		return fmt.Errorf("asset %s: %w", a.Key, err)
	}
	if pin, ok := assets.NewDependencyPin(assets.PID(created.AssetPID), created.Version); ok {
		b.assetRefs[a.Key] = pin
	}
	b.item("asset "+a.AssetType+" "+a.Slug, "created", pathAPI,
		"pid="+created.AssetPID+" version="+created.Version)
	return nil
}

func (b *builder) assetCreatorIDs(a PlanAsset) []string {
	keys := a.Creators
	if len(keys) == 0 {
		keys = []string{"owner"}
	}
	ids := make([]string, 0, len(keys))
	for _, key := range keys {
		if id := b.refs.users[key]; id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func (b *builder) planPR(key string) PlanPR {
	for _, pr := range b.plan.PullRequests {
		if pr.Key == key {
			return pr
		}
	}
	return PlanPR{}
}

// releaseOriginRef returns the newest release the run has cut, as the origin
// pin an asset publication names. The demo publishes its assets from the
// release it just cut, which is what docs/11 §3 asks a published version to
// be traceable to.
func (b *builder) releaseOriginRef() (assets.OriginRef, bool) {
	for i := len(b.plan.Releases) - 1; i >= 0; i-- {
		if id, ok := b.releases[b.plan.Releases[i].Version]; ok {
			if ref, ok := assets.NewOriginRef(assets.KindRelease, id); ok {
				return ref, true
			}
		}
	}
	return "", false
}

func (b *builder) planRelease(version string) PlanRelease {
	for _, r := range b.plan.Releases {
		if r.Version == version {
			return r
		}
	}
	return PlanRelease{}
}

// ensurePR creates the proposal between a branch and main, or finds the one
// an earlier run opened.
func (b *builder) ensurePR(ctx context.Context, pr PlanPR) (prRecord, error) {
	source, ok := b.branches[pr.Branch]
	if !ok {
		return prRecord{}, fmt.Errorf("PR %s names branch %s, which does not exist", pr.Key, pr.Branch)
	}
	target, ok := b.branches["main"]
	if !ok {
		return prRecord{}, fmt.Errorf("main branch is missing")
	}
	if existing, found, err := b.db.pullRequestFor(ctx, b.projectID, source.ID, target.ID); err != nil {
		return prRecord{}, err
	} else if found {
		b.item("pull request "+pr.Key, "reused", pathDBRead,
			fmt.Sprintf("number=%d state=%s", existing.Number, existing.State))
		// The review request is a step of its own in the report, so it is
		// reported here too rather than quietly missing: a re-run finds the PR
		// at or past review_required, which is the state that step produces.
		// Without this line the second run's item list is four items shorter
		// than the first's and nothing says why.
		if pr.Action != "leave_open" {
			b.item(fmt.Sprintf("request review of PR #%d", existing.Number), "reused", pathDBRead,
				fmt.Sprintf("an earlier run already moved this PR to state=%s", existing.State))
		}
		return existing, nil
	}
	var created struct {
		ID     string `json:"id"`
		Number int64  `json:"number"`
		State  string `json:"state"`
	}
	req := map[string]any{
		"source_branch_id": source.ID,
		"target_branch_id": target.ID,
		"title":            pr.Title,
		"body":             pr.Body,
	}
	// The route demands an Idempotency-Key, and it is the route's own word
	// for why: a repeated request must return the pull request it already
	// opened rather than opening a second one. The key is derived from the
	// plan key, so a re-run replays instead of duplicating.
	path := "/api/v1/projects/" + b.projectID + "/pull-requests"
	reqRaw, err := json.Marshal(req)
	if err != nil {
		return prRecord{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.api.base+path, bytes.NewReader(reqRaw))
	if err != nil {
		return prRecord{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Origin", b.api.base)
	httpReq.Header.Set("X-CSRF-Token", b.api.csrf)
	httpReq.Header.Set("Idempotency-Key", "seed-demo-pr-"+pr.Key)
	if err := b.api.send(httpReq, &created); err != nil {
		return prRecord{}, fmt.Errorf("open PR %s: %w", pr.Key, err)
	}
	rec := prRecord{ID: created.ID, Number: created.Number, State: created.State}
	b.item("pull request "+pr.Key, "created", pathAPI,
		fmt.Sprintf("number=%d state=%s", created.Number, created.State))

	// The lifecycle is a product rule, not a formality: docs/43's map runs
	// open -> review_required -> approved -> merge_ready -> merged, and only
	// merge_ready merges. The PR is sent to review here and the aggregation
	// inside the review command is what advances it once both dimensions are
	// approved — the demo drives the states, it does not assert them.
	if pr.Action != "leave_open" {
		if err := b.requestReview(ctx, rec); err != nil {
			return prRecord{}, err
		}
		rec.State = "review_required"
	}
	return rec, nil
}

// requestReview moves an open PR to review_required. A re-run finds the PR
// past this point already, and the product answers that with a state conflict
// rather than a silent success — which is the answer this treats as "done".
func (b *builder) requestReview(ctx context.Context, pr prRecord) error {
	name := fmt.Sprintf("request review of PR #%d", pr.Number)
	var out struct {
		State string `json:"state"`
	}
	err := b.api.post(fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d:request-review", b.projectID, pr.Number),
		map[string]any{}, &out)
	if err != nil {
		var ae *apiError
		if asAPIError(err, &ae) && ae.is("PR_STATE_CONFLICT") {
			b.item(name, "reused", pathAPI, "an earlier run already moved this PR past review_required")
			return nil
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	b.item(name, "created", pathAPI, "state="+out.State)
	return nil
}

// reviewPR records an approved scientific and an approved integrity review on
// the PR. Publication and release both demand those two dimensions approved;
// there is no product route that grants them without a review record, and the
// demo must not pretend otherwise.
func (b *builder) reviewPR(ctx context.Context, pr prRecord, label string) error {
	have, err := b.db.reviewKinds(ctx, pr.ID)
	if err != nil {
		return err
	}
	for _, kind := range []string{"scientific", "integrity"} {
		if have[kind] == "approved" {
			b.item("review "+kind+" "+label, "reused", pathDBRead, "")
			continue
		}
		var created struct {
			ID string `json:"id"`
		}
		path := fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d/reviews", b.projectID, pr.Number)
		if err := b.api.post(path, map[string]any{
			"kind":     kind,
			"decision": "approved",
			"body":     "Synthetic demo review: the seeded content is internally consistent (examples/seed-demo/README.md).",
		}, &created); err != nil {
			return fmt.Errorf("record %s review: %w", kind, err)
		}
		b.item("review "+kind+" "+label, "created", pathAPI, "review_id="+created.ID)
	}
	return nil
}

// mergePR merges with the product's required idempotency key. A second run
// replays the recorded merge rather than merging twice.
// mergePRExpectingRefusal attempts the merge the demo wants the product to
// refuse: docs/34 §PR asks for a protocol scientific conflict, and the point
// of the item is the refusal (internal/rsg/conflict refuses a diverging
// scientific field and never resolves it itself — CLAUDE.md §9 invariant 11
// puts that decision with a human). The refusal is recorded with its own
// status so it is not confused with a broken item.
func (b *builder) mergePRExpectingRefusal(ctx context.Context, pr prRecord, label string) error {
	b.expectRefusal = true
	defer func() { b.expectRefusal = false }()
	return b.mergePR(ctx, pr, label)
}

func (b *builder) mergePR(ctx context.Context, pr prRecord, label string) error {
	name := "merge " + label
	if pr.State == "merged" {
		b.item(name, "reused", pathDBRead, fmt.Sprintf("PR %d already merged", pr.Number))
		// The refresh runs on a re-run too: what it repairs is a property of
		// the merge, and the second run reads its references the same way the
		// first one did.
		return b.refreshRefsAfterMerge(ctx, label)
	}
	path := fmt.Sprintf("/api/v1/projects/%s/pull-requests/%d:merge", b.projectID, pr.Number)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.api.base+path,
		strings.NewReader(`{"message":"seed-demo: merge the synthetic demo PR"}`))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", b.api.base)
	req.Header.Set("X-CSRF-Token", b.api.csrf)
	req.Header.Set("Idempotency-Key", "seed-demo-merge-"+strings.ReplaceAll(label, " ", "-"))
	resp, err := b.api.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		StateID  string `json:"state_id"`
		Replayed bool   `json:"replayed"`
		Applied  int    `json:"applied"`
		Carried  int    `json:"carried"`
		Withheld int    `json:"withheld"`
		Code     string `json:"code"`
		Message  string `json:"message"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode >= 400 {
		// A refused merge is a RESULT here, not a crash: the demo's conflict
		// PR is supposed to be refused, and recording the product's own
		// reason is more useful than a stack trace.
		status := "failed"
		if b.expectRefusal {
			// A refusal the plan ASKED for — see mergePRExpectingRefusal. It
			// is recorded in its own status so a reader (and the ops summary)
			// can tell "the product refused the conflict, as docs/34 asks"
			// from "an item of the build is broken".
			status = "refused"
		}
		b.item(name, status, pathAPI, fmt.Sprintf("HTTP %d %s: %s", resp.StatusCode, out.Code, out.Message))
		b.note("%s %s: HTTP %d %s: %s", name, status, resp.StatusCode, out.Code, out.Message)
		return &apiError{Status: resp.StatusCode, Code: out.Code, Message: out.Message}
	}
	b.item(name, "created", pathAPI,
		fmt.Sprintf("state_id=%s applied=%d carried=%d withheld=%d replayed=%v",
			out.StateID, out.Applied, out.Carried, out.Withheld, out.Replayed))
	return b.refreshRefsAfterMerge(ctx, label)
}

// refreshRefsAfterMerge re-reads every seeded object's current version once a
// merge has landed, and re-attaches the merged objects' files to the versions
// the merge created.
//
// A merge in this product does not adopt the branch's versions. The merge
// state's parent is the TARGET head, not the branch head (/scientific_object
// _versions: every merged object of the 40% RH campaign came back as version
// 2 in one main state, while its version 1 stayed on the branch), so from the
// merge onward:
//
//   - every id the builder held for an object the branch touched names a
//     version that is NOT in main's lineage. A later write that resolves
//     "@v:ds-iso-40" through those stale ids pins a version main's manifest
//     does not contain, and the product refuses the next proposal that
//     carries it (internal/rsg/integrity's relation_endpoints_in_proposal);
//   - the merged versions carry the same blob_ids payload, but the blob
//     ATTACHMENT still belongs to the branch's version and state. The
//     manifest reads blobs through blob_attachments filtered by the
//     attachment's own state (queries/manifest.sql), so a main version whose
//     attachment stayed on the branch resolves to no blob ref at all
//     (internal/rsg/integrity's blob_refs_resolve — blocking, and it reads
//     the whole proposed manifest, not just the diff).
//
// Both are properties of the merge, not of the demo: any client that keeps
// writing after a merge has to re-read what main now holds. The refresh is
// therefore unconditional and safe to repeat — it only ever moves a
// reference to the version the database already reports as current.
func (b *builder) refreshRefsAfterMerge(ctx context.Context, label string) error {
	name := "refresh references after merging " + label
	moved, reattached := 0, 0
	for _, key := range b.plan.ObjectKeys() {
		if _, ok := b.refs.versions[key]; !ok {
			// The plan declares an object this run has not written yet: a
			// branch cut after this merge, whose objects are deliberately
			// not part of it.
			continue
		}
		rec, found, err := b.db.objectBySeedKey(ctx, b.projectID, key)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if !found {
			return fmt.Errorf("%s: object %s is not in the database", name, key)
		}
		if b.refs.versions[key] == rec.VersionID {
			continue
		}
		b.refs.set(key, rec.ObjectID, rec.VersionID)
		moved++
		o, ok := b.plan.ObjectByKey(key)
		if !ok || o.File == nil {
			continue
		}
		blobID, found, err := (&blobStore{pool: b.db.pool}).findBlobByContent(ctx, o.File.Content)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if !found {
			return fmt.Errorf("%s: the file of %s has no blob row; it was never written", name, key)
		}
		// The merged version lives on main, not on the branch that proposed
		// it: the name says which line the attachment now belongs to.
		if err := b.attachObjectFile(ctx, o, blobID, rec.VersionID, "main"); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		reattached++
	}
	b.item(name, "reused", pathDBRead,
		fmt.Sprintf("%d object reference(s) moved to the merged version, %d file attachment(s) re-made", moved, reattached))
	return nil
}

func (b *builder) ensureRelease(ctx context.Context, rel PlanRelease) error {
	if rel.Version == "" {
		return fmt.Errorf("plan has no release named %q", rel.Version)
	}
	if id, exists, err := b.db.releaseByVersion(ctx, b.projectID, rel.Version); err != nil {
		return err
	} else if exists {
		b.releases[rel.Version] = id
		b.item("release "+rel.Version, "reused", pathDBRead, "release_id="+id)
		return nil
	}
	var created struct {
		ID           string `json:"id"`
		ManifestHash string `json:"manifest_hash"`
	}
	err := b.api.post("/api/v1/projects/"+b.projectID+"/releases", map[string]any{
		"version": rel.Version,
		"title":   rel.Title,
	}, &created)
	if err != nil {
		b.softItem("release "+rel.Version, pathAPI, err)
		return nil
	}
	b.releases[rel.Version] = created.ID
	b.item("release "+rel.Version, "created", pathAPI,
		"release_id="+created.ID+" manifest_hash="+truncate(created.ManifestHash, 20))
	return nil
}

// publishVersions publishes the named plan versions through knowledge:publish
// — the product route that turns a reviewed version into network-readable
// knowledge. It is a hard error rather than a soft item because the caller
// publishes precisely the versions its next write asserts evidence about: an
// unpublished target is refused by the evidence route, so a swallowed failure
// here would surface later as a puzzling 404.
func (b *builder) publishVersions(ctx context.Context, keys ...string) error {
	rightsRaw, err := json.Marshal(seedRights())
	if err != nil {
		return err
	}
	for _, key := range keys {
		ref, ok := b.plan.publishRef(key)
		if !ok {
			return fmt.Errorf("the plan names no publication for %s", key)
		}
		versionID, err := b.refs.version(key)
		if err != nil {
			return fmt.Errorf("publish %s: %w", key, err)
		}
		// Ask before writing. A publication is once per version by the owner's
		// ruling, so on a re-run the answer is "already published", and the
		// interesting question is under WHICH name: the plan's name means the
		// first run did this, another name means the database and the plan
		// disagree about what this version is called — a real problem, and not
		// one to paper over as reuse.
		if existing, pid, found, err := b.db.publicationOf(ctx, versionID); err != nil {
			return fmt.Errorf("publish %s: %w", key, err)
		} else if found {
			if existing != ref.PublicVersion {
				return fmt.Errorf("publish %s: version %s is already published as %q, but the plan calls it %q",
					key, versionID, existing, ref.PublicVersion)
			}
			b.item("publication "+key, "reused", pathDBRead,
				"already published as "+existing+" (pid="+pid+")")
			continue
		}
		var created struct {
			PID           string `json:"pid"`
			PublicVersion string `json:"public_version"`
		}
		err = b.api.post("/api/v1/projects/"+b.projectID+"/knowledge:publish", map[string]any{
			"knowledge_version_ref": versionID,
			"rights":                json.RawMessage(rightsRaw),
			"public_version":        ref.PublicVersion,
		}, &created)
		if err != nil {
			var ae *apiError
			if asAPIError(err, &ae) && ae.is("KNOWLEDGE_VERSION_ALREADY_PUBLISHED") {
				b.item("publication "+key, "reused", pathDBRead, "published by an earlier run as "+ref.PublicVersion)
				continue
			}
			return fmt.Errorf("publish %s as %s: %w", key, ref.PublicVersion, err)
		}
		b.item("publication "+key, "created", pathAPI, "public_version="+ref.PublicVersion+" knowledge_pid="+created.PID)
	}
	return nil
}

// ---------------------------------------------------------------- external

func (b *builder) stageExternal(ctx context.Context) error {
	ext := b.plan.External
	user := b.planUser(ext.User)
	if user == nil {
		return fmt.Errorf("plan has no user %q for the external contribution", ext.User)
	}
	extClient, err := newClient(b.api.base)
	if err != nil {
		return err
	}
	userID, err := extClient.login(*user)
	if err != nil {
		return err
	}
	b.refs.users[ext.User] = userID
	b.report.Users[ext.User] = userID
	b.external = extClient
	b.userClients[ext.User] = extClient
	b.item("user "+user.Handle+" (external)", "ok", pathAPI, "user_id="+userID)

	if org := b.planOrg(ext.Org); org != nil {
		id, status, err := b.ensureOrg(ctx, extClient, *org)
		if err != nil {
			return err
		}
		b.item("organization "+org.Slug+" (external)", status, pathAPI, "organization_id="+id)
		b.orgIDs[ext.Org] = id
	}

	mainBranch, ok := b.branches["main"]
	if !ok {
		return fmt.Errorf("main branch is missing")
	}
	// Fork: the route is idempotent by (parent, actor) and answers
	// already_forked on a repeat, which is the second half of the
	// re-run-safety story.
	var fork struct {
		Fork struct {
			ForkedSHA string `json:"forked_sha"`
		} `json:"fork"`
		Project struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"project"`
		Branch struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"branch"`
		AlreadyForked bool `json:"already_forked"`
		Imported      bool `json:"imported"`
	}
	err = extClient.post("/api/v1/projects/"+b.projectID+"/forks", map[string]any{
		"source_branch_id": mainBranch.ID,
		"name":             ext.Fork.Name,
		"purpose":          ext.Fork.Purpose,
		"visibility":       ext.Fork.Visibility,
		"branch_name":      ext.Fork.BranchName,
	}, &fork)
	if err != nil {
		return fmt.Errorf("fork (the route needs POST_GITEA_BASE_URL, POST_GITEA_TOKEN and POST_GITEA_WEBHOOK_URL): %w", err)
	}
	forkProjectID, forkBranchID := fork.Project.ID, fork.Branch.ID
	b.projectClients[forkProjectID] = extClient
	b.projectActors[forkProjectID] = userID
	status := "created"
	if fork.AlreadyForked {
		status = "reused"
	}
	b.item("fork by the external group", status, pathAPI,
		fmt.Sprintf("fork_project=%s branch=%s forked_sha=%s imported=%v",
			forkProjectID, forkBranchID, truncate(fork.Fork.ForkedSHA, 12), fork.Imported))

	// The external group's own content lands in ITS project, through the
	// same object route, as the external user.
	b.forkProjectID = forkProjectID
	b.branches["__fork__"] = branchNode{ID: forkBranchID, Name: ext.Fork.BranchName}
	for _, o := range ext.Objects {
		if err := b.ensureObject(ctx, forkProjectID, "__fork__", o); err != nil {
			return err
		}
	}
	for _, r := range ext.Relations {
		if err := b.ensureRelation(ctx, forkProjectID, "__fork__", r); err != nil {
			return err
		}
	}

	// The evidence assertion is made inside the fork, by the external user,
	// against the parent's claim version. That cross-project direction is
	// what makes the assertion evidence_origin='external'.
	if err := b.ensureExternalEvidence(ctx, forkProjectID, forkBranchID); err != nil {
		b.softItem("external contradictory evidence assertion", pathAPI, err)
	}

	// The external PR: a source branch in the fork proposing to the parent's
	// main. The product allows it because the recorded fork lineage backs it
	// (pull_request_fork_gate) — that is the external contribution.
	return b.openExternalPR(ctx, forkProjectID, forkBranchID, mainBranch.ID)
}

// ensureExternalEvidence records the external group's assertions. Each runs
// against the fork project (the evidence lives in the external group's own
// state, and an assertion whose EVIDENCE end were foreign would be refused)
// while the target is a version of the parent — the contradictory finding
// against the parent's contested claim, and the reference to H1. A
// cross-project target is a first-class case for this route: it is what makes
// the recorded origin external (internal/application/rsg's
// AssertionIsExternal), and it needs the target to be published to the
// network, which is why the plan publishes H1.
func (b *builder) ensureExternalEvidence(ctx context.Context, projectID, branchID string) error {
	if b.external == nil {
		return fmt.Errorf("no external session")
	}
	for _, e := range b.plan.External.Evidence {
		targetVersion, err := b.refs.version(e.Target)
		if err != nil {
			return err
		}
		evidenceVersion, err := b.refs.version(e.Evidence)
		if err != nil {
			return err
		}
		targetObject, err := b.refs.object(e.Target)
		if err != nil {
			return err
		}
		evidenceObject, err := b.refs.object(e.Evidence)
		if err != nil {
			return err
		}
		name := "external evidence " + e.Relation + " " + e.Target + "<-" + e.Evidence
		exists, err := b.db.evidenceExists(ctx, projectID, targetObject, evidenceObject, e.Relation)
		if err != nil {
			return err
		}
		if exists {
			b.item(name, "reused", pathDBRead, "")
			continue
		}
		var created struct {
			ID string `json:"id"`
		}
		body := map[string]any{
			"target_version_ref":   targetVersion,
			"evidence_version_ref": evidenceVersion,
			"relation":             e.Relation,
			"evidence_type":        e.EvidenceType,
			"directness":           e.Directness,
			"inference_nature":     e.InferenceNature,
			"reasoning_note":       e.ReasoningNote,
		}
		if err := b.external.post("/api/v1/projects/"+projectID+"/branches/"+branchID+"/evidence-assertions", body, &created); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		b.item(name, "created", pathAPI, "evidence_assertion_id="+created.ID)
	}
	return nil
}

func (b *builder) openExternalPR(ctx context.Context, forkProjectID, sourceBranchID, targetBranchID string) error {
	pr := b.plan.External.PR
	if existing, found, err := b.db.pullRequestFor(ctx, b.projectID, sourceBranchID, targetBranchID); err != nil {
		return err
	} else if found {
		b.item("pull request external contribution", "reused", pathDBRead,
			fmt.Sprintf("number=%d state=%s", existing.Number, existing.State))
		return nil
	}
	var created struct {
		ID     string `json:"id"`
		Number int64  `json:"number"`
		State  string `json:"state"`
	}
	// The same Idempotency-Key contract the project's own PR route has, and
	// for the same reason: the route refuses a request without one, because a
	// repeated request must return the pull request it already opened. The key
	// is the plan's PR key, so the external contribution is replayable too.
	path := "/api/v1/projects/" + b.projectID + "/pull-requests"
	reqRaw, err := json.Marshal(map[string]any{
		"source_branch_id": sourceBranchID,
		"target_branch_id": targetBranchID,
		"title":            pr.Title,
		"body":             pr.Body,
	})
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, b.external.base+path, bytes.NewReader(reqRaw))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("Origin", b.external.base)
	httpReq.Header.Set("X-CSRF-Token", b.external.csrf)
	httpReq.Header.Set("Idempotency-Key", "seed-demo-pr-"+pr.Key)
	if err := b.external.send(httpReq, &created); err != nil {
		return fmt.Errorf("external PR from fork project %s: %w", forkProjectID, err)
	}
	b.item("pull request external contribution", "created", pathAPI,
		fmt.Sprintf("number=%d state=%s", created.Number, created.State))
	return nil
}

// seedRights is the rights document every publication and asset in the seed
// carries. visibility.metadata = project_policy is what lets a publication
// have network audience; a different value would keep it members-only.
func seedRights() rights.Document {
	doc := rights.New()
	note := "synthetic demo content (T1201 seed builder)"
	doc.Notes = &note
	return doc
}
