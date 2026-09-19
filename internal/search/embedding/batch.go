package embedding

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// DefaultBatchSize is how many documents one batch embeds.
//
// A batch is bounded by the WRITE side, not by the provider: a 1536-wide
// vector is roughly 12 KB of text on the wire, so 32 documents is a ~400 KB
// statement batch — small enough to keep a batch's memory flat and to make a
// failed batch cheap to redo, large enough that a backlog of a few thousand
// documents is a few dozen round trips rather than a few thousand.
const DefaultBatchSize = 32

// DefaultInterval is the pause between passes in Run, matching the
// projection's and the other consumers' one second: the embedding backlog is
// fed by a consumer of the same event log, so the same cadence keeps a
// document's vector within a poll of its row.
const DefaultInterval = time.Second

// Worker brings search_documents.embedding up to date with a model.
//
// It is the projection's second writer and it owns exactly four columns:
// embedding and its provenance (embedding_provider, embedding_model,
// embedding_version). It never writes entity_ref, entity_type, visibility,
// project_id, title, content, structured or updated_at — those belong to
// T0901's projection (internal/search/projector.go), and the split is what
// makes both writers idempotent: recomputing vectors cannot rewrite a
// document, and re-projecting a document cannot erase a vector.
//
// # Idempotence
//
// The unit of work is "the documents whose stored vector is not the current
// model's" (SearchDocumentsNeedingEmbedding), and the write is the
// unconditional assignment of the vector and its provenance to a row that
// was selected by that predicate. A second run therefore selects nothing and
// writes nothing — it is not merely that re-running produces the same values
// (it does), it is that it produces no write and no provider call at all.
//
// # Failure
//
// A batch is atomic and its failure is surfaced, never swallowed: if the
// embedder returns an error, nothing is written for that batch and the error
// is returned to the caller. Callers that run inside the job queue return it
// to the loop, which applies the repository's existing policy — retry with
// capped exponential backoff, then dead-letter with the error recorded
// (internal/worker/worker.go) — rather than a policy invented here. The
// always-on Run never exits on failure, like every other consumer: a
// provider outage is retried, and only context cancellation stops it.
type Worker struct {
	pool      *pgxpool.Pool
	queries   *sqlc.Queries
	embedder  Embedder
	batchSize int
	interval  time.Duration
	log       *slog.Logger
}

// WorkerOption tunes a Worker.
type WorkerOption func(*Worker)

// WithLogger sets the worker's logger (default slog.Default()).
func WithLogger(log *slog.Logger) WorkerOption {
	return func(w *Worker) { w.log = log }
}

// WithBatchSize sets the documents one batch embeds (default
// DefaultBatchSize).
func WithBatchSize(n int) WorkerOption {
	return func(w *Worker) { w.batchSize = n }
}

// WithInterval sets the pause between passes in Run (default
// DefaultInterval). It has no effect on RunOnce or Recompute, which the
// caller drives.
func WithInterval(d time.Duration) WorkerOption {
	return func(w *Worker) { w.interval = d }
}

// NewWorker builds the batch job over pool, embedding through emb.
//
// It fails closed on a nil embedder and on an embedder whose model identity
// is incomplete: both would produce rows whose provenance cannot distinguish
// a model change, which is a state every later recompute would be unable to
// repair (the row would look current forever). The pool may be lazy
// (persistence.OpenLazy): like every other consumer, the job retries while
// PostgreSQL is down.
func NewWorker(pool *pgxpool.Pool, emb Embedder, opts ...WorkerOption) (*Worker, error) {
	if pool == nil {
		return nil, errors.New("embedding: a batch worker needs a database pool")
	}
	if emb == nil {
		return nil, errors.New("embedding: a batch worker needs an embedder")
	}
	if err := emb.Model().Validate(); err != nil {
		return nil, err
	}
	w := &Worker{
		pool:      pool,
		queries:   sqlc.New(pool),
		embedder:  emb,
		batchSize: DefaultBatchSize,
		interval:  DefaultInterval,
		log:       slog.Default(),
	}
	for _, opt := range opts {
		opt(w)
	}
	if w.batchSize <= 0 {
		return nil, fmt.Errorf("embedding: batch size must be positive, got %d", w.batchSize)
	}
	if w.interval <= 0 {
		return nil, fmt.Errorf("embedding: interval must be positive, got %s", w.interval)
	}
	return w, nil
}

