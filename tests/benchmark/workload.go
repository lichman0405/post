package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	appvalidation "github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/embedding"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// Workload is the application, wired, over the benchmark corpus.
//
// # Why the measurement is taken HERE and not over HTTP
//
// docs/27_PERFORMANCE_SLO.md states its budgets on two different objects. The
// 页面 section says "p95 API" and the 写入 section says "semantic command";
// both name an operation, and the HTTP layer in front of it (chi-style routing,
// the auth guard, JSON encode/decode, middleware) is a real cost that this
// harness does not measure. What it measures is the layer below: the
// application service each route calls, running the real sqlc queries against
// real PostgreSQL through the real store adapters.
//
// The boundary is drawn there for one reason: the whole of this task's subject
// matter — which index answers which query, how many rows a read touches, how
// the plan changes with capacity — is decided at the database. A p95 that
// includes an HTTP round trip and a JSON encode would move for reasons this
// package cannot see and cannot tune, and the numbers the index decisions rest
// on would be the ones with the most noise in them. Every number in the report
// is therefore labelled with what it times, and the report prints the boundary
// sentence next to the table rather than burying it here.
//
// The wiring below is the same composition cmd/api/main.go performs and
// tests/integration/research_page_test.go reuses, minus the HTTP handlers:
// projectshttp.New(...).Service() is the exact *projects.Service the projects
// route serves from, and rsg.NewService(...) the exact *rsg.Service the RSG
// routes serve from. Nothing here is a benchmark-only reimplementation of an
// application operation, because a benchmark of a reimplementation is a
// benchmark of the reimplementation.
type Workload struct {
	Corpus *Corpus

	// Pool is the measurement pool. It has NO vector codec registered — the
	// codec exists only on the seed pool, see openSeed — so a vector parameter
	// travels as pgvector's text syntax exactly as production sends it.
	Pool *pgxpool.Pool

	// RSG and Projects are the application services. Retriever is the
	// retrieval pipeline in its PRODUCTION wiring: NewRetriever(store, nil).
	// The nil embedder is not an omission here — cmd/api/main.go wires it the
	// same way, and the pipeline reports the vector signal as
	// skipped=no_embedder rather than quietly substituting one. The vector
	// half is measured separately, through the store, by vectorScanProbe.
	RSG       *rsg.Service
	Projects  *projects.Service
	Ranking   *ranking.SQLStore
	Retriever *retrieval.Retriever
	Store     *retrieval.SQLStore

	// SearchSQL is the generated query set, used by the two probes that have
	// no adapter method (see probe_deep_offset / probe_ranking_facts).
	SearchSQL *sqlc.Queries

	// Reader is the caller identity every page read runs as: the corpus owner,
	// an authenticated member of every project. Actor is the writer.
	Reader projects.Reader
	Actor  domain.User
	Scope  search.Scope

	// Embedding is the query vector in pgvector's text syntax, produced by the
	// same deterministic embedder that produced the corpus's stored vectors.
	Embedding string

	tracer *captureTracer

	// benchURL is kept so a cold sample can open its own connection.
	benchURL string

	writeVersion int
	writeMu      sync.Mutex
}

// measurementSettings are the session settings every measured connection is
// opened with, and they are part of the report because they are part of the
// measurement's meaning.
//
//	max_parallel_workers_per_gather = 0
//
// Parallel query is OFF. This is not a tuning choice — it is the only way the
// spec tier runs at all on the development stack this task was given, and the
// reason is a property of the HOST, not of any query:
//
// At the spec tier the outline's relation read reaches a plan whose parallel
// hash join asks PostgreSQL to resize a dynamic shared memory segment to 16 MB,
// and that call fails with `ERROR: could not resize shared memory segment
// "...": No space left on device (SQLSTATE 53100)`. The segment lives in
// /dev/shm, because the server runs with `dynamic_shared_memory_type = posix`
// (the default) inside a container whose /dev/shm is far smaller than the
// default; an independent probe on the same database showed a 6.2 MB parallel
// hash join succeeding where the 16 MB resize failed, which brackets the
// container's limit between those two numbers. `dynamic_shared_memory_type` is
// PGC_POSTMASTER and cannot be changed from a session, and this harness has no
// way to restart the server it is pointed at, so the remaining lever that is
// actually available is the one that stops the allocation being requested.
//
// What this costs the report, stated plainly rather than left for a reader to
// discover from a GUC: at the spec tier the numbers below are SERIAL plans, so
// a production server with `max_parallel_workers_per_gather = 2` may run some
// of these reads faster than reported. The direction is conservative — this
// harness cannot report a budget as met that a parallel plan would miss — and
// the plans the gate checks are the serial plans, which is why the gate's
// index-reachability checks are unaffected by the setting: an index either is
// or is not reachable, with or without workers.
//
// The proper fix is on the server (start it with a larger /dev/shm, or
// `dynamic_shared_memory_type = mmap`); it is recorded as a follow-up rather
// than done here, because it is infrastructure outside this task's scope.
var measurementSettings = []string{"max_parallel_workers_per_gather=0"}

