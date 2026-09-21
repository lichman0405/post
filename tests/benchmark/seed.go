package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/rsg/schemareg"
	"github.com/lichman0405/post/internal/search/embedding"
)

// Corpus is the identity of what a seed produced: the few rows a measurement
// needs to name, plus the row counts per table that gate.go checks against the
// scale. It deliberately does NOT hold 100k object ids — a corpus in memory is
// the thing the benchmark exists to avoid.
type Corpus struct {
	Scale Scale

	// OwnerID is the user who owns every project in the corpus and is a
	// member of every one of them, so a read as this actor is a read as a
	// member (not as an anonymous visitor whose access predicate prunes
	// everything and makes every query look fast).
	OwnerID string
	// ActorID is a second user, the actor of the semantic-command write.
	ActorID string

	// The hot project — docs/27:20's "单 Project" half.
	HotProjectID    string
	HotMainBranchID string
	// HotWriteBranchID is the active working branch the semantic-command write
	// appends to. It is NOT main: frozen main moves only through a Research PR
	// merge (CLAUDE.md §9 invariant 3), and docs/27:20's "1k branches/PR
	// history（active branch 可限制）" is the everyday case of writing on a
	// working branch. A write measured against main would be measuring the
	// refusal path.
	HotWriteBranchID string
	// HotHeadStateID is main's current head; advance() moves it on every
	// semantic-command write, so the measurement reads it from the corpus
	// rather than assuming what the last write left behind.
	HotHeadStateID string
	// HotObjectID is one object of the hot project, for the object-detail
	// read. HotVersionObjectID is a different one, used as the target of
	// the semantic-command write's version append.
	HotObjectID        string
	HotVersionObjectID string

	// The network half — docs/27:20's "100 Projects/100k searchable entities".
	NetworkProjectIDs []string
	// QueryEmbedding is a real vector of the corpus's own vocabulary,
	// produced by the embedder that produced the stored ones, and it is
	// handed to the retrieval reads as the text literal production sends.
	QueryEmbedding string
	// QueryText is the text QueryEmbedding was computed from.
	QueryText string

	// networkGenesis maps a network project to the one state its object hangs
	// on. It is unexported: it exists so two seed phases agree on an id, not
	// so a measurement can name it.
	networkGenesis map[string]string

	// Counts is what the seed actually put in each table. gate.go compares
	// it to Scale; measure.go prints it beside the numbers so a reader can
	// tell what was measured.
	Counts map[string]int
}

// objectTypes are the corpus's scientific object types. Every entry is a
// canonical V1 type — the name of a schema under specs/schemas, which is the
// same catalog internal/rsg/schemareg embeds and New resolves — because
// scientific_object_versions.schema_id is written from this list and
// rsg.Service.resolveSchemaRef refuses a type that is not canonical
// (internal/application/rsg/service.go: "object_type %q is not a canonical V1
// type"). A plausible-sounding type with no schema behind it ("measurement",
// say) would produce a corpus the application could not have created.
//
// They are deliberately NOT research_question, hypothesis or
// external_reference: those three carry deferred constraint triggers
// (infra/migrations/00040_question_hypothesis_relationship_guards.sql,
// 00045_external_reference_live_identity_and_snapshot.sql) whose subject is the
// API's reference structure — a question's parent, a hypothesis's question, an
// external reference's live identity — and a synthetic corpus has none of that
// structure to give them. Naming one of them here would not make the corpus
// more realistic; it would make the seed produce rows the guards must reject.
//
// relation, evidence-assertion, research-asset-version, rsg-manifest and
// core-scientific-object are schemas too, but none is a scientific object type
// a project authors objects of in this corpus: the first is the relation
// payload, the second is owned by the assertion path, and the rest describe
// structure rather than objects.
var objectTypes = []string{"sample", "material", "protocol", "dataset", "experiment", "calculation"}

// versionIDFor is the id of an object's version 1 as seedObjects writes it.
// The corpus writes exactly one version per object, so the id is a function of
// the object id — but it is spelled HERE rather than at a call site, so a
// measurement that needs a version id cannot invent one that does not exist
// (an id that names no row turns a ranking read into a no-op that still
// "succeeds").
func versionIDFor(objectID string) string { return benchUUID("version/"+objectID, 0) }

