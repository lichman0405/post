package rsg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
)

// Query unit tests (T0209): selection rules, slice resolution, input
// validation and the per-hop authorization — over fakes that record what
// the service asked for, so the no-leak claims are provable (the denial
// precedes every query-port read, and every hop carries its project
// checks). The SQL shapes themselves (as-of selection, lineage walk,
// adjacency) are covered by tests/integration/rsg_query_test.go over a
// real PostgreSQL.

// queryFakeProjects is a ProjectGate whose per-project read outcomes are
// canned, and which records every Get — the calls list is the evidence
// that each traversal hop carried its own project check.
type queryFakeProjects struct {
	outcomes map[string]error // nil = readable
	project  domain.Project   // returned when set; zero = the bare default
	calls    []string
}

func (f *queryFakeProjects) Get(_ context.Context, _ projects.Reader, projectID string) (domain.Project, error) {
	f.calls = append(f.calls, projectID)
	if err, ok := f.outcomes[projectID]; ok {
		return domain.Project{}, err
	}
	if f.project.ID != "" {
		return f.project, nil
	}
	return domain.Project{ID: projectID, Visibility: domain.VisibilityPublic}, nil
}

func (f *queryFakeProjects) GetMembership(_ context.Context, _ domain.User, _ string) (domain.ProjectMembership, error) {
	return domain.ProjectMembership{}, projects.ErrMemberNotFound
}

// queryFakePort is a QueryPort over canned rows. The as-of selection is
// not modelled (the fake returns what it is given); it records the
// lineage and the version-id batches the service passed, which is what
// the unit claims are about.
type queryFakePort struct {
	objects       []ObjectQueryRow
	relations     []RelationQueryRow
	adjacent      map[string][]AdjacentRelationRow
	byIDs         map[string]ObjectQueryRow
	lineageByKey  map[string][]string
	lineageCalls  int
	objectCalls   int
	adjacentCalls int
	byIDsArg      []string
	lastObjectArg []string
	err           error
	// byIDsErr fails only the version-id batch fetch (ListObjectVersionsByIDs),
	// leaving the list reads healthy — the claim-ref fetch failure path.
	byIDsErr error
}

func (f *queryFakePort) ListStateLineage(_ context.Context, projectID, stateID string) ([]string, error) {
	f.lineageCalls++
	return f.lineageByKey[projectID+"|"+stateID], f.err
}

func (f *queryFakePort) ListObjectVersions(_ context.Context, _ string, objectTypes, lineage []string) ([]ObjectQueryRow, error) {
	f.objectCalls++
	f.lastObjectArg = objectTypes
	if f.err != nil {
		return nil, f.err
	}
	if len(objectTypes) == 0 {
		return f.objects, nil
	}
	var out []ObjectQueryRow
	for _, row := range f.objects {
		for _, t := range objectTypes {
			if row.Object.ObjectType == t {
				out = append(out, row)
				break
			}
		}
	}
	return out, nil
}

func (f *queryFakePort) ListRelationVersions(_ context.Context, _ string, _ []string, _ []string) ([]RelationQueryRow, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.relations, nil
}

func (f *queryFakePort) ListAdjacentRelationVersions(_ context.Context, versionIDs, _ []string) ([]AdjacentRelationRow, error) {
	f.adjacentCalls++
	if f.err != nil {
		return nil, f.err
	}
	seen := map[string]bool{}
	var out []AdjacentRelationRow
	for _, vid := range versionIDs {
		for _, row := range f.adjacent[vid] {
			if !seen[row.Version.ID] {
				seen[row.Version.ID] = true
				out = append(out, row)
			}
		}
	}
	return out, nil
}

func (f *queryFakePort) ListObjectVersionsByIDs(_ context.Context, versionIDs []string) ([]ObjectQueryRow, error) {
	f.byIDsArg = append(f.byIDsArg, versionIDs...)
	if f.err != nil {
		return nil, f.err
	}
	if f.byIDsErr != nil {
		return nil, f.byIDsErr
	}
	var out []ObjectQueryRow
	for _, vid := range versionIDs {
		if row, ok := f.byIDs[vid]; ok {
			out = append(out, row)
		}
	}
	return out, nil
}