// openWorkload wires the application over an already-seeded database.
func openWorkload(ctx context.Context, benchURL string, c *Corpus) (*Workload, error) {
	tracer := &captureTracer{}
	cfg, err := pgxpool.ParseConfig(benchURL)
	if err != nil {
		return nil, fmt.Errorf("benchmark: parse bench url: %w", err)
	}
	cfg.ConnConfig.Tracer = tracer
	// Session settings go in the startup packet, so every connection in the
	// pool — and every connection a cold sample opens — carries them.
	for _, s := range measurementSettings {
		name, value, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("benchmark: malformed session setting %q", s)
		}
		cfg.ConnConfig.RuntimeParams[name] = value
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("benchmark: open measurement pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("benchmark: %s is not reachable: %w", redactURL(benchURL), err)
	}

	reg, err := schemareg.New()
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("benchmark: schemareg.New: %w", err)
	}
	stateStore := persistence.NewStateStore(pool)
	projectSvc := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(pool),
		Orgs:  persistence.NewOrgStore(pool),
		Authz: authz.NewMatrixEngine(),
	}).Service()

	rsgSvc := rsg.NewService(rsg.Deps{
		Projects: projectSvc,
		Branches: branches.NewService(persistence.NewBranchStore(pool)),
		States: states.NewService(stateStore,
			appvalidation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe())),
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Queries:   persistence.NewRSGQueryStore(pool),
		Profiles:  persistence.NewProfileStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})

	store, err := retrieval.NewSQLStore(pool)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("benchmark: build the retrieval store: %w", err)
	}
	// The production wiring, verbatim: cmd/api/main.go builds the retriever
	// with no embedder.
	retriever, err := retrieval.NewRetriever(store, nil)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("benchmark: build the retriever: %w", err)
	}
	rankStore, err := ranking.NewSQLStore(pool)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("benchmark: build the ranking store: %w", err)
	}

	scope, err := search.ResolveScope(ctx, persistence.NewProjectStore(pool), c.OwnerID)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("benchmark: resolve the search scope for the corpus owner: %w", err)
	}
	if !scope.Authenticated() {
		pool.Close()
		return nil, fmt.Errorf("benchmark: the corpus owner resolved to an unauthenticated scope")
	}
	// The owner is a member of every project in the corpus — the hot one
	// included — so the scope covers 1 + NetworkProjects and nothing else.
	if got, want := len(scope.AllowedProjectUUIDs()), 1+c.Scale.NetworkProjects; got != want {
		pool.Close()
		return nil, fmt.Errorf("benchmark: the corpus owner's scope covers %d projects, want %d "+
			"(a scope that does not cover the corpus makes every search read measure nothing)", got, want)
	}

	// ANALYZE before anything is timed. Without statistics the planner works
	// from the default row estimate and picks plans the application would never
	// see, which would make both the timings and the plan checks describe a
	// database nobody runs. It is deliberately not "the thing under test": the
	// gate re-runs it after a fresh load, and the report prints the wall clock
	// it took.
	if _, err := pool.Exec(ctx, `ANALYZE`); err != nil {
		pool.Close()
		return nil, fmt.Errorf("benchmark: ANALYZE: %w", err)
	}

	// The write object's current version is READ, never assumed to be 1.
	//
	// CreateObjectVersion is optimistic-concurrency guarded (it takes an
	// ExpectedVersion and appends ExpectedVersion+1), so a counter seeded from a
	// constant is correct exactly once: the first write of a freshly seeded
	// database. Every --no-seed re-run over an existing corpus, and the cold
	// sample (which wires a SECOND workload in the middle of the run), would
	// fail every call after that with an expected_version mismatch — and a
	// failed call is not a sample, so the write row would come back empty with
	// no explanation beyond the error. Reading the version instead makes the
	// counter start wherever the previous writes left the object.
	writeVersion, err := currentVersion(ctx, pool, c.HotVersionObjectID)
	if err != nil {
		pool.Close()
		return nil, err
	}

	return &Workload{
		Corpus:       c,
		Pool:         pool,
		RSG:          rsgSvc,
		Projects:     projectSvc,
		Ranking:      rankStore,
		Retriever:    retriever,
		Store:        store,
		SearchSQL:    sqlc.New(pool),
		Reader:       projects.Reader{UserID: c.OwnerID, Authenticated: true},
		Actor:        domain.User{ID: c.ActorID, Handle: "bench-actor", DisplayName: "Benchmark Actor"},
		Scope:        scope,
		Embedding:    c.QueryEmbedding,
		tracer:       tracer,
		benchURL:     benchURL,
		writeVersion: writeVersion,
	}, nil
}

