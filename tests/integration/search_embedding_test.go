package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/search/embedding"
	"github.com/lichman0405/post/internal/worker"
)

// searchEmbeddingTaskID namespaces this file's test databases (docs/66 §3).
const searchEmbeddingTaskID = "T0902"

// embeddingModel names one model version of the in-process embedder. Two
// values of it are the "change the provider" of the acceptance criterion.
func embeddingModel(version string) embedding.Model {
	return embedding.Model{
		Provider: embedding.LocalModel.Provider,
		Name:     embedding.LocalModel.Name,
		Version:  version,
	}
}

func mustEmbedder(t *testing.T, m embedding.Model) embedding.Deterministic {
	t.Helper()
	d, err := embedding.NewDeterministic(m)
	if err != nil {
		t.Fatalf("NewDeterministic(%v): %v", m, err)
	}
	return d
}

func mustEmbeddingWorker(t *testing.T, pool *pgxpool.Pool, emb embedding.Embedder, opts ...embedding.WorkerOption) *embedding.Worker {
	t.Helper()
	w, err := embedding.NewWorker(pool, emb, opts...)
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	return w
}

// seedSearchDocument writes one search_documents row the way the projection
// does (raw SQL, like search_access_test.go: the projection's own path is
// T0901's subject, and the embedding job's subject is the column it fills).
func seedSearchDocument(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	ref, title, content, visibility string, projectID pgtype.UUID) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO search_documents
		(entity_ref, entity_type, visibility, project_id, title, content, structured)
		VALUES ($1,'dataset',$2,$3,$4,$5,'{}'::jsonb)`,
		ref, visibility, projectID, title, content); err != nil {
		t.Fatalf("seed search document %s: %v", ref, err)
	}
}

// storedEmbedding is one row's vector and provenance, read back as SQL text
// (the vector is read through embedding::text, the same representation the
// write path uses).
type storedEmbedding struct {
	provider *string
	model    *string
	version  *string
	vector   *string
}

func readEmbedding(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ref string) storedEmbedding {
	t.Helper()
	var got storedEmbedding
	err := pool.QueryRow(ctx, `SELECT embedding_provider, embedding_model, embedding_version, embedding::text
		FROM search_documents WHERE entity_ref = $1`, ref).
		Scan(&got.provider, &got.model, &got.version, &got.vector)
	if err != nil {
		t.Fatalf("read the embedding of %s: %v", ref, err)
	}
	return got
}

// describe renders a stored embedding for a failure message: a nil pointer
// must be readable as "NULL", not as a crash in the test's own formatting.
func (s storedEmbedding) describe() string {
	part := func(label string, p *string) string {
		if p == nil {
			return label + "=NULL"
		}
		return label + "=" + *p
	}
	return strings.Join([]string{part("provider", s.provider), part("model", s.model),
		part("version", s.version), part("vector", s.vector)}, " ")
}

func (s storedEmbedding) isNull() bool {
	return s.provider == nil && s.model == nil && s.version == nil && s.vector == nil
}

// requireProvenance asserts a row carries exactly the model's identity —
// all three parts, since a vector without its provenance is not a usable
// vector.
func requireProvenance(t *testing.T, what string, got storedEmbedding, m embedding.Model) {
	t.Helper()
	if got.provider == nil || got.model == nil || got.version == nil {
		t.Fatalf("%s: provenance is incomplete: %s", what, got.describe())
	}
	if *got.provider != m.Provider || *got.model != m.Name || *got.version != m.Version {
		t.Fatalf("%s: provenance is %s, want the model that embedded it (%s)", what, got.describe(), m)
	}
	if got.vector == nil || *got.vector == "" {
		t.Fatalf("%s: provenance is present but there is no vector: %s", what, got.describe())
	}
}

// parseVector reads back the stored pgvector text form.
func parseVector(t *testing.T, what, text string) []float32 {
	t.Helper()
	body := strings.TrimSuffix(strings.TrimPrefix(text, "["), "]")
	fields := strings.Split(body, ",")
	out := make([]float32, len(fields))
	for i, f := range fields {
		v, err := strconv.ParseFloat(strings.TrimSpace(f), 32)
		if err != nil {
			t.Fatalf("%s: the stored vector is not parsable at %d: %q", what, i, text)
		}
		out[i] = float32(v)
	}
	return out
}

// requireVectorIs is the content check the write path needs: the row must
// hold the vector the embedder computed for that document's text, element by
// element. It is what proves the text encoding (FormatVector -> ::vector)
// did not mangle the vector on its way into the column, which no check on
// "a vector exists" can see.
func requireVectorIs(t *testing.T, what string, got storedEmbedding, want []float32) {
	t.Helper()
	if got.vector == nil {
		t.Fatalf("%s: no vector stored", what)
	}
	stored := parseVector(t, what, *got.vector)
	if len(stored) != len(want) {
		t.Fatalf("%s: the stored vector is %d wide, the embedder produced %d", what, len(stored), len(want))
	}
	for i := range stored {
		if diff := stored[i] - want[i]; diff > 1e-6 || diff < -1e-6 {
			t.Fatalf("%s: dimension %d is %v, want %v (the stored vector is not the one the embedder computed)",
				what, i, stored[i], want[i])
		}
	}
}

// TestSearchEmbeddingRecomputeIsIdempotentAndTracksTheModel is acceptance
// criterion "可重建": one command/job recomputes every embedding, running it
// twice changes nothing, and switching the model moves BOTH the metadata and
// the vectors.
func TestSearchEmbeddingRecomputeIsIdempotentAndTracksTheModel(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchEmbeddingTaskID)

	docs := []struct{ ref, title, content string }{
		{"asset:1", "Perovskite solar cell stability", "needle: a stability study of perovskite cells"},
		{"asset:2", "Perovskite degradation", "needle: humidity drives degradation in perovskite films"},
		{"release:1", "Release 1.4.0", "needle: reproducible release of the catalyst dataset"},
	}
	for _, d := range docs {
		seedSearchDocument(t, ctx, pool, d.ref, d.title, d.content, "public", pgtype.UUID{})
	}

	// Batch size 2 over 3 documents: the backlog is drained in more than one
	// batch, so the batching itself is exercised, not just a single pass.
	modelV1 := embeddingModel("v1")
	first := mustEmbeddingWorker(t, pool, mustEmbedder(t, modelV1), embedding.WithBatchSize(2))
	report, err := first.Recompute(ctx)
	if err != nil {
		t.Fatalf("Recompute (v1): %v", err)
	}
	if report.Embedded != 3 || report.Batches != 2 {
		t.Fatalf("Recompute (v1) = %+v, want embedded=3 in 2 batches of 2", report)
	}

	// Every row carries the model's complete identity AND the vector the
	// embedder computes for that row's text.
	v1Vectors := map[string]string{}
	for _, d := range docs {
		got := readEmbedding(t, ctx, pool, d.ref)
		requireProvenance(t, d.ref, got, modelV1)
		want, err := mustEmbedder(t, modelV1).Embed(ctx, []string{embedding.TextFor(d.title, d.content)})
		if err != nil {
			t.Fatalf("compute the expected vector for %s: %v", d.ref, err)
		}
		requireVectorIs(t, d.ref, got, want[0])
		v1Vectors[d.ref] = *got.vector
	}

	// Idempotence, in the strong form: the second run selects nothing, so it
	// writes nothing and calls the embedder for nothing.
	second, err := first.Recompute(ctx)
	if err != nil {
		t.Fatalf("Recompute (v1, second run): %v", err)
	}
	if second.Embedded != 0 || second.Batches != 0 {
		t.Fatalf("the second run did work: %+v, want embedded=0 batches=0", second)
	}
	for _, d := range docs {
		if got := readEmbedding(t, ctx, pool, d.ref); *got.vector != v1Vectors[d.ref] {
			t.Fatalf("%s: the second run changed the stored vector", d.ref)
		}
	}

	// The model changes. This is the half of "可重建" that a recompute which
	// only refills NULLs would silently fail: the vectors are no longer the
	// current model's, so every row is stale again, and BOTH the metadata and
	// the vector must move.
	modelV2 := embeddingModel("v2")
	upgraded := mustEmbeddingWorker(t, pool, mustEmbedder(t, modelV2), embedding.WithBatchSize(2))
	report, err = upgraded.Recompute(ctx)
	if err != nil {
		t.Fatalf("Recompute (v2): %v", err)
	}
	if report.Embedded != 3 {
		t.Fatalf("switching the model left %d document(s) stale, want 0 (%+v)", 3-report.Embedded, report)
	}
	for _, d := range docs {
		got := readEmbedding(t, ctx, pool, d.ref)
		requireProvenance(t, d.ref, got, modelV2)
		if *got.vector == v1Vectors[d.ref] {
			t.Fatalf("%s: the provenance moved to v2 but the vector did not — a vector from a replaced model is a wrong answer that looks like a right one", d.ref)
		}
	}

	// And back: the predicate is symmetric, and the v1 vector is reproduced
	// byte for byte, which is the determinism the whole design rests on.
	report, err = first.Recompute(ctx)
	if err != nil {
		t.Fatalf("Recompute (v1 again): %v", err)
	}
	if report.Embedded != 3 {
		t.Fatalf("switching back to v1 left %d document(s) stale", 3-report.Embedded)
	}
	for _, d := range docs {
		got := readEmbedding(t, ctx, pool, d.ref)
		requireProvenance(t, d.ref, got, modelV1)
		if *got.vector != v1Vectors[d.ref] {
			t.Fatalf("%s: recomputing with v1 produced a different vector than the first v1 run", d.ref)
		}
	}
}

// TestSearchEmbeddingLeavesTheProjectionAlone is the write-path contract this
// task chose (b) for: the embedding job updates the vector and its
// provenance and NOTHING else, so re-projecting a document (T0901's
// UpsertSearchDocument, the projection's only write path) must not erase a
// vector, and embedding a document must not rewrite its content.
func TestSearchEmbeddingLeavesTheProjectionAlone(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchEmbeddingTaskID)
	q := sqlc.New(pool)

	seedSearchDocument(t, ctx, pool, "asset:9", "Original title", "needle original content", "public", pgtype.UUID{})
	model := embeddingModel("v1")
	w := mustEmbeddingWorker(t, pool, mustEmbedder(t, model))
	if report, err := w.Recompute(ctx); err != nil || report.Embedded != 1 {
		t.Fatalf("Recompute = %+v, %v; want embedded=1", report, err)
	}
	before := readEmbedding(t, ctx, pool, "asset:9")

	// The projector writes the document again with new content (a re-projection
	// after the entity changed — the ordinary path, and the reason the
	// document rows and vectors have separate writers).
	if err := q.UpsertSearchDocument(ctx, sqlc.UpsertSearchDocumentParams{
		EntityRef:  "asset:9",
		EntityType: "dataset",
		Visibility: "public",
		Title:      "Renamed title",
		Content:    "needle rewritten content",
		Structured: []byte(`{"kind":"dataset"}`),
	}); err != nil {
		t.Fatalf("re-project the document: %v", err)
	}

	after := readEmbedding(t, ctx, pool, "asset:9")
	if after.isNull() || *after.vector != *before.vector {
		t.Fatalf("re-projecting the document erased or changed its vector:\n before: %s\n after:  %s", before.describe(), after.describe())
	}
	// The document columns are the projector's, and it did write those.
	var title string
	if err := pool.QueryRow(ctx, `SELECT title FROM search_documents WHERE entity_ref = 'asset:9'`).Scan(&title); err != nil {
		t.Fatalf("read the title: %v", err)
	}
	if title != "Renamed title" {
		t.Fatalf("the projection's own write went missing: title = %q", title)
	}

	// The document's text changed, so its vector is now stale: the next
	// recompute selects it — but only because the TEXT is not part of the
	// predicate. It is not: the predicate is the model identity. What this
	// asserts is the documented boundary — a re-projection does not
	// re-embed, and the vector is refreshed by the embedding job's own pass.
	if report, err := w.Recompute(ctx); err != nil || report.Embedded != 0 {
		t.Fatalf("Recompute after re-projection = %+v, %v; want embedded=0 (the predicate is the model identity, not the text)", report, err)
	}
}

// gatedFailingEmbedder is the provider acceptance criterion 1 asks for: it
// fails on every call, and it fails while the batch job is provably holding
// it — Embed signals that it has been entered and then waits for release
// before returning its error. A provider that failed instantly would let a
// test pass without ever having the embedding work in flight, which is
// exactly the shape that reads green while measuring nothing.
type gatedFailingEmbedder struct {
	model embedding.Model
	err   error

	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	released    chan struct{}
	// releaseOnce, not a check-then-close: the gate is opened from the test
	// goroutine and again from a cleanup, so the "already open" test and the
	// close must be one step or two callers can both decide to close.
	releaseOnce sync.Once
}

func newGatedFailingEmbedder(m embedding.Model) *gatedFailingEmbedder {
	return &gatedFailingEmbedder{
		model:    m,
		err:      errors.New("provider is unreachable"),
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

func (g *gatedFailingEmbedder) Model() embedding.Model { return g.model }

func (g *gatedFailingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	g.enteredOnce.Do(func() { close(g.entered) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	g.closeReleased()
	return nil, g.err
}

// releaseGate lets the wedged Embed return. It is idempotent so a test can
// call it on its normal path and a cleanup can call it again safely.
func (g *gatedFailingEmbedder) releaseGate() { g.closeReleased() }

func (g *gatedFailingEmbedder) closeReleased() {
	g.releaseOnce.Do(func() {
		close(g.released)
		close(g.release)
	})
}

func (g *gatedFailingEmbedder) waitEntered(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(timeout):
		t.Fatalf("the embed job never reached the provider within %s", timeout)
	}
}

// TestEmbeddingFailureDoesNotBlockStructuredSearch is acceptance criterion 1:
// a provider that fails on every call must not block structured search.
//
// The trap this test is built around, named in the repository's own words
// (asset_page_test.go: "An empty string is a failure rather than a pass: a
// check that forbade nothing is the shape that reads green while measuring
// nothing"): SearchDocuments does not touch the embedding column at all, so
// "search still answers while embedding fails" is TRUE BY CONSTRUCTION. A
// test that merely ran the query and checked its rows would be green even if
// the whole embedding path were wedged, deleted or blocking. So this test
// does three things a vacuous version would not:
//
//  1. it proves the embedding job is IN FLIGHT, holding the provider, when
//     the search runs (the gate: Embed has been entered and has not
//     returned);
//  2. it proves the search answered BEFORE the provider was released — the
//     search carries a deadline, so a search that waited on the job would
//     fail rather than pass;
//  3. it proves the failure was a real, surfaced failure: the job returns
//     the provider's error and left no partial state behind.
//
// (The mutation that makes this test red is recorded in the task result: a
// batch that holds a lock the search needs — LOCK TABLE search_documents IN
// ACCESS EXCLUSIVE MODE across the provider call — turns the search's
// deadline into the failure.)
func TestEmbeddingFailureDoesNotBlockStructuredSearch(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchEmbeddingTaskID)
	q := sqlc.New(pool)

	// Two public documents the searcher may see, and one private document in
	// a project the searcher's scope does not name: the failing embedding must
	// not become a way around the visibility filter either.
	seedSearchDocument(t, ctx, pool, "pub-1", "Public one", "needle in a public document", "public", pgtype.UUID{})
	seedSearchDocument(t, ctx, pool, "pub-2", "Public two", "another needle, also public", "public", pgtype.UUID{})
	alice := mustUUID(t, ctx, pool, `INSERT INTO users (handle, display_name) VALUES ('t0902-alice','Alice') RETURNING id`)
	acme := mustUUID(t, ctx, pool, `INSERT INTO organizations (slug, name) VALUES ('t0902-acme','Acme') RETURNING id`)
	theirs := mustUUID(t, ctx, pool, `INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ($1,'t0902-theirs','Theirs','p','private',$2) RETURNING id`, acme, alice)
	seedSearchDocument(t, ctx, pool, "priv-1", "Private one", "needle in a private document", "private", theirs)

	provider := newGatedFailingEmbedder(embeddingModel("v1"))
	w := mustEmbeddingWorker(t, pool, provider, embedding.WithBatchSize(10))

	jobCtx, cancelJob := context.WithCancel(context.Background())
	t.Cleanup(cancelJob)
	t.Cleanup(provider.releaseGate)
	jobDone := make(chan error, 1)
	go func() {
		_, err := w.Recompute(jobCtx)
		jobDone <- err
	}()
	// The job is now provably inside the provider, and the provider will not
	// return until this test releases it.
	provider.waitEntered(t, 30*time.Second)

	// The search, on a deadline, while the embedding job is wedged.
	searchCtx, cancelSearch := context.WithTimeout(ctx, 15*time.Second)
	defer cancelSearch()
	started := time.Now()
	rows, err := q.SearchDocuments(searchCtx, sqlc.SearchDocumentsParams{
		Query:             "needle",
		AllowedProjectIds: nil, // empty scope: public only, the fail-closed case
		PageSize:          100,
		PageOffset:        0,
	})
	if err != nil {
		t.Fatalf("structured search failed while embedding was wedged (%s after it started): %v", time.Since(started), err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.EntityRef] = true
	}
	if !got["pub-1"] || !got["pub-2"] {
		t.Fatalf("the search did not return the public documents: %v", got)
	}
	if got["priv-1"] {
		t.Fatalf("the search leaked a private document while embedding was failing: %v", got)
	}
	// The property the deadline is for: nothing had released the provider, so
	// a search that waited on the embedding path would have hit the deadline
	// above instead of returning.
	select {
	case <-provider.released:
		t.Fatalf("the provider was released before the search returned: this run cannot show that the search did not wait for it")
	default:
	}
	select {
	case err := <-jobDone:
		t.Fatalf("the embedding job finished before the search returned (err=%v): the in-flight case was not exercised", err)
	default:
	}

	// Now let the failure happen, and require that it is a real one.
	provider.releaseGate()
	select {
	case err := <-jobDone:
		if err == nil {
			t.Fatalf("the embed job reported success after the provider failed")
		}
		if !strings.Contains(err.Error(), "provider is unreachable") {
			t.Fatalf("the embed job's error does not carry the provider's failure: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the embed job did not finish after the provider was released")
	}

	// A failed batch leaves nothing behind: no vector, and no provenance
	// either (the two travel together, and 00092's CHECK says so).
	for _, ref := range []string{"pub-1", "pub-2", "priv-1"} {
		if got := readEmbedding(t, ctx, pool, ref); !got.isNull() {
			t.Fatalf("%s: a failed batch wrote partial state: %s", ref, got.describe())
		}
	}
}

// mustUUID inserts a row and returns its id (the shape search_access_test.go
// uses for its fixtures).
func mustUUID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		t.Fatalf("seed query failed: %s: %v", strings.Join(strings.Fields(sql), " "), err)
	}
	if !id.Valid {
		t.Fatalf("seed query returned a null uuid: %s", sql)
	}
	return id
}

// TestSearchEmbeddingRejectsTornMetadata pins the migration's invariant: a
// vector without its provenance (and provenance without a vector) must be
// rejected by the database, because such a row is either recomputed forever
// or never recomputed again.
func TestSearchEmbeddingRejectsTornMetadata(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchEmbeddingTaskID)
	seedSearchDocument(t, ctx, pool, "asset:torn", "Torn", "needle", "public", pgtype.UUID{})

	// A vector of the column's own width, so the refusal under test is the
	// provenance rule and not the width check that would fire first on a
	// short probe (a 3-element probe is rejected with "expected 1536
	// dimensions", which would pass this test while measuring the wrong
	// constraint).
	wide := "[" + strings.Repeat("1,", embedding.Dimensions-1) + "1]"
	_, err := pool.Exec(ctx, `UPDATE search_documents SET embedding = $1::vector WHERE entity_ref = 'asset:torn'`, wide)
	if err == nil {
		t.Fatalf("the database accepted a vector with no provenance: every recompute pass would re-embed that row forever")
	}
	if !strings.Contains(err.Error(), "search_documents_embedding_complete") {
		t.Fatalf("the refusal does not name the constraint that refused it: %v", err)
	}

	_, err = pool.Exec(ctx, `UPDATE search_documents
		SET embedding_provider = 'p', embedding_model = 'm', embedding_version = 'v'
		WHERE entity_ref = 'asset:torn'`)
	if err == nil {
		t.Fatalf("the database accepted provenance with no vector: the row would claim an embedding it does not have")
	}
}

// --------------------------------------------------------------------------
// The command

// embedResult is what `post-worker -search-embed` reported on stdout. The
// report is part of what is under test: a recompute that ran and said
// nothing would not be operable, and "embedded=0" must be distinguishable
// from "the command did not run".
type embedResult struct {
	Embedded int
	Batches  int
	Model    string
	Raw      string
}

func runSearchEmbedCommand(t *testing.T, binary, dbURL string) embedResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-search-embed")
	// A scratch cwd: LoadFromCwd may only see the environment this test
	// built, never a developer's .env.<layer> in the repository.
	cmd.Dir = t.TempDir()
	cmd.Env = workerEnv(t, dbURL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("post-worker -search-embed failed: %v\n%s", err, out)
	}
	line := ""
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(l, "search embed: ") {
			line = strings.TrimPrefix(l, "search embed: ")
		}
	}
	if line == "" {
		t.Fatalf("the command reported no embedding run; it must not be a silent no-op:\n%s", out)
	}
	got := embedResult{Raw: line}
	seen := map[string]bool{}
	for _, field := range strings.Fields(line) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			t.Fatalf("unparsable embedding report %q", line)
		}
		switch key {
		case "embedded", "batches":
			n, err := strconv.Atoi(value)
			if err != nil {
				t.Fatalf("embedding report %q: %v", line, err)
			}
			if key == "embedded" {
				got.Embedded = n
			} else {
				got.Batches = n
			}
			seen[key] = true
		case "model":
			got.Model = value
		default:
			t.Fatalf("unexpected embedding report field %q in %q", key, line)
		}
	}
	if !seen["embedded"] || !seen["batches"] || got.Model == "" {
		t.Fatalf("the embedding report is missing a field (want embedded/batches/model): %q", line)
	}
	return got
}

// TestSearchEmbedCommandRecomputesAndIsIdempotent drives the REAL binary —
// the command an operator runs after changing the embedder — and requires
// both halves of "可重建" through it: it fills every vector, and running it
// again does nothing at all.
func TestSearchEmbedCommandRecomputesAndIsIdempotent(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchEmbeddingTaskID)
	for i := 1; i <= 3; i++ {
		seedSearchDocument(t, ctx, pool,
			fmt.Sprintf("asset:cmd-%d", i), fmt.Sprintf("Document %d", i),
			"needle: a document the command must embed", "public", pgtype.UUID{})
	}
	binary := buildPostWorker(t)
	dbURL := databaseURLOf(t, ctx, pool)

	first := runSearchEmbedCommand(t, binary, dbURL)
	if first.Embedded != 3 {
		t.Fatalf("the command embedded %d document(s), want 3 (%q)", first.Embedded, first.Raw)
	}
	if first.Model != embedding.LocalModel.String() {
		t.Fatalf("the command embedded with %q, want the binary's own model %q", first.Model, embedding.LocalModel)
	}
	for i := 1; i <= 3; i++ {
		ref := fmt.Sprintf("asset:cmd-%d", i)
		requireProvenance(t, ref, readEmbedding(t, ctx, pool, ref), embedding.LocalModel)
	}

	second := runSearchEmbedCommand(t, binary, dbURL)
	if second.Embedded != 0 || second.Batches != 0 {
		t.Fatalf("the second run did work: %q, want embedded=0 batches=0", second.Raw)
	}
}

// --------------------------------------------------------------------------
// The worker: the live consumer and the job type

// TestSearchEmbeddingWorkerConsumesTheQueue starts the real worker binary and
// proves both halves of the requirement "batch embedding worker": the
// always-on consumer fills a vector for a document no one asked about, and
// the search.embed job type is registered and handled by the same call.
func TestSearchEmbeddingWorkerConsumesTheQueue(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchEmbeddingTaskID)
	redisServer := startMiniRedis(t)
	binary := buildPostWorker(t)
	dbURL := databaseURLOf(t, ctx, pool)
	proc := startPostWorker(t, binary, dbURL, redisServer.Addr())

	// A document that arrives after the worker started: nothing enqueues
	// anything for it, so only the polling consumer can embed it.
	seedSearchDocument(t, ctx, pool, "asset:live", "Arrived later", "needle: nobody enqueued this", "public", pgtype.UUID{})
	waitFor(t, 60*time.Second, func() string {
		if got := readEmbedding(t, ctx, pool, "asset:live"); got.isNull() {
			return "the worker has not embedded the document it found on its own"
		}
		return ""
	}, func() string { return proc.output() })
	requireProvenance(t, "asset:live", readEmbedding(t, ctx, pool, "asset:live"), embedding.LocalModel)

	// The job type, through the real queue: enqueued by a producer, handled by
	// the same command the flag runs.
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	defer func() { _ = client.Close() }()
	queue := worker.NewRedisQueue(client, "post")
	if err := queue.Enqueue(ctx, worker.Job{
		ID:            "t0902-embed-1",
		Type:          "search.embed",
		CorrelationID: "t0902",
	}); err != nil {
		t.Fatalf("enqueue the search.embed job: %v", err)
	}
	waitFor(t, 60*time.Second, func() string {
		if !strings.Contains(proc.output(), `"job_id":"t0902-embed-1"`) {
			return "the worker has not handled the enqueued search.embed job"
		}
		if !strings.Contains(proc.output(), "search embed job complete") {
			return "the worker handled the job but did not report it as a search embed job"
		}
		return ""
	}, func() string { return proc.output() })

	// The job is idempotent through the queue too: it reports embedded=0,
	// because the consumer above already embedded everything there was.
	if !strings.Contains(proc.output(), `"embedded":0`) {
		t.Errorf("the job did not report embedded=0 after the consumer had already embedded the row:\n%s", proc.output())
	}
}

// failingEmbedder fails every call immediately, with the error a provider
// outage produces.
//
// calls counts the texts it was handed, and it is atomic because the counter
// is the instrument, not decoration: Embed runs on the job loop's goroutine
// while the test's own goroutine polls the value, so a plain int would be a
// data race in the test itself — the one place where a race is least
// forgivable, since a racy instrument can report "never called" (or "called")
// out of a torn read and make the assertion below meaningless.
type failingEmbedder struct {
	model embedding.Model
	err   error
	calls atomic.Int64
}

func (f *failingEmbedder) Model() embedding.Model { return f.model }

func (f *failingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	f.calls.Add(int64(len(texts)))
	return nil, f.err
}

// TestEmbeddingFailureWalksTheQueuesRetryAndDeadLetterPath is the second half
// of the failure story: the job does not invent a retry policy, it returns
// the error to the loop and the loop applies the repository's own
// retry/backoff/dead-letter rules — the same ones every other job type gets.
//
// It drives the REAL loop over a real Redis protocol server with the same
// handler shape cmd/worker registers, and asserts the three observable
// outcomes of that policy: the job runs more than once, it ends in the
// dead-letter list, and the entry records the provider's error for the
// operator.
func TestEmbeddingFailureWalksTheQueuesRetryAndDeadLetterPath(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchEmbeddingTaskID)
	seedSearchDocument(t, ctx, pool, "asset:dead", "Never embedded", "needle: this row stays unembedded", "public", pgtype.UUID{})

	provider := &failingEmbedder{model: embeddingModel("v1"), err: errors.New("provider is unreachable")}
	w := mustEmbeddingWorker(t, pool, provider)

	redisServer := startMiniRedis(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	defer func() { _ = client.Close() }()
	queue := worker.NewRedisQueue(client, "post").WithPollTimeout(time.Second)

	loop := worker.NewLoop(queue, worker.WithLogger(slog.New(slog.DiscardHandler)), worker.WithMaxAttempts(2))
	loop.Register("search.embed", func(ctx context.Context, job worker.Job) error {
		_, err := w.Recompute(ctx)
		return err
	})
	loopCtx, stopLoop := context.WithCancel(context.Background())
	defer stopLoop()
	go func() { _ = loop.Run(loopCtx) }()

	if err := queue.Enqueue(ctx, worker.Job{ID: "t0902-dead-1", Type: "search.embed", CorrelationID: "t0902"}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	var dead worker.Job
	waitFor(t, 60*time.Second, func() string {
		raw, err := client.LIndex(ctx, "post:queue:dead", 0).Result()
		if errors.Is(err, redis.Nil) {
			return "the failing job has not been dead-lettered yet"
		}
		if err != nil {
			t.Fatalf("read the dead-letter list: %v", err)
		}
		if err := json.Unmarshal([]byte(raw), &dead); err != nil {
			t.Fatalf("the dead-letter entry is not a job: %v", err)
		}
		return ""
	}, func() string { return fmt.Sprintf("queue: %v", provider.calls.Load()) })

	if provider.calls.Load() == 0 {
		t.Fatalf("the embedder was never called: the job failed without reaching the provider")
	}
	if dead.Type != "search.embed" || dead.Attempts != 2 {
		t.Fatalf("dead-letter entry = %+v, want type search.embed after 2 attempts", dead)
	}
	if !strings.Contains(dead.Error, "provider is unreachable") {
		t.Fatalf("the dead-letter entry does not carry the provider's failure: %q", dead.Error)
	}

	// The queue is drained (no half-processed job is left) and the failed batch
	// wrote nothing.
	jobs, processing, _, err := queue.Depth(ctx)
	if err != nil {
		t.Fatalf("Depth: %v", err)
	}
	if jobs != 0 || processing != 0 {
		t.Fatalf("the queue was not drained: jobs=%d processing=%d", jobs, processing)
	}
	if got := readEmbedding(t, ctx, pool, "asset:dead"); !got.isNull() {
		t.Fatalf("a dead-lettered job left state behind: %s", got.describe())
	}
}

// TestSearchEmbeddingRefusesASilentWriteFailure drives the case the recompute
// loop's termination argument does NOT cover on its own: a write that is
// accepted by the database but changes no row.
//
// The loop's termination rests on "a row I have written stops matching the
// backlog predicate", which is only true while the write lands. A BEFORE
// UPDATE trigger that returns NULL is PostgreSQL's way of making a write
// ineffective WITHOUT an error: the statement succeeds, reports zero rows, and
// the row stays in the backlog. Without a check on the row count the loop
// selects the same row again, embeds it again and drops it again — forever,
// and in a resident consumer, silently. The batch job therefore compares the
// affected count with the one row entity_ref's primary key allows and returns
// an error, which is what this test requires: an error naming the row, not a
// spin, and not a deadline.
func TestSearchEmbeddingRefusesASilentWriteFailure(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), searchEmbeddingTaskID)
	seedSearchDocument(t, ctx, pool, "asset:silent", "Silent", "needle: never embedded", "public", pgtype.UUID{})

	if _, err := pool.Exec(ctx, `CREATE FUNCTION t0902_skip_embedding_update() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END; $$`); err != nil {
		t.Fatalf("install the trigger function: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TRIGGER t0902_skip_embedding_update
		BEFORE UPDATE ON search_documents FOR EACH ROW
		EXECUTE FUNCTION t0902_skip_embedding_update()`); err != nil {
		t.Fatalf("install the trigger that swallows the update: %v", err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := pool.Exec(dropCtx, `DROP TRIGGER IF EXISTS t0902_skip_embedding_update ON search_documents`); err != nil {
			t.Logf("drop the trigger that swallows the update: %v", err)
		}
	})

	// Prove the trap is armed before trusting what the job does inside it: the
	// statement succeeds and reports zero rows, so a job that only checked
	// "no error" would see nothing wrong.
	tag, err := pool.Exec(ctx, `UPDATE search_documents SET title = title WHERE entity_ref = 'asset:silent'`)
	if err != nil {
		t.Fatalf("the trigger turned the swallowed update into an error, which is not the case under test: %v", err)
	}
	if n := tag.RowsAffected(); n != 0 {
		t.Fatalf("the trigger did not swallow the update: %d row(s) affected, so this test would be measuring nothing", n)
	}

	w := mustEmbeddingWorker(t, pool, mustEmbedder(t, embeddingModel("v1")))

	// A bounded context is the difference between a failing test and a hung
	// one: if the guard is removed the job spins, and the deadline turns that
	// spin into a definite failure whose message is recognisable below (a spin
	// is not reported as the guard firing).
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	report, err := w.Recompute(bounded)

	if err == nil {
		t.Fatalf("the recompute reported success (embedded=%d batches=%d) while every write was being swallowed: a resident consumer would spin on that row forever",
			report.Embedded, report.Batches)
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("the recompute ran until its deadline instead of reporting the write that did not land: %v", err)
	}
	if !strings.Contains(err.Error(), "asset:silent") {
		t.Fatalf("the failure does not name the row whose write was swallowed: %v", err)
	}
	if !strings.Contains(err.Error(), "changed 0 row") {
		t.Fatalf("the failure does not say the update changed no rows, so it is not the bounded-write guard reporting: %v", err)
	}
	if got := readEmbedding(t, ctx, pool, "asset:silent"); !got.isNull() {
		t.Fatalf("the swallowed write left an embedding behind: %s", got.describe())
	}
}