// newQueryService wires a service over the two query fakes. The write
// ports are unwired — Query never touches them.
func newQueryService(projectsGate *queryFakeProjects, port *queryFakePort) *Service {
	return &Service{projects: projectsGate, queries: port}
}

// queryObject builds one canned object row (object + its as-of version).
func queryObject(id, projectID, objectType, versionID string) ObjectQueryRow {
	return ObjectQueryRow{
		Object: domain.ScientificObject{ID: id, ProjectID: projectID, ObjectType: objectType, CurrentVersionNo: 1, CreatedAt: time.Now()},
		Version: domain.ScientificObjectVersion{
			ID: versionID, ObjectID: id, VersionNo: 1, StateID: "state-x",
			Title: objectType, LifecycleState: domain.LifecycleActive, CreatedAt: time.Now(),
		},
	}
}

// queryEndpoint builds one endpoint context for canned relations: the
// ordinary pin, whose version was written in the project that owns the
// container it hangs on, so its container project and its carrier are the same
// project. A landed pin (container = contributor, carrier = acceptor) is built
// explicitly by the tests that are about that difference.
func queryEndpoint(versionID, objectID, objectType, projectID string) EndpointContext {
	return EndpointContext{
		VersionID: versionID, ObjectID: objectID, ObjectType: objectType,
		ProjectID: projectID, CarriedBy: projectID,
	}
}

// queryCarriedEndpoint builds an endpoint context whose container and carrier
// differ: an object of containerProject wearing a version the states of
// carriedBy hold — the shape a merge lands (ADR-027 Decision 1).
func queryCarriedEndpoint(versionID, objectID, objectType, containerProject, carriedBy string) EndpointContext {
	e := queryEndpoint(versionID, objectID, objectType, containerProject)
	e.CarriedBy = carriedBy
	return e
}

// queryRelation builds one canned relation row (relation + its as-of
// version + endpoint contexts).
func queryRelation(id, projectID, relationType, sourceVersion, targetVersion, sourceObject, targetObject string, endpoints ...EndpointContext) RelationQueryRow {
	src := queryEndpoint(sourceVersion, sourceObject, "material", projectID)
	tgt := queryEndpoint(targetVersion, targetObject, "finding", projectID)
	if len(endpoints) > 0 {
		src = endpoints[0]
	}
	if len(endpoints) > 1 {
		tgt = endpoints[1]
	}
	return RelationQueryRow{
		Relation: domain.Relation{ID: id, ProjectID: projectID, CreatedAt: time.Now()},
		Version: domain.RelationVersion{
			ID: id + "-v1", RelationID: id, VersionNo: 1, StateID: "state-x",
			RelationType: relationType, SourceObjectVersionID: sourceVersion, TargetObjectVersionID: targetVersion,
			CreatedAt: time.Now(),
		},
		Source: src,
		Target: tgt,
	}
}

const (
	qP     = "project-1"
	qP2    = "project-2"
	matID  = "material-1"
	matVID = "material-1-v1"
	qID    = "question-1"
	qVID   = "question-1-v1"
	fID    = "finding-1"
	fVID   = "finding-1-v1"
	eID    = "experiment-1"
	eVID   = "experiment-1-v1"
)