// currentVersion reads the hot write object's latest version number.
func currentVersion(ctx context.Context, pool *pgxpool.Pool, objectID string) (int, error) {
	var v int
	err := pool.QueryRow(ctx, `SELECT version_no FROM scientific_object_versions
		WHERE object_id = $1 ORDER BY version_no DESC LIMIT 1`, objectID).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("benchmark: read the write object's current version "+
			"(is the corpus there?): %w", err)
	}
	return v, nil
}

// RebaseWriteVersion re-reads the write object's version into the counter.
//
// It is called after the cold sample of a WRITING operation, because that cold
// sample appends a version of its own through a different workload's counter:
// the warm counter is then one behind the object, and the next timed call would
// fail the optimistic-concurrency check rather than be measured. It runs
// outside every timed region, so it cannot move a number in the report.
func (w *Workload) RebaseWriteVersion(ctx context.Context) error {
	v, err := currentVersion(ctx, w.Pool, w.Corpus.HotVersionObjectID)
	if err != nil {
		return err
	}
	w.writeMu.Lock()
	w.writeVersion = v
	w.writeMu.Unlock()
	return nil
}

// Close releases the measurement pool.
func (w *Workload) Close() { w.Pool.Close() }

// Operation is one measured application operation: the thing docs/27 gives a
// budget to, the code that performs it, and how many samples it gets.
type Operation struct {
	// Name is the report's row key (snake_case; it is also the JSON field).
	Name string
	// SLOItem quotes the docs/27 line the budget comes from, so a reader can
	// check the target without trusting this file. Empty means docs/27 states
	// no budget for it — a probe.
	SLOItem string
	// SLOTarget is that line's number. Zero means no budget to compare to.
	SLOTarget time.Duration
	// Surface names the endpoint and the exact code path, file:line, that
	// performs the read or write. A p95 without this is a number nobody can
	// attribute.
	Surface string
	// Boundary says what is timed, in one clause. Every entry names the
	// database boundary rather than claiming to be an HTTP latency.
	Boundary string
	// Samples is how many timed calls the p95 is computed over. Warmup calls
	// precede them and are not recorded.
	Samples int
	Warmup  int
	// Writes marks an operation that appends state. Only the cold sample's
	// bookkeeping depends on it: a cold write moves the object's version on
	// through a DIFFERENT workload's counter, so the warm counter has to be
	// rebased before the timed samples start (see measure).
	Writes bool
	// Run performs one call exactly as the application performs it.
	//
	// The COLD sample is the same Run, on a workload wired fresh over a new
	// pool (measure.go's coldSample): the plan cache, the buffer cache and
	// the connection are all cold, which is what the first call after a
	// restart costs. It is one observation, and the report prints it as one.
	Run func(ctx context.Context) error
}

