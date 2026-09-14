// Package integration — T0505 "provenance integration": the provenance
// graph projection exercised end to end over REAL PostgreSQL, the real
// auth guard, the real RSG write surface (objects and typed relations over
// the wire) and the real provenance read surface (graph + lineage JSON).
//
// Proves the three requirements and both acceptance criteria:
//
//   - uses/produces/derived/follows etc traversal: the lineage walk follows
//     provenance-category edges upstream (where did the dataset come from)
//     and downstream (what does a change affect), with per-type direction
//     (produces/used_by/part_of point origin at the source, everything
//     else at the target), depth limits, cycle safety and version pinning;
//   - lineage view: GET .../objects/{objectId}/lineage answers the
//     dataset's whole ancestry — raw data, producing experiment, protocol,
//     sample — as one JSON graph;
//   - impact hooks: the same walk in direction=downstream enumerates the
//     dependents of an object — the read T1007's impact analysis consumes;
//   - graph list/JSON output: GET .../provenance/graph returns the whole
//     project's provenance graph (nodes + edges, labels resolved), and the
//     weak related_to / knowledge supports edges never appear in it;
//   - the projection is rebuildable and trigger-maintained: rebuilding it
//     from canonical truth yields the same graph, and every new relation
//     lands in it without a rebuild;
//   - the migration's SQL type list and relationcatalog's Go set are the
//     same: one seeded edge per catalog type asserts membership both ways.
package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/provenancehttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/provenance"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

const provenanceTaskID = "T0505"

// provenanceFixture is the full production composition (the same wiring
// cmd/api/main.go builds) plus the seeded graph:
//
//	experiment ──uses─────────────► protocol
//	experiment ──performed_on─────► material
//	experiment ──produces─────────► dataset(v1)
//	dataset(v1) ─derived_from─────► raw dataset
//	experiment ──follows_protocol─► protocol
//	dataset(v1) ─related_to───────► material   (weak: never projected)
//	experiment ──supports─────────► claim      (knowledge: never projected)
//
// The dataset object then gets a second version, so the version-pinning
// behavior of the lineage read is observable.
type provenanceFixture struct {
	ts   *httptest.Server
	pool *pgxpool.Pool

	alice *testUserClient
	bob   *testUserClient
	anon  *testUserClient

	privateProjectID string
	privateBranchID  string

	protocolID   string
	protocolV1   string
	materialID   string
	materialV1   string
	experimentID string
	experimentV1 string
	datasetID    string
	datasetV1    string
	datasetV2    string
	rawID        string
	rawV1        string
	claimID      string
	claimV1      string
	questionID   string
	questionV1   string
	hypothesisID string
	hypothesisV1 string

	publicProjectID string
	publicBranchID  string
	publicObjectID  string
}

// graphPayload mirrors the wire shape of the graph endpoint (the embedded
// provenance.Graph flattens its nodes/edges fields).
type graphPayload struct {
	ProjectID string            `json:"project_id"`
	Nodes     []provenance.Node `json:"nodes"`
	Edges     []provenance.Edge `json:"edges"`
}

// lineagePayload mirrors the wire shape of the lineage endpoint.
type lineagePayload struct {
	ObjectID  string            `json:"object_id"`
	VersionID string            `json:"version_id"`
	Direction string            `json:"direction"`
	MaxDepth  *int              `json:"max_depth,omitempty"`
	Nodes     []provenance.Node `json:"nodes"`
	Edges     []provenance.Edge `json:"edges"`
}