// queryFixture is the standard graph: a material, a research question, a
// finding and an experiment, connected by performed_on (experiment →
// material), addresses_question (finding → question) and refines
// (experiment → finding). All in project-1.
func queryFixture() (*queryFakeProjects, *queryFakePort) {
	gates := &queryFakeProjects{outcomes: map[string]error{}}
	port := &queryFakePort{
		objects: []ObjectQueryRow{
			queryObject(matID, qP, "material", matVID),
			queryObject(qID, qP, "research_question", qVID),
			queryObject(fID, qP, "finding", fVID),
			queryObject(eID, qP, "experiment", eVID),
		},
		relations: []RelationQueryRow{
			queryRelation("r1", qP, "performed_on", eVID, matVID, eID, matID,
				queryEndpoint(eVID, eID, "experiment", qP), queryEndpoint(matVID, matID, "material", qP)),
			queryRelation("r2", qP, "addresses_question", fVID, qVID, fID, qID,
				queryEndpoint(fVID, fID, "finding", qP), queryEndpoint(qVID, qID, "research_question", qP)),
			queryRelation("r3", qP, "refines", eVID, fVID, eID, fID,
				queryEndpoint(eVID, eID, "experiment", qP), queryEndpoint(fVID, fID, "finding", qP)),
		},
		adjacent: map[string][]AdjacentRelationRow{
			// bidirectional: each pinned endpoint sees its edges
			eVID: {
				adjacentRow("r1", qP, "performed_on", eVID, matVID, eID, "experiment", matID, "material"),
				adjacentRow("r3", qP, "refines", eVID, fVID, eID, "experiment", fID, "finding"),
			},
			matVID: {
				adjacentRow("r1", qP, "performed_on", eVID, matVID, eID, "experiment", matID, "material"),
			},
			fVID: {
				adjacentRow("r2", qP, "addresses_question", fVID, qVID, fID, "finding", qID, "research_question"),
				adjacentRow("r3", qP, "refines", eVID, fVID, eID, "experiment", fID, "finding"),
			},
			qVID: {
				adjacentRow("r2", qP, "addresses_question", fVID, qVID, fID, "finding", qID, "research_question"),
			},
		},
		lineageByKey: map[string][]string{},
	}
	// The endpoint/traversal nodes render through the same batch fetch the
	// production store serves: one row per pinned version id.
	port.byIDs = map[string]ObjectQueryRow{}
	for _, row := range port.objects {
		port.byIDs[row.Version.ID] = row
	}
	return gates, port
}

// adjacentRow builds one traversal-hop row (fixture shorthand).
func adjacentRow(relationID, projectID, relationType, sourceVID, targetVID, sourceObj, sourceType, targetObj, targetType string) AdjacentRelationRow {
	return AdjacentRelationRow{
		Relation: domain.Relation{ID: relationID, ProjectID: projectID},
		Version: domain.RelationVersion{
			ID: relationID + "-v1", RelationID: relationID, VersionNo: 1,
			RelationType: relationType, SourceObjectVersionID: sourceVID, TargetObjectVersionID: targetVID,
		},
		Source: queryEndpoint(sourceVID, sourceObj, sourceType, projectID),
		Target: queryEndpoint(targetVID, targetObj, targetType, projectID),
	}
}

func objectIDs(res QueryResult) []string {
	out := make([]string, 0, len(res.Objects))
	for _, o := range res.Objects {
		out = append(out, o.Object.ID)
	}
	return out
}

func relationIDs(res QueryResult) []string {
	out := make([]string, 0, len(res.Relations))
	for _, r := range res.Relations {
		out = append(out, r.Relation.ID)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// TestQueryNoFiltersReturnsWholeSlice: the unfiltered query is the whole
// graph slice — every object and every relation of the project.
func TestQueryNoFiltersReturnsWholeSlice(t *testing.T) {
	gates, port := queryFixture()
	svc := newQueryService(gates, port)
	res, err := svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Objects) != 4 || len(res.Relations) != 3 {
		t.Fatalf("whole slice = 4 objects + 3 relations, got %d + %d", len(res.Objects), len(res.Relations))
	}
	if res.StateID != "" || res.BranchID != "" {
		t.Fatalf("no pin: state_id %q branch_id %q, want empty", res.StateID, res.BranchID)
	}
}

// TestQueryByObjectTypeSelectsInducedEdges: object_type=material selects
// the material only — no induced edge connects two materials — and the
// relation list stays empty at depth 0; depth 1 expands to the performed_on
// edge and its experiment node.
func TestQueryByObjectTypeSelectsInducedEdges(t *testing.T) {
	gates, port := queryFixture()
	svc := newQueryService(gates, port)

	res, err := svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{ObjectTypes: []string{"material"}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Objects) != 1 || res.Objects[0].Object.ID != matID {
		t.Fatalf("material query at depth 0 = [material-1], got %v", objectIDs(res))
	}
	if len(res.Relations) != 0 {
		t.Fatalf("no induced edge between materials, got %v", relationIDs(res))
	}

	res, err = svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{ObjectTypes: []string{"material"}, Depth: 1})
	if err != nil {
		t.Fatalf("query depth 1: %v", err)
	}
	if len(res.Objects) != 2 || !contains(objectIDs(res), eID) {
		t.Fatalf("material query at depth 1 = [material-1 experiment-1], got %v", objectIDs(res))
	}
	if len(res.Relations) != 1 || res.Relations[0].Relation.ID != "r1" {
		t.Fatalf("depth 1 adds the performed_on edge, got %v", relationIDs(res))
	}
}