// schemaIDFor is the canonical schema id an object of this type pins, spelled
// the way internal/rsg/schemareg spells it so a seeded version row and an
// application-written one are the same shape. checkCanonicalSchemas proves the
// registry really holds each one before the seed writes any row.
func schemaIDFor(objectType string) string {
	return schemareg.CanonicalNamespace + objectType + ".schema.json"
}

// checkCanonicalSchemas refuses to seed a type the application could not have
// created: every entry of objectTypes must resolve in the real registry, at
// the canonical version. It is a seed precondition rather than a gate entry
// because a corpus written under schema ids the registry does not know is not
// a corpus anyone should measure against — the failure has to happen before
// the load, not in a report afterwards.
func checkCanonicalSchemas() error {
	reg, err := schemareg.New()
	if err != nil {
		return fmt.Errorf("schemareg.New: %w", err)
	}
	for _, typ := range objectTypes {
		ref := schemareg.Ref{ID: schemaIDFor(typ), Version: schemareg.CanonicalV1}
		if _, err := reg.Lookup(ref); err != nil {
			return fmt.Errorf("objectTypes names %q, which is not a canonical V1 schema (%s v%s): %w",
				typ, ref.ID, ref.Version, err)
		}
	}
	return nil
}

// chainEpoch is the instant the corpus's state lineage starts at. It is a fixed
// date rather than now() so a corpus is reproducible: two seeds of the same
// scale produce the same rows, including the ones the reads order by
// created_at (see the created_at note in seedStatesAndCommits).
var chainEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// relationTypes is the corpus's relation vocabulary. derived_from is a
// provenance relation and characterizes is not (see
// provenance_relation_types() in infra/migrations/00043), so the pairs exercise
// both the provenance projection trigger and the plain path.
var relationTypes = []string{"derived_from", "characterizes"}

// vocabulary is the corpus's text. The words are ordinary material-science
// nouns so the FTS half of the search reads has something real to match with
// plainto_tsquery('simple', ...) — the same configuration the projection's
// index is built for.
var vocabulary = []string{
	"mof", "humidity", "separation", "adsorption", "isotherm", "porosity",
	"linker", "node", "solvent", "crystal", "lattice", "permeance",
	"selectivity", "carbon", "capture", "membrane", "catalyst", "kinetics",
	"diffraction", "spectra", "thermal", "stability", "surface", "pore",
}

// queryText is the question a retrieval measurement poses. It is deliberately
// made only of vocabulary words so that it retrieves a real, non-empty
// candidate set: a query that matches nothing would measure the cost of
// scanning and finding zero rows, which is not what a candidate-retrieval
// budget is about.
const queryText = "mof humidity separation adsorption isotherm"

// benchUUID builds a deterministic, correctly-shaped UUID from a label and an
// index.
//
// Deterministic rather than random because reproducibility is the whole point
// of a baseline: two runs at the same scale produce the same ids, so a plan or
// a row count that changed between them changed for a reason a reviewer can
// see. The version and variant nibbles are set so the value is a well-formed
// v4 UUID and PostgreSQL's uuid input accepts it.
func benchUUID(label string, n int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "T1108/%s/%d", label, n))
	sum[6] = (sum[6] & 0x0f) | 0x40
	sum[8] = (sum[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[0:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}

// benchHash builds the hex digest shape state_hash and integrity_hash carry.
func benchHash(label string, n int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "T1108/hash/%s/%d", label, n))
	return hex.EncodeToString(sum[:])
}

// crockford is the alphabet research_assets_pid_format and
// knowledge_publications_pid_format use (00064, 00083): a pid is 26 Crockford
// base32 characters. A search document's entity_ref is "<type>:<identity>" and
// for a knowledge document the identity is that pid, so the corpus spells it
// the same way the projection does rather than inventing a shorter key.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func benchPID(n int) string {
	var b [26]byte
	sum := sha256.Sum256(fmt.Appendf(nil, "T1108/pid/%d", n))
	for i := range b {
		b[i] = crockford[int(sum[i%len(sum)])%len(crockford)]
	}
	return string(b[:])
}