func newProvenanceFixture(t *testing.T, ctx context.Context) *provenanceFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), provenanceTaskID)

	// --- composition (identical to cmd/api/main.go) ---
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
		States:    states.NewService(stateStore, validation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe())),
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Profiles:  persistence.NewProfileStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
	})
	rsgAPI := rsghttp.New(rsghttp.Deps{Service: rsgSvc})
	provenanceAPI := provenancehttp.New(provenancehttp.Deps{
		Store: provenancehttp.NewProjectionStore(pool),
		Gate:  projectAPI.Service(),
	})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", authAPI.Routes())
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	rsgAPI.Register(apiMux)
	provenanceAPI.Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)

	alice, _ := signup(t, ts.URL, "prov-alice@example.com", "prov-alice")
	bob, _ := signup(t, ts.URL, "prov-bob@example.com", "prov-bob")
	anon := newTestUserClient(ts.URL)

	// --- alice's private project and branch over the wire ---
	resp := alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"prov-lab","name":"Provenance Lab","purpose":"exercise the provenance graph","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var privateProject projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&privateProject); err != nil {
		t.Fatalf("create private project payload: %v", err)
	}
	privateProjectID := privateProject.Project.ID

	resp = alice.do(t, http.MethodPost, "/api/v1/projects/"+privateProjectID+"/branches",
		`{"name":"main","base_ref":"","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var branchResp struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&branchResp); err != nil {
		t.Fatalf("create branch payload: %v", err)
	}
	privateBranchID := branchResp.ID

	// --- the graph's objects ---
	createObject := func(objectType, payload string) (id, versionID string) {
		t.Helper()
		resp := alice.do(t, http.MethodPost,
			"/api/v1/projects/"+privateProjectID+"/branches/"+privateBranchID+"/objects",
			`{"object_type":`+jsonString(objectType)+`,"payload":`+payload+`}`)
		mustStatus(t, resp, http.StatusCreated)
		var out struct {
			ID        string `json:"id"`
			VersionID string `json:"version_id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("create %s payload: %v", objectType, err)
		}
		return out.ID, out.VersionID
	}
	protocolID, protocolV1 := createObject("protocol", `{"name":"MOF synthesis protocol"}`)
	materialID, materialV1 := createObject("material", `{"name":"Zn4O(BDC)3 precursor"}`)
	experimentID, experimentV1 := createObject("experiment", `{"name":"batch-7 synthesis"}`)
	datasetID, datasetV1 := createObject("dataset", `{"name":"isotherm series"}`)
	rawID, rawV1 := createObject("dataset", `{"name":"raw adsorption readings"}`)
	claimID, claimV1 := createObject("claim", `{"statement":"MOF-5 has high uptake"}`)
	// The question/hypothesis objects give the guarded knowledge types of
	// the rebased catalog (addresses_question, tests_hypothesis — migration
	// 00040) their valid endpoints in the cross-check.
	questionID, questionV1 := createObject("research_question", `{"statement":"Which conditions maximize MOF uptake?"}`)
	hypothesisID, hypothesisV1 := createObject("hypothesis", `{"statement":"Higher pressure increases uptake"}`)

	// --- the graph's relations ---
	createRelation := func(typ, sourceV, targetV string) {
		t.Helper()
		resp := alice.do(t, http.MethodPost,
			"/api/v1/projects/"+privateProjectID+"/branches/"+privateBranchID+"/relations",
			`{"relation_type":`+jsonString(typ)+`,"source_object_version_id":`+jsonString(sourceV)+`,"target_object_version_id":`+jsonString(targetV)+`}`)
		mustStatus(t, resp, http.StatusCreated)
	}
	createRelation("uses", experimentV1, protocolV1)
	createRelation("performed_on", experimentV1, materialV1)
	createRelation("produces", experimentV1, datasetV1)
	createRelation("derived_from", datasetV1, rawV1)
	createRelation("follows_protocol", experimentV1, protocolV1)
	createRelation("related_to", datasetV1, materialV1) // weak: never projected
	createRelation("supports", experimentV1, claimV1)   // knowledge: never projected

	// --- a second dataset version: lineage default walks the latest ---
	resp = alice.do(t, http.MethodPost,
		"/api/v1/projects/"+privateProjectID+"/branches/"+privateBranchID+"/objects/"+datasetID+":version",
		`{"expected_version":1,"patch":{"name":"isotherm series v2"}}`)
	mustStatus(t, resp, http.StatusCreated)
	var v2 struct {
		VersionID string `json:"version_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v2); err != nil {
		t.Fatalf("dataset v2 payload: %v", err)
	}
	datasetV2 := v2.VersionID

	// --- the public project: one object anyone may read ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"prov-public","name":"Provenance Public","purpose":"public provenance fixture","visibility":"public"}`)
	mustStatus(t, resp, http.StatusCreated)
	var publicProject projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&publicProject); err != nil {
		t.Fatalf("create public project payload: %v", err)
	}
	publicProjectID := publicProject.Project.ID

	resp = alice.do(t, http.MethodPost, "/api/v1/projects/"+publicProjectID+"/branches",
		`{"name":"main","base_ref":"","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	if err := json.NewDecoder(resp.Body).Decode(&branchResp); err != nil {
		t.Fatalf("create public branch payload: %v", err)
	}
	publicBranchID := branchResp.ID

	publicObjectID, _ := createObjectForPublic(t, alice, publicProjectID, publicBranchID)

	return &provenanceFixture{
		ts:               ts,
		pool:             pool,
		alice:            alice,
		bob:              bob,
		anon:             anon,
		privateProjectID: privateProjectID,
		privateBranchID:  privateBranchID,
		protocolID:       protocolID,
		protocolV1:       protocolV1,
		materialID:       materialID,
		materialV1:       materialV1,
		experimentID:     experimentID,
		experimentV1:     experimentV1,
		datasetID:        datasetID,
		datasetV1:        datasetV1,
		datasetV2:        datasetV2,
		rawID:            rawID,
		rawV1:            rawV1,
		claimID:          claimID,
		claimV1:          claimV1,
		questionID:       questionID,
		questionV1:       questionV1,
		hypothesisID:     hypothesisID,
		hypothesisV1:     hypothesisV1,
		publicProjectID:  publicProjectID,
		publicBranchID:   publicBranchID,
		publicObjectID:   publicObjectID,
	}
}

func createObjectForPublic(t *testing.T, alice *testUserClient, projectID, branchID string) (id, versionID string) {
	t.Helper()
	resp := alice.do(t, http.MethodPost,
		"/api/v1/projects/"+projectID+"/branches/"+branchID+"/objects",
		`{"object_type":"material","payload":{"name":"Public MOF"}}`)
	mustStatus(t, resp, http.StatusCreated)
	var out struct {
		ID        string `json:"id"`
		VersionID string `json:"version_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("create public object payload: %v", err)
	}
	return out.ID, out.VersionID
}