// TestQueryByRelationTypeReturnsEndpointNodes: relation_type selects the
// matching edges and their pinned endpoint nodes, and nothing else.
func TestQueryByRelationTypeReturnsEndpointNodes(t *testing.T) {
	gates, port := queryFixture()
	svc := newQueryService(gates, port)
	res, err := svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{RelationTypes: []string{"addresses_question"}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Relations) != 1 || res.Relations[0].Relation.ID != "r2" {
		t.Fatalf("relation query = [r2], got %v", relationIDs(res))
	}
	if len(res.Objects) != 2 || !contains(objectIDs(res), fID) || !contains(objectIDs(res), qID) {
		t.Fatalf("relation query nodes = [finding-1 question-1], got %v", objectIDs(res))
	}
}

// TestQueryTraversalDepthIsBounded: a two-hop chain — question → finding →
// experiment — appears hop by hop, and never beyond MaxQueryDepth.
func TestQueryTraversalDepthIsBounded(t *testing.T) {
	gates, port := queryFixture()
	svc := newQueryService(gates, port)

	res, err := svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{ObjectTypes: []string{"research_question"}, Depth: 1})
	if err != nil {
		t.Fatalf("depth 1: %v", err)
	}
	if !contains(objectIDs(res), fID) || contains(objectIDs(res), eID) {
		t.Fatalf("depth 1 reaches the finding but not the experiment, got %v", objectIDs(res))
	}
	if !contains(relationIDs(res), "r2") || contains(relationIDs(res), "r3") {
		t.Fatalf("depth 1 adds addresses_question but not refines, got %v", relationIDs(res))
	}

	res, err = svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{ObjectTypes: []string{"research_question"}, Depth: 2})
	if err != nil {
		t.Fatalf("depth 2: %v", err)
	}
	if !contains(objectIDs(res), eID) || !contains(relationIDs(res), "r3") {
		t.Fatalf("depth 2 reaches the experiment through the finding, got objects=%v relations=%v", objectIDs(res), relationIDs(res))
	}
}

// TestQueryValidation: shape errors refuse before any read, and the type
// lists dedupe.
func TestQueryValidation(t *testing.T) {
	gates, port := queryFixture()
	svc := newQueryService(gates, port)
	reader := projects.Reader{UserID: "u1", Authenticated: true}

	cases := []struct {
		name string
		in   QueryInput
	}{
		{"both pins", QueryInput{StateID: "s1", BranchID: "b1"}},
		{"negative depth", QueryInput{Depth: -1}},
		{"depth beyond max", QueryInput{Depth: MaxQueryDepth + 1}},
		{"bad type token", QueryInput{ObjectTypes: []string{"../etc/passwd"}}},
		{"empty type token", QueryInput{RelationTypes: []string{""}}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.Query(context.Background(), reader, qP, tt.in)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("want ErrValidation, got %v", err)
			}
		})
	}

	// Unknown-but-well-formed types match nothing, they are not errors.
	res, err := svc.Query(context.Background(), reader, qP, QueryInput{ObjectTypes: []string{"no_such_type"}})
	if err != nil {
		t.Fatalf("unknown type: %v", err)
	}
	if len(res.Objects) != 0 {
		t.Fatalf("unknown type must match nothing, got %v", objectIDs(res))
	}
}