// seedCorpus fills a freshly migrated database to the requested scale.
//
// # What it writes and in what order
//
// The order is the foreign-key order, with one deliberate exception: branches
// carry a NULL base_state_id until their states exist. branches.base_state_id
// references project_states and project_states.branch_id references branches
// (specs/database/postgres.sql:193-231), so the pair is a cycle and the
// nullable column is the only way in. The head is written back at the end,
// because ProjectOverview reads it to decide whether main has "no accepted
// state yet".
//
// # Triggers are left ON
//
// Nothing here disables a trigger. DISABLE TRIGGER USER would make the seed
// faster and would also make it a corpus the application could not have
// written — the branch git_ref rule, the pull-request gates, the provenance
// projection and the two deferred reference guards would all be skipped, and
// the report would then be describing a database shape the product does not
// have. The guards are satisfied instead (refs/heads/<name>, same-project pull
// requests, object types with no guard), and the only trigger this seed
// deliberately does not trip is the provenance projection's, which fires for
// half of the relation types — by design, so both paths are exercised.
func seedCorpus(ctx context.Context, pool *pgxpool.Pool, sc Scale) (*Corpus, error) {
	c := &Corpus{
		Scale:          sc,
		OwnerID:        benchUUID("user/owner", 0),
		ActorID:        benchUUID("user/actor", 0),
		Counts:         map[string]int{},
		networkGenesis: map[string]string{},
	}
	rng := rand.New(rand.NewPCG(0x7108, 0xBE0C))

	if err := checkCanonicalSchemas(); err != nil {
		return nil, fmt.Errorf("benchmark: %w", err)
	}

	embedder, err := embedding.NewDeterministic(embedding.LocalModel)
	if err != nil {
		return nil, fmt.Errorf("benchmark: build the deterministic embedder: %w", err)
	}

	if err := seedIdentity(ctx, pool, c); err != nil {
		return nil, err
	}
	if err := seedHotProjects(ctx, pool, c); err != nil {
		return nil, err
	}
	if err := seedBranches(ctx, pool, c); err != nil {
		return nil, err
	}
	if err := seedStatesAndCommits(ctx, pool, c, rng); err != nil {
		return nil, err
	}
	if err := seedObjects(ctx, pool, c, rng); err != nil {
		return nil, err
	}
	if err := seedRelations(ctx, pool, c); err != nil {
		return nil, err
	}
	if err := seedPullRequests(ctx, pool, c); err != nil {
		return nil, err
	}
	if err := seedSearchDocuments(ctx, pool, c, embedder, rng); err != nil {
		return nil, err
	}
	if err := finalizeHeads(ctx, pool, c); err != nil {
		return nil, err
	}
	// ANALYZE before any measurement: without statistics PostgreSQL plans on
	// defaults, and a plan chosen from "this table is probably tiny" is a plan
	// the gate would then assert about. The measurement must run on the plan a
	// real deployment would get.
	if _, err := pool.Exec(ctx, `ANALYZE`); err != nil {
		return nil, fmt.Errorf("benchmark: ANALYZE: %w", err)
	}
	if err := countRows(ctx, pool, c); err != nil {
		return nil, err
	}
	return c, nil
}

func seedIdentity(ctx context.Context, pool *pgxpool.Pool, c *Corpus) error {
	if err := copyRows(ctx, pool, "users", []string{"id", "handle", "display_name"}, [][]any{
		{c.OwnerID, "bench-owner", "Benchmark Owner"},
		{c.ActorID, "bench-actor", "Benchmark Actor"},
	}); err != nil {
		return err
	}
	c.Counts["users"] = 2
	return nil
}

func seedHotProjects(ctx context.Context, pool *pgxpool.Pool, c *Corpus) error {
	total := 1 + c.Scale.NetworkProjects
	rows := make([][]any, 0, total)
	c.HotProjectID = benchUUID("project/hot", 0)
	rows = append(rows, []any{
		c.HotProjectID, "lattice-lab", "Lattice Lab",
		"the single-project capacity workload of docs/27:20",
		"active", "public", c.OwnerID,
	})
	c.NetworkProjectIDs = make([]string, 0, c.Scale.NetworkProjects)
	for i := 0; i < c.Scale.NetworkProjects; i++ {
		id := benchUUID("project/net", i)
		c.NetworkProjectIDs = append(c.NetworkProjectIDs, id)
		rows = append(rows, []any{
			id, fmt.Sprintf("network-%03d", i), fmt.Sprintf("Network Project %03d", i),
			"the network capacity workload of docs/27:20",
			"active", "public", c.OwnerID,
		})
	}
	if err := copyRows(ctx, pool, "projects",
		[]string{"id", "slug", "name", "purpose", "activity_status", "visibility", "created_by"}, rows); err != nil {
		return err
	}
	c.Counts["projects"] = total

	members := make([][]any, 0, total+1)
	members = append(members, []any{c.HotProjectID, c.OwnerID, "owner"})
	members = append(members, []any{c.HotProjectID, c.ActorID, "contributor"})
	for _, id := range c.NetworkProjectIDs {
		members = append(members, []any{id, c.OwnerID, "owner"})
	}
	if err := copyRows(ctx, pool, "project_memberships", []string{"project_id", "user_id", "role"}, members); err != nil {
		return err
	}
	c.Counts["project_memberships"] = len(members)
	return nil
}