// operations is the measured set, in docs/27's own order.
func (w *Workload) operations() []*Operation {
	c := w.Corpus
	return []*Operation{
		{
			Name:      "project_overview",
			SLOItem:   "docs/27:6 普通 Project Overview p95 API < 500ms（不含大图/LLM）",
			SLOTarget: 500 * time.Millisecond,
			Surface: "GET /api/v1/projects/{projectId}/overview -> " +
				"rsg.Service.ProjectOverview (internal/application/rsg/overview.go:154); " +
				"GetProjectByID + GetProjectMembership (projects.sql:14,92), " +
				"ListObjectVersionsAsOf + ListRelationVersionsAsOf (rsg_query.sql:44,61), " +
				"ListBranchesByProject (rsg.sql:50), GetProjectStateByID (rsg.sql:74), " +
				"ListStateCommitsByBranch (rsg.sql:97)",
			Boundary: "the service call's database work (all queries + profile reads); not the HTTP round trip",
			Warmup:   5,
			Samples:  50,
			Run: func(ctx context.Context) error {
				_, err := w.RSG.ProjectOverview(ctx, w.Reader, c.HotProjectID)
				return err
			},
		},
		{
			Name:      "project_list",
			SLOItem:   "docs/27:7 常规列表 p95 < 400ms",
			SLOTarget: 400 * time.Millisecond,
			Surface: "GET /api/v1/projects -> projects.Service.List " +
				"(internal/application/projects/service.go:209); " +
				"ListPublicProjects (projects.sql:105) + ListProjectsForUser (projects.sql:96)",
			Boundary: "the service call's database work; not the HTTP round trip",
			Warmup:   5,
			Samples:  50,
			Run: func(ctx context.Context) error {
				_, err := w.Projects.List(ctx, w.Reader)
				return err
			},
		},
		{
			Name:      "object_detail",
			SLOItem:   "docs/27:8 Object detail p95 < 500ms",
			SLOTarget: 500 * time.Millisecond,
			Surface: "GET /api/v1/projects/{projectId}/branches/{branchId}/objects/{objectId} -> " +
				"rsg.Service.GetObjectDetail (internal/application/rsg/service.go:546); " +
				"GetScientificObjectByID (scientific_objects.sql:18), " +
				"ListScientificObjectVersions (scientific_objects.sql:66), " +
				"ListRelationVersionsForObject (relations.sql:83)",
			Boundary: "the service call's database work; not the HTTP round trip",
			Warmup:   5,
			Samples:  50,
			Run: func(ctx context.Context) error {
				_, err := w.RSG.GetObjectDetail(ctx, w.Reader, c.HotProjectID, c.HotMainBranchID, c.HotObjectID, nil)
				return err
			},
		},
		{
			Name:      "research_map",
			SLOItem:   "docs/27:9 Research Map initial aggregated query p95 < 1s，复杂展开可异步",
			SLOTarget: time.Second,
			Surface: "GET /api/v1/projects/{projectId}/research -> " +
				"rsg.Service.ResearchOutline (internal/application/rsg/outline.go:156); " +
				"the same ListObjectVersionsAsOf + ListRelationVersionsAsOf the overview runs " +
				"(cmd/api/rsghttp/researchmap.go:51-55 records that the map adds no read of its own)",
			Boundary: "the service call's database work; not the HTTP round trip",
			Warmup:   5,
			Samples:  50,
			Run: func(ctx context.Context) error {
				_, err := w.RSG.ResearchOutline(ctx, w.Reader, c.HotProjectID)
				return err
			},
		},
		{
			Name:      "candidate_retrieval",
			SLOItem:   "docs/27:12 Candidate retrieval p95 < 2s（seed/中小规模）",
			SLOTarget: 2 * time.Second,
			Surface: "POST /api/v1/search -> retrieval.Retriever.Retrieve " +
				"(internal/search/retrieval/retrieval.go:353) over " +
				"retrieval.SQLStore (internal/search/retrieval/store.go): " +
				"SearchDocumentsFullText (search.sql:154), SearchDocumentsByFacets (search.sql:216), " +
				"SeedObjectVersions + AdjacentRelations (search.sql), " +
				"SearchDocumentsByVector (search.sql:172) SKIPPED: no embedder is wired, " +
				"exactly as cmd/api/main.go:1280 wires it — see vector_scan for the vector half",
			Boundary: "the pipeline's database work in its production wiring; not the HTTP round trip",
			Warmup:   3,
			Samples:  30,
			Run: func(ctx context.Context) error {
				_, err := w.Retriever.Retrieve(ctx, w.Scope, retrieval.Request{
					Query:  c.QueryText,
					Limits: retrieval.Limits{},
				})
				return err
			},
		},
		{
			Name:      "semantic_command",
			SLOItem:   "docs/27:16 常规 semantic command p95 < 800ms（不含 blob upload）",
			SLOTarget: 800 * time.Millisecond,
			Surface: "POST /api/v1/projects/{projectId}/branches/{branchId}/objects/{objectId} -> " +
				"rsg.Service.CreateObjectVersion (internal/application/rsg/service.go:307) -> " +
				"states.Service.Commit -> StateStore.CommitState " +
				"(internal/persistence/state_store.go) as ONE transaction; " +
				"GetMainFrozenForBranch + CreateProjectState + UpdateBranchBaseState (rsg.sql:147,69,118), " +
				"CreateScientificObjectVersion + BumpScientificObjectVersionNo (scientific_objects.sql:32,21), " +
				"CreateStateCommit (rsg.sql:90)",
			Boundary: "the whole service write including its transaction commit; not the HTTP round trip",
			Warmup:   3,
			Samples:  30,
			Writes:   true,
			Run: func(ctx context.Context) error {
				w.writeMu.Lock()
				expected := w.writeVersion
				w.writeVersion++
				w.writeMu.Unlock()
				_, err := w.RSG.CreateObjectVersion(ctx, w.Actor, c.HotProjectID, c.HotWriteBranchID,
					c.HotVersionObjectID, rsg.CreateObjectVersionInput{
						ExpectedVersion: expected,
						Patch:           json.RawMessage(`{"benchmark_write":true}`),
					})
				return err
			},
		},
	}
}