// TestQueryEntryGateHidesInvisibleProjects: a caller who may not read the
// project gets projects.ErrProjectNotFound — not forbidden — and the
// query port is never touched (the refusal precedes every read).
func TestQueryEntryGateHidesInvisibleProjects(t *testing.T) {
	gates, port := queryFixture()
	gates.outcomes[qP] = projects.ErrProjectNotFound
	svc := newQueryService(gates, port)
	_, err := svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{})
	if !errors.Is(err, projects.ErrProjectNotFound) {
		t.Fatalf("want projects.ErrProjectNotFound, got %v", err)
	}
	if port.objectCalls != 0 || port.lineageCalls != 0 {
		t.Fatalf("denial must precede every query-port read, saw %d object / %d lineage calls", port.objectCalls, port.lineageCalls)
	}
}

// TestQueryTraversalPrunesInvisibleHops: every hop carries its own
// project checks — a hop into an invisible project (the edge's project or
// the neighbor's) is pruned, and the per-project check really ran (the
// gate's call log names the foreign project). The seed itself stays
// intact.
func TestQueryTraversalPrunesInvisibleHops(t *testing.T) {
	gates, port := queryFixture()
	// A foreign edge touching the material: edge in project-2 (private),
	// neighbor also in project-2. The caller cannot read project-2.
	port.adjacent[matVID] = append(port.adjacent[matVID], AdjacentRelationRow{
		Relation: domain.Relation{ID: "rx", ProjectID: qP2},
		Version:  domain.RelationVersion{ID: "rx-v1", RelationID: "rx", RelationType: "forked_from", SourceObjectVersionID: matVID, TargetObjectVersionID: "p2-v1"},
		Source:   queryEndpoint(matVID, matID, "material", qP),
		Target:   queryEndpoint("p2-v1", "p2-obj", "material", qP2),
	})
	gates.outcomes[qP2] = projects.ErrProjectNotFound
	svc := newQueryService(gates, port)

	res, err := svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{ObjectTypes: []string{"material"}, Depth: 1})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for _, r := range res.Relations {
		if r.Relation.ID == "rx" {
			t.Fatalf("the foreign edge leaked into the slice")
		}
	}
	for _, o := range res.Objects {
		if o.Object.ID == "p2-obj" {
			t.Fatalf("the foreign node leaked into the slice")
		}
	}
	if !contains(gates.calls, qP2) {
		t.Fatalf("the hop must run its own project check for %s, gate calls: %v", qP2, gates.calls)
	}
}

// TestQueryTraversalKeepsVisibleForeignHops: a foreign hop whose projects
// are readable enters the slice — cross-project traversal exists and is
// authorized hop by hop, not by the seed's project alone.
func TestQueryTraversalKeepsVisibleForeignHops(t *testing.T) {
	gates, port := queryFixture()
	port.adjacent[matVID] = append(port.adjacent[matVID], AdjacentRelationRow{
		Relation: domain.Relation{ID: "rx", ProjectID: qP2},
		Version:  domain.RelationVersion{ID: "rx-v1", RelationID: "rx", RelationType: "forked_from", SourceObjectVersionID: matVID, TargetObjectVersionID: "p2-v1"},
		Source:   queryEndpoint(matVID, matID, "material", qP),
		Target:   queryEndpoint("p2-v1", "p2-obj", "material", qP2),
	})
	port.byIDs["p2-v1"] = queryObject("p2-obj", qP2, "material", "p2-v1")
	svc := newQueryService(gates, port)

	res, err := svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{ObjectTypes: []string{"material"}, Depth: 1})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !contains(relationIDs(res), "rx") || !contains(objectIDs(res), "p2-obj") {
		t.Fatalf("the visible foreign hop must enter the slice, got objects=%v relations=%v", objectIDs(res), relationIDs(res))
	}
}

// TestQueryEdgeWithHiddenEndpointIsPruned: a seed edge whose own project is
// visible but whose endpoint is CARRIED by a project the caller cannot read
// must not enter the slice — its payload would disclose the hidden project's
// pinned version id. The pin is authorized by its carrier (query.go's pin
// check), and the hidden project's states are what carry it.
func TestQueryEdgeWithHiddenEndpointIsPruned(t *testing.T) {
	gates, port := queryFixture()
	port.relations = append(port.relations, queryRelation("r4", qP, "forked_from", matVID, "p2-v1", matID, "p2-obj",
		queryEndpoint(matVID, matID, "material", qP), queryEndpoint("p2-v1", "p2-obj", "material", qP2)))
	gates.outcomes[qP2] = projects.ErrProjectNotFound
	svc := newQueryService(gates, port)

	res, err := svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if contains(relationIDs(res), "r4") || contains(objectIDs(res), "p2-obj") {
		t.Fatalf("the edge with a hidden endpoint leaked, got objects=%v relations=%v", objectIDs(res), relationIDs(res))
	}
}