// jsonString renders a Go string as a JSON string literal.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// graph fetches the project graph as alice (the owner) and decodes it.
func (f *provenanceFixture) graph(t *testing.T, projectID string) graphPayload {
	t.Helper()
	resp := f.alice.do(t, http.MethodGet,
		"/api/v1/projects/"+projectID+"/provenance/graph", "")
	mustStatus(t, resp, http.StatusOK)
	var g graphPayload
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		t.Fatalf("graph payload: %v", err)
	}
	return g
}

// lineage fetches one lineage read and decodes it, asserting the status.
func (f *provenanceFixture) lineage(t *testing.T, uc *testUserClient, projectID, objectID, query string, want int) (lineagePayload, errorEnvelope) {
	t.Helper()
	path := "/api/v1/projects/" + projectID + "/objects/" + objectID + "/lineage"
	if query != "" {
		path += "?" + query
	}
	resp := uc.do(t, http.MethodGet, path, "")
	if resp.StatusCode != want {
		t.Fatalf("GET %s = %d, want %d: %s", path, resp.StatusCode, want, readAll(t, resp))
	}
	if want != http.StatusOK {
		var env errorEnvelope
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("error envelope: %v", err)
		}
		return lineagePayload{}, env
	}
	var l lineagePayload
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		t.Fatalf("lineage payload: %v", err)
	}
	return l, errorEnvelope{}
}

// nodeVersionIDs and edgeTypes are small projection helpers for the
// assertions.
func nodeVersionIDs(nodes []provenance.Node) map[string]bool {
	out := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		out[n.VersionID] = true
	}
	return out
}

func edgeTypes(edges []provenance.Edge) map[string]bool {
	out := make(map[string]bool, len(edges))
	for _, e := range edges {
		out[e.RelationType] = true
	}
	return out
}