// seedBranches writes branches with a NULL base_state_id; finalizeHeads fills
// it in once the states exist.
//
// git_ref is not free-form: branch_git_ref_guard
// (infra/migrations/00031_branch_git_ref_sync.sql:91-95) rejects any row whose
// git_ref is not 'refs/heads/' || name, and the guard is a BEFORE INSERT
// trigger, so getting this wrong fails the seed rather than the read.
func seedBranches(ctx context.Context, pool *pgxpool.Pool, c *Corpus) error {
	cols := []string{"id", "project_id", "name", "visibility", "git_ref", "lifecycle_state", "created_by"}
	rows := make([][]any, 0, c.Scale.HotBranches+c.Scale.NetworkProjects)
	c.HotMainBranchID = benchUUID("branch/main", 0)
	c.HotWriteBranchID = c.HotMainBranchID
	rows = append(rows, []any{c.HotMainBranchID, c.HotProjectID, "main", "public", "refs/heads/main", "active", c.OwnerID})
	for i := 1; i < c.Scale.HotBranches; i++ {
		name := fmt.Sprintf("study-%04d", i)
		rows = append(rows, []any{benchUUID("branch/hot", i), c.HotProjectID, name, "public", "refs/heads/" + name, "active", c.OwnerID})
	}
	if c.Scale.HotBranches > 1 {
		c.HotWriteBranchID = benchUUID("branch/hot", 1)
	}
	for i, pid := range c.NetworkProjectIDs {
		rows = append(rows, []any{benchUUID("branch/net", i), pid, "main", "public", "refs/heads/main", "active", c.OwnerID})
	}
	if err := copyRows(ctx, pool, "branches", cols, rows); err != nil {
		return err
	}
	c.Counts["branches"] = len(rows)
	return nil
}