// TestQuerySeedEdgeIsAuthorizedByPinCarrierNotContainer is the rule T0818
// changes, at the service layer, in the direction that used to be broken: a
// seed edge whose relation row AND endpoints wear an invisible project's
// containers still enters the slice when BOTH pins are carried by the reading
// project's own states — that is what a merged external fork's landed content
// is, and the container is reported as it is (ADR-027 Decisions 1-3①).
func TestQuerySeedEdgeIsAuthorizedByPinCarrierNotContainer(t *testing.T) {
	gates, port := queryFixture()
	// The contributor's project qP2 is PRIVATE and unreadable; the edge and
	// both containers are its; both pins are carried by qP's states.
	port.relations = append(port.relations, queryRelation("r5", qP2, "supports", "p2-v1", "p2-v2", "p2-obj", "p2-obj2",
		queryCarriedEndpoint("p2-v1", "p2-obj", "material", qP2, qP),
		queryCarriedEndpoint("p2-v2", "p2-obj2", "material", qP2, qP)))
	port.byIDs["p2-v1"] = queryObject("p2-obj", qP2, "material", "p2-v1")
	port.byIDs["p2-v2"] = queryObject("p2-obj2", qP2, "material", "p2-v2")
	gates.outcomes[qP2] = projects.ErrProjectNotFound
	svc := newQueryService(gates, port)

	res, err := svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if !contains(relationIDs(res), "r5") {
		t.Fatalf("the landed edge was pruned on its containers, got relations=%v objects=%v", relationIDs(res), objectIDs(res))
	}
	// The landed node is reported with the contributor's container identity —
	// the container did not become a gate, and it did not get rewritten
	// either.
	got, ok := findObject(res, "p2-obj")
	if !ok || got.Object.ProjectID != qP2 || got.Version.ID != "p2-v1" {
		t.Fatalf("the landed node must be reported as the contributor's container at the pinned version, got %+v", got)
	}
}

// TestQuerySeedEdgeWithHiddenPinCarrierIsPruned is the negative half of the
// same rule: the CONTAINER being readable is not permission. A pin carried by
// a project this caller cannot read prunes the seed edge even when the edge and
// that endpoint's container are both the caller's own — the enumeration range
// is this project's lineage, never another project's versions (ADR-027
// Decision 4).
func TestQuerySeedEdgeWithHiddenPinCarrierIsPruned(t *testing.T) {
	gates, port := queryFixture()
	// Containers and the relation are all qP's; the source pin's version is
	// carried by qP2's states, which u1 may not read.
	port.relations = append(port.relations, queryRelation("r6", qP, "forked_from", matVID, "p2-v1", matID, "p2-obj",
		queryEndpoint(matVID, matID, "material", qP),
		queryCarriedEndpoint("p2-v1", "p2-obj", "material", qP, qP2)))
	gates.outcomes[qP2] = projects.ErrProjectNotFound
	svc := newQueryService(gates, port)

	res, err := svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if contains(relationIDs(res), "r6") {
		t.Fatalf("an edge pinning a version carried by an unreadable project leaked, got relations=%v", relationIDs(res))
	}
}

// findObject returns the slice row for one object id.
func findObject(res QueryResult, objectID string) (ObjectResult, bool) {
	for _, o := range res.Objects {
		if o.Object.ID == objectID {
			return o, true
		}
	}
	return ObjectResult{}, false
}

// TestQueryHopStoreFailureFailsClosed: a store failure on a traversal hop
// aborts the query with ErrStore — never a silently partial slice.
func TestQueryHopStoreFailureFailsClosed(t *testing.T) {
	gates, port := queryFixture()
	svc := newQueryService(gates, port)
	port.err = errors.New("boom")
	_, err := svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{ObjectTypes: []string{"material"}, Depth: 1})
	if !errors.Is(err, ErrStore) {
		t.Fatalf("want ErrStore, got %v", err)
	}
}