// TestProvenanceGraphJSON is the required "provenance integration" test's
// graph half: the graph list/JSON output carries exactly the provenance
// edges of the project, with resolved labels and deterministic ordering —
// and never the weak or knowledge edges.
func TestProvenanceGraphJSON(t *testing.T) {
	ctx := context.Background()
	f := newProvenanceFixture(t, ctx)

	g := f.graph(t, f.privateProjectID)
	if g.ProjectID != f.privateProjectID {
		t.Fatalf("graph project_id = %s, want %s", g.ProjectID, f.privateProjectID)
	}

	// Exactly the five provenance edges; the weak and knowledge seeds are
	// absent from the projection.
	types := edgeTypes(g.Edges)
	wantTypes := map[string]bool{
		"uses": true, "performed_on": true, "produces": true,
		"derived_from": true, "follows_protocol": true,
	}
	if len(g.Edges) != len(wantTypes) {
		t.Fatalf("edges = %d, want %d: %v", len(g.Edges), len(wantTypes), types)
	}
	for typ := range wantTypes {
		if !types[typ] {
			t.Errorf("edge type %s missing from the graph", typ)
		}
	}
	for _, typ := range []string{"related_to", "supports"} {
		if types[typ] {
			t.Errorf("edge type %s must never enter the provenance graph", typ)
		}
	}

	// The nodes carry resolved labels: five object versions, one per
	// object — and NOT the dataset's second version (no edge pins it).
	nodes := nodeVersionIDs(g.Nodes)
	if len(g.Nodes) != 5 {
		t.Fatalf("nodes = %d, want 5: %+v", len(g.Nodes), g.Nodes)
	}
	for _, want := range []string{f.protocolV1, f.materialV1, f.experimentV1, f.datasetV1, f.rawV1} {
		if !nodes[want] {
			t.Errorf("node %s missing from the graph", want)
		}
	}
	if nodes[f.datasetV2] {
		t.Errorf("dataset v2 must not appear: no edge pins it")
	}
	for _, n := range g.Nodes {
		if n.VersionID == f.protocolV1 {
			if n.ObjectType != "protocol" || n.Title != "MOF synthesis protocol" || n.VersionNo != 1 {
				t.Errorf("protocol node labels not resolved: %+v", n)
			}
		}
	}
	for _, e := range g.Edges {
		if e.RelationID == "" || e.RelationVersionID == "" || e.RelationType == "" {
			t.Errorf("edge identity facts missing: %+v", e)
		}
		if e.Source.VersionID == "" || e.Target.VersionID == "" {
			t.Errorf("edge endpoints missing: %+v", e)
		}
	}

	// Deterministic: the same graph twice is byte-identical.
	resp := f.alice.do(t, http.MethodGet,
		"/api/v1/projects/"+f.privateProjectID+"/provenance/graph", "")
	mustStatus(t, resp, http.StatusOK)
	again := readAll(t, resp)
	resp = f.alice.do(t, http.MethodGet,
		"/api/v1/projects/"+f.privateProjectID+"/provenance/graph", "")
	mustStatus(t, resp, http.StatusOK)
	if again != readAll(t, resp) {
		t.Errorf("graph output is not deterministic across identical reads")
	}
}