// probes are measured the same way but compared to nothing: docs/27 states no
// budget for them. They exist because T1108 has to answer two questions about
// the search port that only a measurement can answer, and an unmeasured
// suspicion recorded as a finding would be exactly the "not measuring" outcome
// the task rules out.
func (w *Workload) probes() []*Operation {
	c := w.Corpus
	return []*Operation{
		{
			Name: "probe_deep_offset",
			Surface: "internal/persistence/queries/search.sql:25 SearchDocuments " +
				"(LIMIT @page_size OFFSET @page_offset, search.sql:45-46). " +
				"NOTE: this generated method has no production caller in this tree — no adapter in " +
				"internal/search calls it (only tests do). The probe measures the query as it is " +
				"written, because deep paging is a property of the query, not of its caller.",
			// The rows this reads are the NETWORK tier's, not the hot project's:
			// scale.go is explicit that the hot project carries no search
			// documents (docs/27's search budget is stated for the network
			// seed), and the probe passes w.Scope.AllowedProjectUUIDs(), which
			// covers the owner's 1 + NetworkProjects projects. The first version
			// of this line said "over the hot project's rows", which is the one
			// thing it cannot be — the hot project has none — and a boundary
			// that names the wrong rows is a boundary a reader cannot use to
			// check the number.
			Boundary: "one SearchDocuments call at the deepest page of the whole corpus (OFFSET " +
				"NetworkDocuments-20), scoped to the corpus owner's projects — which is the network tier: " +
				"the 100k search documents all belong to the NetworkProjects, and the hot project carries none",
			Warmup:  2,
			Samples: 15,
			Run: func(ctx context.Context) error {
				_, err := w.SearchSQL.SearchDocuments(ctx, sqlc.SearchDocumentsParams{
					Query:             c.QueryText,
					AllowedProjectIds: w.Scope.AllowedProjectUUIDs(),
					// The deepest offset a client can reach without walking past
					// the whole corpus: one page short of the end.
					PageOffset: int32(c.Scale.NetworkDocuments) - 20,
					PageSize:   20,
				})
				return err
			},
		},
		{
			Name: "probe_ranking_facts",
			Surface: "internal/persistence/queries/search.sql:400 SearchRankingFacts, called through " +
				"ranking.SQLStore.Factors (internal/search/ranking/store.go:121). The query carries a " +
				"per-row correlated subquery over scientific_object_versions (search.sql:412-417) " +
				"selecting max(version_no) per object.",
			Boundary: "one Factors call for a full answer page of candidate object versions",
			Warmup:   2,
			Samples:  15,
			Run: func(ctx context.Context) error {
				_, err := w.Ranking.Factors(ctx, w.Scope, w.candidateVersionIDs())
				return err
			},
		},
		{
			Name: "vector_scan",
			Surface: "internal/persistence/queries/search.sql:172 SearchDocumentsByVector through " +
				"retrieval.SQLStore.Vector (internal/search/retrieval/store.go:72). This is the vector " +
				"signal the production pipeline SKIPS (no embedder is wired), measured here on the same " +
				"corpus so the index decision recorded at search.sql:186-190 has its own numbers. The " +
				"query vector is the deterministic local embedder's output for the query text, in " +
				"pgvector's text syntax — the form the sqlc parameter takes.",
			Boundary: "one exact (non-approximate) vector scan over every embedded document in scope",
			Warmup:   2,
			Samples:  15,
			Run: func(ctx context.Context) error {
				_, err := w.Store.Vector(ctx, retrieval.VectorQuery{
					DocumentQuery: retrieval.DocumentQuery{Scope: w.Scope, PageSize: 20},
					Embedding:     w.Embedding,
					Model:         embedding.LocalModel,
				})
				return err
			},
		},
	}
}