// TestQueryStatePinResolution: an explicit state resolves its lineage and
// echoes the pin; a state that is missing or foreign answers
// states.ErrStateNotFound (the same outcome for both — no existence
// leak).
func TestQueryStatePinResolution(t *testing.T) {
	gates, port := queryFixture()
	port.lineageByKey[qP+"|s1"] = []string{"s1", "s0"}
	svc := newQueryService(gates, port)

	res, err := svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{StateID: "s1"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.StateID != "s1" || res.BranchID != "" {
		t.Fatalf("state pin must echo, got state=%q branch=%q", res.StateID, res.BranchID)
	}
	if len(res.Objects) != 4 {
		t.Fatalf("pinned slice still renders the project's objects, got %d", len(res.Objects))
	}

	// A state the project does not have (missing or foreign — identical).
	_, err = svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{StateID: "foreign-s"})
	if !errors.Is(err, states.ErrStateNotFound) {
		t.Fatalf("want states.ErrStateNotFound, got %v", err)
	}
}

// TestQueryBranchPinResolution: a branch resolves through the branch gate
// and its head state's lineage; a branch of another project answers
// branches.ErrBranchNotFound.
func TestQueryBranchPinResolution(t *testing.T) {
	gates, port := queryFixture()
	branchesGate := &queryFakeBranches{branch: domain.Branch{ID: "b1", ProjectID: qP, Name: "main"}}
	statesGate := &queryFakeStates{head: domain.ProjectState{ID: "s2", ProjectID: qP, BranchID: strPtr("b1")}}
	port.lineageByKey[qP+"|s2"] = []string{"s2", "s1", "s0"}
	svc := &Service{projects: gates, queries: port, branches: branchesGate, states: statesGate}

	res, err := svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{BranchID: "b1"})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if res.StateID != "s2" || res.BranchID != "b1" {
		t.Fatalf("branch pin must echo head state, got state=%q branch=%q", res.StateID, res.BranchID)
	}

	branchesGate.getErr = branches.ErrBranchNotFound
	_, err = svc.Query(context.Background(), projects.Reader{UserID: "u1", Authenticated: true}, qP, QueryInput{BranchID: "b9"})
	if !errors.Is(err, branches.ErrBranchNotFound) {
		t.Fatalf("foreign branch must answer the branch-not-found outcome, got %v", err)
	}
}

// queryFakeBranches / queryFakeStates are the minimal BranchPort /
// StatePort slices the query resolution needs.
type queryFakeBranches struct {
	branch domain.Branch
	getErr error
}

func (f *queryFakeBranches) Create(_ context.Context, _ branches.CreateBranchParams) (domain.Branch, error) {
	return domain.Branch{}, errors.New("unused")
}

func (f *queryFakeBranches) Get(_ context.Context, _, _ string) (domain.Branch, error) {
	if f.getErr != nil {
		return domain.Branch{}, f.getErr
	}
	return f.branch, nil
}

func (f *queryFakeBranches) List(_ context.Context, _ string) ([]domain.Branch, error) {
	return nil, errors.New("unused")
}

type queryFakeStates struct {
	head domain.ProjectState
}

func (f *queryFakeStates) Commit(_ context.Context, _ states.CommitParams, _ states.WriteFunc) (domain.ProjectState, domain.StateCommit, error) {
	return domain.ProjectState{}, domain.StateCommit{}, errors.New("unused")
}

func (f *queryFakeStates) CreateInitialState(_ context.Context, _ states.CreateInitialStateParams) (domain.ProjectState, error) {
	return domain.ProjectState{}, errors.New("unused")
}

func (f *queryFakeStates) GetBranchHead(_ context.Context, _ string) (domain.ProjectState, error) {
	return f.head, nil
}

func (f *queryFakeStates) ListCommits(_ context.Context, _ string) ([]domain.StateCommit, error) {
	return nil, errors.New("unused")
}

func strPtr(s string) *string { return &s }