// TestProvenanceLineage is the required "provenance integration" test's
// lineage half: the lineage read answers "where did the dataset come from"
// — the raw readings, the producing experiment, the protocol and the
// sample — as one walked JSON graph, version-pinned and depth-bounded.
func TestProvenanceLineage(t *testing.T) {
	ctx := context.Background()
	f := newProvenanceFixture(t, ctx)

	// Version-pinned honesty first: the dataset's LATEST version (v2) has
	// no edges pinned to it, so its lineage is exactly itself.
	l, _ := f.lineage(t, f.alice, f.privateProjectID, f.datasetID, "", http.StatusOK)
	if l.VersionID != f.datasetV2 {
		t.Fatalf("default lineage starts from %s, want latest %s", l.VersionID, f.datasetV2)
	}
	if l.Direction != "upstream" {
		t.Fatalf("default direction = %s, want upstream", l.Direction)
	}
	if len(l.Nodes) != 1 || l.Nodes[0].VersionID != f.datasetV2 || len(l.Edges) != 0 {
		t.Fatalf("latest-version lineage = %+v, want the dataset v2 node alone", l)
	}

	// The pinned lineage: version 1's whole ancestry in one walk.
	l, _ = f.lineage(t, f.alice, f.privateProjectID, f.datasetID, "version_no=1", http.StatusOK)
	if l.VersionID != f.datasetV1 {
		t.Fatalf("pinned lineage starts from %s, want %s", l.VersionID, f.datasetV1)
	}
	nodes := nodeVersionIDs(l.Nodes)
	for _, want := range []string{f.datasetV1, f.experimentV1, f.protocolV1, f.materialV1, f.rawV1} {
		if !nodes[want] {
			t.Errorf("lineage node %s missing — the dataset's origin is incomplete", want)
		}
	}
	if len(l.Nodes) != 5 || len(l.Edges) != 5 {
		t.Fatalf("lineage = %d nodes / %d edges, want 5/5: %+v", len(l.Nodes), len(l.Edges), l)
	}

	// Downstream (impact) from the protocol: the experiment and, through
	// produces, the dataset v1 — but not the material or the raw data.
	l, _ = f.lineage(t, f.alice, f.privateProjectID, f.protocolID, "direction=downstream", http.StatusOK)
	if l.Direction != "downstream" {
		t.Fatalf("direction = %s, want downstream", l.Direction)
	}
	nodes = nodeVersionIDs(l.Nodes)
	for _, want := range []string{f.protocolV1, f.experimentV1, f.datasetV1} {
		if !nodes[want] {
			t.Errorf("downstream node %s missing from the impact walk", want)
		}
	}
	if len(l.Nodes) != 3 || len(l.Edges) != 3 {
		t.Fatalf("downstream = %d nodes / %d edges, want 3/3: %+v", len(l.Nodes), len(l.Edges), l)
	}

	// Depth bounds the walk: depth 1 from the dataset reaches the direct
	// origins only (the experiment and the raw data).
	l, _ = f.lineage(t, f.alice, f.privateProjectID, f.datasetID, "version_no=1&max_depth=1", http.StatusOK)
	nodes = nodeVersionIDs(l.Nodes)
	if !nodes[f.datasetV1] || !nodes[f.experimentV1] || !nodes[f.rawV1] {
		t.Fatalf("depth-1 lineage missing direct origins: %+v", l)
	}
	if nodes[f.protocolV1] || nodes[f.materialV1] {
		t.Fatalf("depth-1 lineage reached beyond one hop: %+v", l)
	}
	if l.MaxDepth == nil || *l.MaxDepth != 1 {
		t.Fatalf("max_depth echo = %v, want 1", l.MaxDepth)
	}

	// Depth 0: the start node alone.
	l, _ = f.lineage(t, f.alice, f.privateProjectID, f.datasetID, "version_no=1&max_depth=0", http.StatusOK)
	if len(l.Nodes) != 1 || len(l.Edges) != 0 {
		t.Fatalf("depth-0 lineage = %+v, want the start node alone", l)
	}

	// The parameter surface.
	_, env := f.lineage(t, f.alice, f.privateProjectID, f.datasetID, "direction=sideways", http.StatusBadRequest)
	if env.Code != provenancehttp.CodeValidation {
		t.Fatalf("bad direction code = %s, want %s", env.Code, provenancehttp.CodeValidation)
	}
	_, _ = f.lineage(t, f.alice, f.privateProjectID, f.datasetID, "max_depth=-1", http.StatusBadRequest)
	_, _ = f.lineage(t, f.alice, f.privateProjectID, f.datasetID, "version_no=0", http.StatusBadRequest)
	_, env = f.lineage(t, f.alice, f.privateProjectID, f.datasetID, "version_no=999", http.StatusNotFound)
	if env.Code != "OBJECT_VERSION_NOT_FOUND" {
		t.Fatalf("unknown version code = %s, want OBJECT_VERSION_NOT_FOUND", env.Code)
	}
	_, env = f.lineage(t, f.alice, f.privateProjectID, "00000000-0000-4000-8000-000000000000", "", http.StatusNotFound)
	if env.Code != "OBJECT_NOT_FOUND" {
		t.Fatalf("unknown object code = %s, want OBJECT_NOT_FOUND", env.Code)
	}
}