// seedStatesAndCommits writes the hot project's state lineage: one initial
// state per branch, then the capacity baseline's state transitions as a linear
// chain on main.
//
// The chain is what makes docs/27's "10k state transitions" a read cost rather
// than a row count: ProjectOverview calls states.ListCommits(main), which is
// ListStateCommitsByBranch (internal/persistence/queries/rsg.sql:97) and has
// no LIMIT, so all of them come back on every overview render.
func seedStatesAndCommits(ctx context.Context, pool *pgxpool.Pool, c *Corpus, rng *rand.Rand) error {
	// created_at is written EXPLICITLY rather than left to its DEFAULT now().
	// Every row of one COPY shares the transaction's timestamp, and both reads
	// that have to name "the newest state/commit of this branch" break that tie
	// by id: ListStateCommitsByBranch orders `created_at, id`
	// (internal/persistence/queries/rsg.sql:97) and the branch-head lookup
	// orders `created_at DESC, id DESC` (finalizeHeads below). With one shared
	// timestamp the winner is a random uuid, so the state the corpus calls
	// "main's head" need not be the end of main's chain — and an object version
	// hung on a mid-chain state is a version the head's ancestor walk does not
	// reach, which would make every read in this package return nothing and
	// measure nothing. A one-second-per-transition clock makes the chain's
	// order and its timestamps agree.
	stateCols := []string{"id", "project_id", "branch_id", "parent_state_id", "state_hash", "manifest_version", "created_at"}
	commitCols := []string{"id", "project_id", "branch_id", "base_state_id", "result_state_id", "actor_id", "via", "message", "operation_summary", "created_at"}
	chainClock := func(step int) time.Time { return chainEpoch.Add(time.Duration(step) * time.Second) }

	states := make([][]any, 0, batchRows)
	commits := make([][]any, 0, batchRows)
	total := 0

	flush := func() error {
		if len(states) > 0 {
			if err := copyRows(ctx, pool, "project_states", stateCols, states); err != nil {
				return err
			}
			states = states[:0]
		}
		if len(commits) > 0 {
			if err := copyRows(ctx, pool, "state_commits", commitCols, commits); err != nil {
				return err
			}
			commits = commits[:0]
		}
		return nil
	}

	// One initial state per branch. Network projects get exactly one, which is
	// the state their single object version hangs on.
	genesisOf := func(projectID, branchID, label string) string {
		id := benchUUID("state/0/"+label, total)
		// The hash's label is "genesis/<label>", NOT "state/<label>": the chain
		// below hashes its states as "state/main"/i, and project_states carries
		// UNIQUE (project_id, state_hash) — so a genesis hashed as
		// "state/main"/0 collides with the chain's first transition and the
		// COPY is refused by the database. The ids do not collide (different
		// labels), which is exactly why this is worth a comment: the id and the
		// hash are derived from the same string here, and only one of them has
		// a uniqueness constraint.
		states = append(states, []any{id, projectID, branchID, nil, benchHash("genesis/"+label, total), "v1", chainEpoch})
		total++
		return id
	}
	mainHead := genesisOf(c.HotProjectID, c.HotMainBranchID, "main")
	c.HotHeadStateID = mainHead
	for i := 1; i < c.Scale.HotBranches; i++ {
		genesisOf(c.HotProjectID, benchUUID("branch/hot", i), "branch")
	}
	for i, pid := range c.NetworkProjectIDs {
		c.networkGenesis[pid] = genesisOf(pid, benchUUID("branch/net", i), "net")
	}
	if err := flush(); err != nil {
		return err
	}

	// The hot project's state transitions: a linear chain on main, each one a
	// state plus the commit that produced it.
	parent := mainHead
	for i := 0; i < c.Scale.HotStateCommits; i++ {
		result := benchUUID("state/main", i)
		at := chainClock(i + 1)
		states = append(states, []any{result, c.HotProjectID, c.HotMainBranchID, parent, benchHash("state/main", i), "v1", at})
		commits = append(commits, []any{
			benchUUID("commit/main", i), c.HotProjectID, c.HotMainBranchID,
			parent, result, c.OwnerID, "api",
			fmt.Sprintf("state transition %d", i),
			[]byte(`{"objects":1,"relations":1}`),
			at,
		})
		parent = result
		if len(commits) >= batchRows {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	c.HotHeadStateID = parent
	return nil
}

// seedObjects writes the hot project's scientific objects and the network
// projects' one object each.
//
// Every version hangs on a state the project owns, because that is the
// enumeration rule ListObjectVersionsAsOf is written to (ADR-027, quoted at
// internal/persistence/queries/rsg_query.sql:7-31): a project's RSG is what
// its state lineage carries, so an object version attached to another project's
// state would be invisible to every read this corpus exists to measure.
//
// current_version_no is written explicitly. Nothing maintains it — it is a
// compare-and-swap counter the application advances in the same transaction as
// the version insert (infra/migrations/00024, 00025) — so a bulk load that left
// it at its DEFAULT 0 would produce objects the application believes have no
// versions.
func seedObjects(ctx context.Context, pool *pgxpool.Pool, c *Corpus, rng *rand.Rand) error {
	objCols := []string{"id", "project_id", "object_type", "current_version_no", "created_by"}
	verCols := []string{"id", "object_id", "version_no", "state_id", "branch_id", "schema_id", "schema_version", "title", "lifecycle_state", "payload", "integrity_hash", "created_by"}

	objs := make([][]any, 0, batchRows)
	vers := make([][]any, 0, batchRows)
	flush := func() error {
		if len(objs) > 0 {
			if err := copyRows(ctx, pool, "scientific_objects", objCols, objs); err != nil {
				return err
			}
			objs = objs[:0]
		}
		if len(vers) > 0 {
			if err := copyRows(ctx, pool, "scientific_object_versions", verCols, vers); err != nil {
				return err
			}
			vers = vers[:0]
		}
		return nil
	}

	add := func(objectID, projectID, stateID, branchID, typ, title string) {
		objs = append(objs, []any{objectID, projectID, typ, 1, c.OwnerID})
		payload, _ := json.Marshal(map[string]any{
			"name": title, "value": rng.Float64(), "unit": "K",
			"terms": []string{vocabulary[rng.IntN(len(vocabulary))], vocabulary[rng.IntN(len(vocabulary))]},
		})
		vers = append(vers, []any{
			benchUUID("version/"+objectID, 0), objectID, 1, stateID, branchID,
			schemaIDFor(typ), schemareg.CanonicalV1, title, "active", payload,
			benchHash("version/"+objectID, 0), c.OwnerID,
		})
	}

	for i := 0; i < c.Scale.HotObjects; i++ {
		typ := objectTypes[i%len(objectTypes)]
		add(benchUUID("object/hot", i), c.HotProjectID, c.HotHeadStateID, c.HotMainBranchID,
			typ, fmt.Sprintf("%s %d of the lattice study", strings.ToUpper(typ), i))
		if len(objs) >= batchRows {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	c.HotObjectID = benchUUID("object/hot", 0)
	// A distinct object for the semantic-command write's version append: the
	// detail read and the write must not contend for the same row, or a p95
	// would report lock waits as read cost.
	c.HotVersionObjectID = benchUUID("object/hot", c.Scale.HotObjects-1)

	for i, pid := range c.NetworkProjectIDs {
		if err := flush(); err != nil {
			return err
		}
		per := c.Scale.NetworkDocuments / len(c.NetworkProjectIDs)
		if i < c.Scale.NetworkDocuments%len(c.NetworkProjectIDs) {
			per++
		}
		for j := 0; j < per; j++ {
			n := i*100000 + j
			typ := objectTypes[n%len(objectTypes)]
			add(benchUUID("object/net", n), pid, c.networkGenesis[pid], benchUUID("branch/net", i),
				typ, fmt.Sprintf("%s %d of the network study", strings.ToUpper(typ), n))
			if len(objs) >= batchRows {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return nil
}

// seedRelations writes the hot project's relations and their single version.
// Both endpoints are versions of the SAME project, so the relation is one the
// project's own lineage carries and the reads can see it.
func seedRelations(ctx context.Context, pool *pgxpool.Pool, c *Corpus) error {
	relCols := []string{"id", "project_id", "current_version_no"}
	verCols := []string{"id", "relation_id", "version_no", "state_id", "relation_type", "source_object_version_id", "target_object_version_id", "payload", "integrity_hash", "created_by"}

	rels := make([][]any, 0, batchRows)
	vers := make([][]any, 0, batchRows)
	flush := func() error {
		if len(rels) > 0 {
			if err := copyRows(ctx, pool, "relations", relCols, rels); err != nil {
				return err
			}
			rels = rels[:0]
		}
		if len(vers) > 0 {
			if err := copyRows(ctx, pool, "relation_versions", verCols, vers); err != nil {
				return err
			}
			vers = vers[:0]
		}
		return nil
	}

	for i := 0; i < c.Scale.HotRelations; i++ {
		src := i % c.Scale.HotObjects
		dst := (i*7 + 3) % c.Scale.HotObjects
		if src == dst {
			dst = (dst + 1) % c.Scale.HotObjects
		}
		relID := benchUUID("relation", i)
		rels = append(rels, []any{relID, c.HotProjectID, 1})
		vers = append(vers, []any{
			benchUUID("relationversion", i), relID, 1, c.HotHeadStateID,
			relationTypes[i%len(relationTypes)],
			benchUUID("version/"+benchUUID("object/hot", src), 0),
			benchUUID("version/"+benchUUID("object/hot", dst), 0),
			[]byte(`{"note":"synthetic"}`), benchHash("relationversion", i), c.OwnerID,
		})
		if len(rels) >= batchRows {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	return nil
}

// seedPullRequests writes the hot project's PR history.
//
// Every PR is same-project (source and target branch both belong to the hot
// project), which is what pull_request_fork_gate
// (infra/migrations/00086_external_fork.sql:197-207) requires of a row with no
// matching project_forks entry, and state 'open' with a NULL merged_at, which
// is the pair pull_request_guard (infra/migrations/00051:100-107) accepts.
func seedPullRequests(ctx context.Context, pool *pgxpool.Pool, c *Corpus) error {
	cols := []string{"id", "project_id", "number", "source_branch_id", "target_branch_id",
		"base_state_id", "proposed_state_id", "title", "body", "state", "created_by"}
	rows := make([][]any, 0, c.Scale.HotPullRequests)
	for i := 0; i < c.Scale.HotPullRequests; i++ {
		src := c.hotBranchFor(i)
		rows = append(rows, []any{
			benchUUID("pullrequest", i), c.HotProjectID, int64(i + 1),
			src, c.HotMainBranchID,
			c.HotHeadStateID, c.HotHeadStateID,
			fmt.Sprintf("merge study %d into main", i), "", "open", c.OwnerID,
		})
	}
	if err := copyRows(ctx, pool, "pull_requests", cols, rows); err != nil {
		return err
	}
	c.Counts["pull_requests"] = len(rows)
	return nil
}

// seedSearchDocuments writes the network tier's searchable entities.
//
// # The documents carry real vectors from the shipped embedder
//
// embedding.NewDeterministic(embedding.LocalModel) is POST's own V1 embedder
// (internal/search/embedding/deterministic.go). Using it rather than random
// float32s is the difference between a vector column of the right WIDTH and a
// vector column of the right MEANING: the retrieval reads compare against a
// query vector produced by the same implementation, so the corpus's recall is
// real lexical overlap instead of noise. It is also what makes the provenance
// triple on every row the same constant the application writes, which is the
// predicate SearchDocumentsByVector filters on.
//
// # The row shape is the projection's
//
// entity_ref is "<entity_type>:<identity>" (internal/search/projection.go:98),
// the identity for a knowledge document is the entity's pid, and structured is
// the facet object structuredFacets builds — entity_type, project_id and the
// entity's own facets. The documents are written directly rather than by
// running the projector over knowledge_publications, and that boundary is
// stated in the report: the corpus is a size fixture, and the projector's
// sources (published assets, knowledge publications, releases) have semantics
// this task does not own.
func seedSearchDocuments(ctx context.Context, pool *pgxpool.Pool, c *Corpus, embedder embedding.Deterministic, rng *rand.Rand) error {
	cols := []string{"entity_ref", "entity_type", "visibility", "project_id", "title", "content",
		"structured", "embedding", "embedding_provider", "embedding_model", "embedding_version"}

	total := c.Scale.NetworkDocuments
	rows := make([][]any, 0, batchRows)
	texts := make([]string, 0, batchRows)
	flush := func() error {
		if len(rows) == 0 {
			return nil
		}
		vecs, err := embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("benchmark: embed a batch: %w", err)
		}
		for i := range rows {
			rows[i][7] = vecs[i]
		}
		if err := copyRows(ctx, pool, "search_documents", cols, rows); err != nil {
			return err
		}
		rows, texts = rows[:0], texts[:0]
		return nil
	}

	for n := 0; n < total; n++ {
		pid := c.NetworkProjectIDs[n%len(c.NetworkProjectIDs)]
		title := fmt.Sprintf("%s study %d", strings.ToUpper(vocabulary[n%len(vocabulary)]), n)
		// The first six terms are always the query's, so the query retrieves a
		// real candidate set; the rest is filler that changes per document.
		content := strings.Join([]string{
			queryText,
			vocabulary[rng.IntN(len(vocabulary))],
			vocabulary[rng.IntN(len(vocabulary))],
			vocabulary[rng.IntN(len(vocabulary))],
			fmt.Sprintf("sample %d of the network corpus", n),
		}, " ")
		structured, err := json.Marshal(map[string]string{
			"entity_type": "knowledge",
			"project_id":  pid,
			"object_type": objectTypes[n%len(objectTypes)],
		})
		if err != nil {
			return err
		}
		rows = append(rows, []any{
			"knowledge:" + benchPID(n), "knowledge", "public", pid, title, content, structured, nil,
			embedding.LocalModel.Provider, embedding.LocalModel.Name, embedding.LocalModel.Version,
		})
		texts = append(texts, title+" "+content)
		if len(rows) >= batchRows {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}

	// The retrieval measurement's query vector, from the same embedder, as
	// pgvector's text syntax — the exact form the application sends
	// (internal/persistence/queries/search.sql:206-208 casts $1 to vector and
	// the parameter's Go type is string).
	qv, err := embedder.Embed(ctx, []string{queryText})
	if err != nil {
		return fmt.Errorf("benchmark: embed the query: %w", err)
	}
	c.QueryEmbedding = vectorLiteral(qv[0])
	c.QueryText = queryText
	return nil
}

// hotBranchFor returns the id of the working branch for the i-th row of a
// corpus structure that names one — the pull requests' source branch today.
//
// # Why the divisor is guarded here and not at the call site
//
// The expression this replaces was `benchUUID("branch/hot", 1+i%(c.Scale.HotBranches-1))`:
// a modulo by HotBranches-1, which is zero when a scale has exactly one branch,
// and a Go integer division by zero is a PANIC. A panic inside the seed is the
// worst place for one — it leaves a half-written corpus behind and the
// append-only tables cannot be cleaned without dropping the whole database
// (db.go's provision says why) — and it is unreachable from the two shipped
// tiers, so only an assertion can catch it. selfcheck.go asserts this function
// on both a one-branch and a thousand-branch scale.
//
// HotBranches <= 1 returns main, which is the only branch that exists. The
// resulting pull request names main on both sides; that is a degenerate row, and
// it is what this path is: a scale with one branch has no working branch for a
// merge to come from, so the choice is between a row whose branches coincide and
// a crash. The row is admissible — pull_request_guard
// (infra/migrations/00051_pull_request_state_and_fixity.sql:52-115) constrains
// the project/number/branch triple's immutability and the state machine, and
// says nothing about source vs target — and neither shipped scale reaches here.
func (c *Corpus) hotBranchFor(i int) string {
	if c.Scale.HotBranches <= 1 {
		return c.HotMainBranchID
	}
	return benchUUID("branch/hot", 1+i%(c.Scale.HotBranches-1))
}

// finalizeHeads writes each branch's head state back, which is the one value
// the corpus cannot write in insertion order: branches.base_state_id and
// project_states.branch_id reference each other.
//
// This is also what decides whether ProjectOverview's "current main" branch
// renders at all (internal/application/rsg/overview.go:180-197): a main with a
// NULL head is answered with "no accepted state yet" and skips both the state
// read and the commit list, which would make the overview's most expensive
// read disappear from the measurement.
func finalizeHeads(ctx context.Context, pool *pgxpool.Pool, c *Corpus) error {
	if _, err := pool.Exec(ctx, `
		UPDATE branches b
		   SET base_state_id = s.id
		  FROM (
		    SELECT DISTINCT ON (branch_id) branch_id, id
		      FROM project_states
		     ORDER BY branch_id, created_at DESC, id DESC
		  ) s
		 WHERE s.branch_id = b.id`); err != nil {
		return fmt.Errorf("benchmark: write branch heads: %w", err)
	}
	// The hot project's main must point at the LAST state of the chain, not at
	// an arbitrary one: ProjectOverview follows the head it finds here.
	var head string
	if err := pool.QueryRow(ctx,
		`SELECT id FROM project_states WHERE project_id = $1 AND branch_id = $2
		  ORDER BY created_at DESC, id DESC LIMIT 1`, c.HotProjectID, c.HotMainBranchID).Scan(&head); err != nil {
		return fmt.Errorf("benchmark: read main's head back: %w", err)
	}
	c.HotHeadStateID = head
	return nil
}

// countRows reads the row counts the report prints and gate.go checks.
func countRows(ctx context.Context, pool *pgxpool.Pool, c *Corpus) error {
	for _, t := range []string{
		"users", "projects", "project_memberships", "branches", "project_states", "state_commits",
		"scientific_objects", "scientific_object_versions", "relations", "relation_versions",
		"pull_requests", "search_documents", "evidence_assertions",
	} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{t}.Sanitize()).Scan(&n); err != nil {
			return fmt.Errorf("benchmark: count %s: %w", t, err)
		}
		c.Counts[t] = n
	}
	return nil
}

// copyRows is the one bulk-write helper: pgx's binary COPY, which is the only
// practical way to put 100k rows with a 6 KB vector column into a table.
func copyRows(ctx context.Context, pool *pgxpool.Pool, table string, cols []string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	_, err := pool.CopyFrom(ctx, pgx.Identifier{table}, cols, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("benchmark: copy into %s (%d rows): %w", table, len(rows), err)
	}
	return nil
}

// vectorLiteral renders a vector in pgvector's text input syntax, which is what
// the application sends as the query parameter and what the seed's own SQL
// would accept.
func vectorLiteral(v []float32) string {
	var b strings.Builder
	b.Grow(len(v) * 9)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%g", x)
	}
	b.WriteByte(']')
	return b.String()
}