// candidateVersionIDs returns the object version ids a full answer page would
// rank: one version per object of the hot project, at the corpus's version
// number. It reads them from the corpus rather than inventing uuids, because
// SearchRankingFacts' per-row subquery is per EXISTING object.
func (w *Workload) candidateVersionIDs() []string {
	const page = 20
	n := page
	if n > w.Corpus.Scale.HotObjects {
		n = w.Corpus.Scale.HotObjects
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, versionIDFor(benchUUID("object/hot", i)))
	}
	return out
}

// captureTracer records, per statement text, the last argument list a call was
// made with and how many times it ran.
//
// It exists so the gate can EXPLAIN the statement the workload ACTUALLY issued
// rather than a copy of the SQL kept in a table somewhere: a plan check written
// against a hand-copied query drifts away from the query the moment either
// changes, and it fails by silently continuing to pass. The tracer cannot
// drift, because there is nothing to keep in sync.
type captureTracer struct {
	mu    sync.Mutex
	stmts map[string]*capturedStatement
}

type capturedStatement struct {
	SQL   string
	Args  []any
	Calls int
	Err   error
}

// CapturedStatement is the read-only view the gate uses.
type CapturedStatement struct {
	SQL   string
	Args  []any
	Calls int
}

// traceSQLKey carries a statement's text from TraceQueryStart to
// TraceQueryEnd. pgx's TraceQueryEndData has no SQL field — only a
// CommandTag and an error — so the only place the statement can be carried
// across is the context TraceQueryStart returns. (Capturing the error matters:
// a statement that failed must not be EXPLAINed as if it were the operation's
// query.)
type traceSQLKey struct{}

func (t *captureTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	if t.stmts == nil {
		t.stmts = map[string]*capturedStatement{}
	}
	st, ok := t.stmts[data.SQL]
	if !ok {
		st = &capturedStatement{SQL: data.SQL}
		t.stmts[data.SQL] = st
	}
	st.Args = data.Args
	st.Calls++
	st.Err = nil
	t.mu.Unlock()
	return context.WithValue(ctx, traceSQLKey{}, data.SQL)
}

func (t *captureTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	sql, ok := ctx.Value(traceSQLKey{}).(string)
	if !ok {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if st, ok := t.stmts[sql]; ok && data.Err != nil {
		st.Err = data.Err
	}
}

// reset forgets every captured statement. It is called before an operation
// runs so the capture describes that operation alone.
func (t *captureTracer) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stmts = map[string]*capturedStatement{}
}

// snapshot returns the statements captured since the last reset that ran
// without error, keyed by statement text.
func (t *captureTracer) snapshot() []CapturedStatement {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]CapturedStatement, 0, len(t.stmts))
	for _, st := range t.stmts {
		if st.Err != nil || st.Calls == 0 {
			continue
		}
		out = append(out, CapturedStatement{SQL: st.SQL, Args: st.Args, Calls: st.Calls})
	}
	return out
}