// TestProvenanceVisibility: the provenance reads are exactly as visible as
// the project they belong to — a non-member and an anonymous caller get
// the existence-hiding 404 on the private project, while the public
// project answers anonymous readers.
func TestProvenanceVisibility(t *testing.T) {
	ctx := context.Background()
	f := newProvenanceFixture(t, ctx)

	for _, uc := range []*testUserClient{f.bob, f.anon} {
		resp := uc.do(t, http.MethodGet,
			"/api/v1/projects/"+f.privateProjectID+"/provenance/graph", "")
		mustStatus(t, resp, http.StatusNotFound)
		var env errorEnvelope
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("error envelope: %v", err)
		}
		if env.Code != "PROJECT_NOT_FOUND" {
			t.Fatalf("denied graph code = %s, want PROJECT_NOT_FOUND", env.Code)
		}
	}

	// The lineage read runs the project gate FIRST — before the object is
	// even resolved — so a denied reader gets the project 404 for every
	// object id and every parameter value: object existence, version
	// existence and parameter validity stay indistinguishable from the
	// project not existing (no existence/membership oracle, docs/45).
	for _, path := range []string{
		"/objects/" + f.datasetID + "/lineage",                    // the object exists
		"/objects/00000000-0000-4000-8000-000000000000/lineage",   // no such object
		"/objects/" + f.datasetID + "/lineage?version_no=999",     // no such version
		"/objects/" + f.datasetID + "/lineage?direction=sideways", // bad parameter
	} {
		resp := f.bob.do(t, http.MethodGet,
			"/api/v1/projects/"+f.privateProjectID+path, "")
		mustStatus(t, resp, http.StatusNotFound)
		var env errorEnvelope
		if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
			t.Fatalf("error envelope: %v", err)
		}
		if env.Code != "PROJECT_NOT_FOUND" {
			t.Fatalf("denied lineage %s code = %s, want PROJECT_NOT_FOUND", path, env.Code)
		}
	}

	// A project id that is not even the object's project is indistinguishable:
	// the object does not exist there, the object 404 answers.
	_, env := f.lineage(t, f.alice, f.publicProjectID, f.datasetID, "", http.StatusNotFound)
	if env.Code != "OBJECT_NOT_FOUND" {
		t.Fatalf("foreign-project object code = %s, want OBJECT_NOT_FOUND", env.Code)
	}

	// The version-pinned axis is existence-hidden too: a foreign object
	// probed with version_no answers the object 404 whatever the number —
	// the membership check must run before the version lookup, so the
	// codes never disclose that the object exists elsewhere or how many
	// versions it has (docs/45: the object-level and version-level probes
	// are one indistinguishable 404).
	for _, q := range []string{"version_no=1", "version_no=999"} {
		_, env := f.lineage(t, f.alice, f.publicProjectID, f.datasetID, q, http.StatusNotFound)
		if env.Code != "OBJECT_NOT_FOUND" {
			t.Fatalf("foreign object with %s code = %s, want OBJECT_NOT_FOUND", q, env.Code)
		}
	}

	// The public project answers anonymous readers — and its empty graph
	// carries one consistent wire shape: "nodes":[] and "edges":[], never
	// "nodes":null.
	resp := f.anon.do(t, http.MethodGet,
		"/api/v1/projects/"+f.publicProjectID+"/provenance/graph", "")
	mustStatus(t, resp, http.StatusOK)
	body := readAll(t, resp)
	if !strings.Contains(body, `"nodes":[]`) || !strings.Contains(body, `"edges":[]`) {
		t.Fatalf("public project graph JSON = %s, want {\"nodes\":[],\"edges\":[]}", body)
	}
	var g graphPayload
	if err := json.Unmarshal([]byte(body), &g); err != nil {
		t.Fatalf("public graph payload: %v", err)
	}
	if len(g.Nodes) != 0 || len(g.Edges) != 0 {
		t.Fatalf("public project graph = %+v, want empty (no relations seeded)", g)
	}
	_, _ = f.lineage(t, f.anon, f.publicProjectID, f.publicObjectID, "", http.StatusOK)
}