// Model returns the identity this worker writes to every row it embeds.
func (w *Worker) Model() Model { return w.embedder.Model() }

// Report is what one RunOnce or one Recompute did, so a caller (a test, an
// operator's one-shot command) can assert on the outcome rather than scrape
// a log.
type Report struct {
	// Batches is how many batches embedded at least one document. A pass
	// that found nothing to do is zero, not one: "there was no work" and
	// "there was a batch of work" must not look the same in a report.
	Batches int
	// Embedded is how many documents were written.
	Embedded int
}

// String renders the report as "embedded=N batches=N" for a command's
// one-line stdout summary.
func (r Report) String() string {
	return fmt.Sprintf("embedded=%d batches=%d", r.Embedded, r.Batches)
}

// RunOnce embeds at most one batch of documents and returns what it did.
//
// The three steps are deliberately ordered read -> embed -> write, with NO
// transaction open while the embedder runs. That is not an optimisation: a
// transaction held across a provider call holds a database connection for
// the duration of an outbound request, and (with any future provider) a
// failing provider would then hold locks on rows that the rest of the
// platform is reading. Embedding first, writing after, in one transaction of
// its own, means the batch is all-or-nothing on the write side and leaves
// nothing open while it waits.
func (w *Worker) RunOnce(ctx context.Context) (Report, error) {
	model := w.embedder.Model()
	rows, err := w.queries.SearchDocumentsNeedingEmbedding(ctx, sqlc.SearchDocumentsNeedingEmbeddingParams{
		EmbeddingProvider: model.Provider,
		EmbeddingModel:    model.Name,
		EmbeddingVersion:  model.Version,
		BatchSize:         int32(w.batchSize),
	})
	if err != nil {
		return Report{}, fmt.Errorf("embedding: select the backlog: %w", err)
	}
	if len(rows) == 0 {
		return Report{}, nil
	}

	texts := make([]string, len(rows))
	for i, row := range rows {
		texts[i] = TextFor(row.Title, row.Content)
	}
	vectors, err := w.embedder.Embed(ctx, texts)
	if err != nil {
		// The batch's rows stay exactly as they were: nothing is written
		// before every vector of the batch is in hand.
		return Report{}, fmt.Errorf("embedding: %s: embed %d document(s): %w",
			model, len(rows), err)
	}
	if err := checkVectors(model, rows, vectors); err != nil {
		return Report{}, err
	}

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("embedding: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	q := w.queries.WithTx(tx)
	for i, row := range rows {
		affected, err := q.UpdateSearchDocumentEmbedding(ctx, sqlc.UpdateSearchDocumentEmbeddingParams{
			EntityRef:         row.EntityRef,
			Embedding:         FormatVector(vectors[i]),
			EmbeddingProvider: model.Provider,
			EmbeddingModel:    model.Name,
			EmbeddingVersion:  model.Version,
		})
		if err != nil {
			return Report{}, fmt.Errorf("embedding: write %s: %w", row.EntityRef, err)
		}
		// The row count is what bounds Recompute. Its termination argument is
		// "a row I have written no longer matches the backlog predicate", and
		// that argument holds only while the write actually lands. A write
		// that changes no row (a BEFORE UPDATE trigger returning NULL, a
		// WHERE that stopped matching, a renamed column) leaves the row in
		// the backlog, so the next pass selects it again — forever, in a
		// resident consumer, silently. entity_ref is the primary key, so
		// exactly one row is the only correct outcome; anything else is
		// reported instead of assumed, and returned as an error so it takes
		// the queue's retry/dead-letter path rather than being swallowed.
		if affected != 1 {
			return Report{}, fmt.Errorf(
				"embedding: write %s: the update changed %d row(s), want exactly 1: the stored vector would not become the current model's, so this document would be selected by every later pass",
				row.EntityRef, affected)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Report{}, fmt.Errorf("embedding: commit: %w", err)
	}
	w.log.Info("search: embedded documents",
		"embedded", len(rows), "model", model.String(), "batch_size", w.batchSize)
	return Report{Batches: 1, Embedded: len(rows)}, nil
}

// Recompute embeds every document that needs it and returns when none is
// left. It is the rebuild of the embedding half of the projection: the
// command `post-worker -search-embed` and the search.embed job type are both
// this call.
//
// It terminates because every iteration makes progress: a batch either
// selected no rows (done), or wrote the current model's provenance to exactly
// the rows it selected — and a row that carries the current model's vector is
// no longer selected by the very predicate that selected it. Running it twice
// is therefore the same as running it once, and the second run reports
// embedded=0 with no provider call.
//
// The termination argument rests on the write landing, so it is not trusted
// silently: RunOnce fails the batch when its update changes anything other
// than exactly one row, and that error ends this loop. A write that quietly
// stopped landing would otherwise make this function — and the resident
// consumer built on it — spin on the same rows forever
// (TestSearchEmbeddingRefusesASilentWriteFailure drives that case with a
// trigger that skips the update).
func (w *Worker) Recompute(ctx context.Context) (Report, error) {
	var total Report
	for {
		report, err := w.RunOnce(ctx)
		if err != nil {
			return total, err
		}
		total.Batches += report.Batches
		total.Embedded += report.Embedded
		if report.Embedded == 0 {
			return total, nil
		}
	}
}

// Run keeps the embedding column up to date until ctx is cancelled: it
// drains the backlog, then waits one interval and drains what arrived since.
//
// Like the dispatcher and the other consumers, the loop never exits on a
// failure — a provider or database outage is logged and retried on the next
// interval, and only ctx cancellation ends it. The error is NOT swallowed
// into silence: it is logged on every pass, and the job-type path returns it
// to the queue's retry/backoff/dead-letter machinery instead.
func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if _, err := w.Recompute(ctx); err != nil && ctx.Err() == nil {
			w.log.Error("search: embedding pass failed",
				"error", err, "model", w.embedder.Model().String())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// checkVectors enforces the port's contract on what an embedder returned,
// naming the provider and the offending row.
//
// It is a guard, not a formality: a provider that returns the wrong number of
// vectors, or vectors of the wrong width, would otherwise be caught by
// PostgreSQL as a type or cardinality error with nothing in it about which
// embedder produced it — and, worse, a short result could be silently paired
// with the wrong rows if the caller zipped by index without checking. The
// non-finite check exists because pgvector rejects NaN and infinity: a
// provider that produces one has produced a vector POST cannot store, and
// saying so in the provider's own terms is the difference between a dead
// letter an operator can act on and a parser error.
func checkVectors(model Model, rows []sqlc.SearchDocumentsNeedingEmbeddingRow, vectors [][]float32) error {
	if len(vectors) != len(rows) {
		return fmt.Errorf("embedding: %s: asked for %d vector(s), got %d",
			model, len(rows), len(vectors))
	}
	for i, vec := range vectors {
		if len(vec) != Dimensions {
			return fmt.Errorf("embedding: %s: %s: vector is %d wide, the column is %d",
				model, rows[i].EntityRef, len(vec), Dimensions)
		}
		for _, f := range vec {
			if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
				return fmt.Errorf("embedding: %s: %s: vector holds a non-finite value (%v)",
					model, rows[i].EntityRef, f)
			}
		}
	}
	return nil
}
