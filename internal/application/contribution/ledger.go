package contribution

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lichman0405/post/internal/contribution"
)

// LedgerProjector drives the Contribution Ledger projection (docs/13 §1):
// it turns the append-only domain event log (research_events) into
// contribution ledger rows (contribution_events) by repeatedly asking the
// store for the next batch of unprojected events.
//
// The projector decides nothing about what a ledger row contains — that is
// internal/contribution/ledger.go's mapping table, in one readable place.
// What it owns is the part a decision table cannot express:
//
//   - COVERAGE VISIBILITY. An event whose type the mapping table does not
//     cover produces no ledger row, and that must never be a silent hole.
//     Every pass reports the unmapped types with their counts (the store
//     counts them; a per-pass delta would read as zero while the gap
//     stayed), and each type is logged once at WARN when it is first seen
//     — a standing fact gets stated once, not once per poll.
//   - THE RETRY LOOP. A failed pass is retried like every other dependency
//     outage and never ends the loop; the projection's idempotency
//     (migration 00087's partial unique index) is what makes running it
//     again — after a crash, a redeploy or by accident — a no-op instead of
//     a duplicate.
//
// WHERE THIS RUNS, honestly: the projector is wired nowhere yet. The
// process that owns background consumers is cmd/worker, which is outside
// T0807's write scope, and unilaterally moving worker responsibilities into
// cmd/api is not this task's decision to make. The projector is therefore
// delivered as a service with Run/RunOnce and driven, for now, by its
// tests; wiring it into cmd/worker is the remaining one-line composition
// step and is reported as such.
type LedgerProjector struct {
	store     LedgerPort
	log       *slog.Logger
	batchSize int
	interval  time.Duration

	// mu guards warned: the set of "kind:event_type" pairs already reported
	// at WARN. Run is a single goroutine, but RunOnce is exported and a
	// caller may drive it from several.
	mu     sync.Mutex
	warned map[string]bool
}

// LedgerPort is the persistence port the projector drives. The production
// adapter is *contribution.LedgerStore over PostgreSQL; the port is one
// method wide because the projection's whole decision surface is the
// mapping table (internal/contribution/ledger.go) — the adapter executes
// it and owns the candidate scan, the affiliation resolution and the
// idempotent insert.
type LedgerPort interface {
	// ProjectBatch runs one projection pass over at most limit unprojected
	// events. It is idempotent: events that already have a ledger row are
	// reported as duplicates, never written twice.
	ProjectBatch(ctx context.Context, limit int) (contribution.LedgerBatch, error)
}

// DefaultLedgerBatchSize is how many events one projection pass projects.
const DefaultLedgerBatchSize = 500

// DefaultLedgerInterval is the pause between passes in Run. The projection
// is a consumer of the event log, not a hot loop: it is expected to run
// behind the outbox publisher, and its backlog is bounded by the events the
// log receives.
const DefaultLedgerInterval = 15 * time.Second

// LedgerOption tunes a LedgerProjector.
type LedgerOption func(*LedgerProjector)

// WithLedgerLogger sets the projector's logger (default slog.Default()).
func WithLedgerLogger(log *slog.Logger) LedgerOption {
	return func(p *LedgerProjector) { p.log = log }
}

// WithLedgerBatchSize sets the events one pass projects (default
// DefaultLedgerBatchSize).
func WithLedgerBatchSize(n int) LedgerOption {
	return func(p *LedgerProjector) { p.batchSize = n }
}

// WithLedgerInterval sets the pause between passes in Run (default
// DefaultLedgerInterval). It has no effect on RunOnce, which a caller
// drives itself.
func WithLedgerInterval(d time.Duration) LedgerOption {
	return func(p *LedgerProjector) { p.interval = d }
}

// NewLedgerProjector wires the projector on store.
func NewLedgerProjector(store LedgerPort, opts ...LedgerOption) *LedgerProjector {
	p := &LedgerProjector{
		store:     store,
		log:       slog.Default(),
		batchSize: DefaultLedgerBatchSize,
		interval:  DefaultLedgerInterval,
		warned:    map[string]bool{},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// RunOnce runs one projection pass and reports it. The returned batch is
// the pass's own outcome, including the log's coverage counts, so a caller
// (a test, an operator's one-shot command) can assert on it rather than
// scrape the log.
func (p *LedgerProjector) RunOnce(ctx context.Context) (contribution.LedgerBatch, error) {
	batch, err := p.store.ProjectBatch(ctx, p.batchSize)
	if err != nil {
		return contribution.LedgerBatch{}, fmt.Errorf("contribution: ledger projection pass: %w", err)
	}
	p.report(batch)
	return batch, nil
}

// Run projects until ctx is cancelled: one pass immediately (the backlog
// built while nothing was running drains at once), then one pass per
// interval. Like the outbox dispatcher, the loop never exits on a transient
// failure — a down database is retried; only ctx cancellation ends it.
func (p *LedgerProjector) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		if _, err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
			p.log.Error("contribution: ledger projection pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// report logs one pass: the projection's own numbers, then the coverage
// facts that are the reason this projector exists rather than a bare SQL
// loop.
func (p *LedgerProjector) report(b contribution.LedgerBatch) {
	for _, c := range b.Unmapped {
		if p.firstTime("unmapped:" + c.EventType) {
			p.log.Warn("contribution: event type has no ledger mapping — counted, never projected",
				"event_type", c.EventType, "events_in_log", c.Count,
				"mapping_table", "internal/contribution/ledger.go")
		}
	}
	for _, c := range b.Unattributable {
		if p.firstTime("unattributable:" + c.EventType) {
			p.log.Warn("contribution: mapped event carries no actor — it cannot become a ledger row",
				"event_type", c.EventType, "events_in_log", c.Count)
		}
	}
	if b.Candidates == 0 {
		// A quiet pass says nothing: the numbers below are the log's
		// standing state and repeating them every interval would bury the
		// passes that actually did something.
		return
	}
	p.log.Info("contribution: ledger projection pass",
		"candidates", b.Candidates, "projected", b.Projected,
		"duplicates", b.Duplicates, "pending", b.Pending,
		"unmapped_types", len(b.Unmapped), "unattributable_types", len(b.Unattributable))
}

// firstTime reports whether key is being reported for the first time.
func (p *LedgerProjector) firstTime(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.warned[key] {
		return false
	}
	p.warned[key] = true
	return true
}

var _ LedgerPort = (*contribution.LedgerStore)(nil)