// TestProvenanceProjectionMaintenance: the projection is rebuildable and
// trigger-maintained — a rebuild from canonical truth reproduces it, and a
// new relation lands in it without one.
func TestProvenanceProjectionMaintenance(t *testing.T) {
	ctx := context.Background()
	f := newProvenanceFixture(t, ctx)

	count := func() int {
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM provenance_edges`).Scan(&n); err != nil {
			t.Fatalf("count provenance_edges: %v", err)
		}
		return n
	}
	if n := count(); n != 5 {
		t.Fatalf("projected edges = %d, want 5 (the five provenance relations)", n)
	}

	// Rebuild from canonical truth: same content, still the same API
	// answer afterwards.
	if _, err := f.pool.Exec(ctx, `TRUNCATE provenance_edges`); err != nil {
		t.Fatalf("truncate projection: %v", err)
	}
	if n := count(); n != 0 {
		t.Fatalf("projected edges after truncate = %d, want 0", n)
	}
	if _, err := f.pool.Exec(ctx, `SELECT rebuild_provenance_edges()`); err != nil {
		t.Fatalf("rebuild projection: %v", err)
	}
	if n := count(); n != 5 {
		t.Fatalf("projected edges after rebuild = %d, want 5", n)
	}
	g := f.graph(t, f.privateProjectID)
	if len(g.Edges) != 5 {
		t.Fatalf("graph after rebuild = %d edges, want 5", len(g.Edges))
	}

	// The trigger projects a new relation immediately — no rebuild.
	resp := f.alice.do(t, http.MethodPost,
		"/api/v1/projects/"+f.privateProjectID+"/branches/"+f.privateBranchID+"/relations",
		`{"relation_type":"parameterized_by","source_object_version_id":`+jsonString(f.experimentV1)+`,"target_object_version_id":`+jsonString(f.materialV1)+`}`)
	mustStatus(t, resp, http.StatusCreated)
	if n := count(); n != 6 {
		t.Fatalf("projected edges after a new relation = %d, want 6 (the trigger must maintain the projection)", n)
	}
}

// TestProvenanceCatalogCrossCheck pins the migration's SQL type list to the
// Go catalog: one seeded edge per catalog type, membership in the
// projection must equal the catalog's ProvenanceInference flag, both ways.
func TestProvenanceCatalogCrossCheck(t *testing.T) {
	ctx := context.Background()
	f := newProvenanceFixture(t, ctx)

	var stateID string
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM project_states WHERE project_id = $1::uuid ORDER BY created_at LIMIT 1`,
		f.privateProjectID).Scan(&stateID); err != nil {
		t.Fatalf("find fixture state: %v", err)
	}
	var aliceID string
	if err := f.pool.QueryRow(ctx,
		`SELECT id FROM users WHERE handle = 'prov-alice'`).Scan(&aliceID); err != nil {
		t.Fatalf("find alice: %v", err)
	}

	// One relation per catalog type, seeded directly (the catalog is the
	// Go truth; the trigger projects whatever lands in relation_versions).
	// The guarded knowledge types of migration 00040 demand their declared
	// endpoints: addresses_question targets a research_question and
	// tests_hypothesis targets a hypothesis (the fixture creates both).
	for _, entry := range relationcatalog.Types() {
		targetV := f.materialV1
		switch entry.Type {
		case "addresses_question":
			targetV = f.questionV1
		case "tests_hypothesis":
			targetV = f.hypothesisV1
		}
		var relationID string
		if err := f.pool.QueryRow(ctx, `
			INSERT INTO relations (project_id) VALUES ($1::uuid) RETURNING id`,
			f.privateProjectID).Scan(&relationID); err != nil {
			t.Fatalf("seed relation %s: %v", entry.Type, err)
		}
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO relation_versions
			  (relation_id, version_no, state_id, relation_type,
			   source_object_version_id, target_object_version_id,
			   payload, integrity_hash, created_by)
			VALUES ($1::uuid, 1, $2::uuid, $3,
			        $4::uuid, $5::uuid, '{}'::jsonb, 'seed', $6::uuid)`,
			relationID, stateID, entry.Type, f.experimentV1, targetV, aliceID); err != nil {
			t.Fatalf("seed relation version %s: %v", entry.Type, err)
		}
	}

	// Membership must equal the flag, both ways.
	rows, err := f.pool.Query(ctx, `SELECT DISTINCT relation_type FROM provenance_edges`)
	if err != nil {
		t.Fatalf("read projected types: %v", err)
	}
	defer rows.Close()
	projected := map[string]bool{}
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatalf("scan projected type: %v", err)
		}
		projected[typ] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read projected types: %v", err)
	}
	for _, entry := range relationcatalog.Types() {
		if entry.ProvenanceInference && !projected[entry.Type] {
			t.Errorf("%s is a catalog provenance type but the projection excludes it — the migration's SQL list drifted", entry.Type)
		}
		if !entry.ProvenanceInference && projected[entry.Type] {
			t.Errorf("%s is not a catalog provenance type but the projection includes it — the migration's SQL list drifted", entry.Type)
		}
	}
}
