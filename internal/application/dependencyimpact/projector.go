package dependencyimpact

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Projector drives the dependency impact analysis as a background consumer
// of the published event log, in the shape the Contribution Ledger
// projection established (internal/application/contribution/ledger.go):
// repeated passes over a store that owns the candidate scan and the
// idempotent write, a WARN that is stated once per standing fact, and a
// quiet pass that says nothing.
//
// # Why it is a projector and not an endpoint
//
// docs/19 §3's 「上游变更触发 impact analysis」 has a subject and a verb but
// no user: what triggers the analysis is that an upstream change HAPPENED,
// and the thing that knows a change happened is the log that recorded it.
// An HTTP route would change the trigger to "somebody remembered to press
// it", which is a different feature with the same name — so this package
// adds no route, and a test asserts that cmd/api registers no impact path.
//
// Where it runs: cmd/worker, as the eighth background consumer of the
// shared pool (docs/20 §"Go worker" names impact analysis in the worker's
// job list beside event delivery and the digest). It joins the same wait
// group as the outbox dispatcher and the other consumers, so it stops on
// the root context's cancellation, and a failed pass is retried forever
// like every other dependency outage rather than exiting the process.
//
// # Its candidate set is the log, and it drains
//
// The pass reads trigger events from research_events that have no alert of
// their own yet, oldest first. The one trap in that shape is a candidate
// that yields NOTHING and therefore never stops being a candidate: it
// would occupy the batch window forever and starve every newer trigger.
// The store's scan closes it at the source by requiring the subject to
// have at least one dependent before a trigger is picked up, so every
// candidate produces at least one alert and leaves the window. What that
// excludes is not silent: the projector reports the trigger-type events
// whose subject could not be resolved (Batch.Unresolvable), and a subject
// with no dependents has nothing to analyse by construction.
type Projector struct {
	store     Store
	log       *slog.Logger
	batchSize int
	interval  time.Duration

	// mu guards warned, the set of facts already reported at WARN. Run is
	// a single goroutine, but RunOnce is exported and a caller (a test, an
	// operator's one-shot) may drive it from several.
	mu     sync.Mutex
	warned map[string]bool
}

// DefaultBatchSize is how many trigger events one pass analyses.
const DefaultBatchSize = 200

// DefaultInterval is the pause between passes in Run. The analysis is a
// consumer of the log, not a hot loop: its backlog is bounded by the
// changes the platform actually makes, and it runs behind the outbox
// publisher and the dispatcher that put those rows in the log.
const DefaultInterval = 30 * time.Second

// ProjectorOption tunes a Projector.
type ProjectorOption func(*Projector)

// WithLogger sets the projector's logger (default slog.Default()).
func WithLogger(log *slog.Logger) ProjectorOption {
	return func(p *Projector) { p.log = log }
}

// WithBatchSize sets the triggers one pass analyses (default
// DefaultBatchSize).
func WithBatchSize(n int) ProjectorOption {
	return func(p *Projector) { p.batchSize = n }
}

// WithInterval sets the pause between passes in Run (default
// DefaultInterval). It has no effect on RunOnce, which the caller drives.
func WithInterval(d time.Duration) ProjectorOption {
	return func(p *Projector) { p.interval = d }
}

// NewProjector wires the projector on store.
func NewProjector(store Store, opts ...ProjectorOption) *Projector {
	p := &Projector{
		store:     store,
		log:       slog.Default(),
		batchSize: DefaultBatchSize,
		interval:  DefaultInterval,
		warned:    map[string]bool{},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// RunOnce runs one analysis pass and reports it. The returned batch is the
// pass's own outcome — including the coverage counts — so a caller can
// assert on it rather than scrape the log.
func (p *Projector) RunOnce(ctx context.Context) (Batch, error) {
	batch, err := p.store.AnalyzeBatch(ctx, p.batchSize)
	if err != nil {
		return Batch{}, fmt.Errorf("dependencyimpact: analysis pass: %w", err)
	}
	p.report(batch)
	return batch, nil
}

// Run analyses until ctx is cancelled: one pass immediately (the backlog
// built while nothing was running drains at once), then one pass per
// interval. A transient failure never ends the loop — a down database is
// retried, and the alerts are idempotent, so a retry after a partial pass
// re-derives rather than doubles.
func (p *Projector) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		if _, err := p.RunOnce(ctx); err != nil && ctx.Err() == nil {
			p.log.Error("dependencyimpact: analysis pass failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// report logs one pass: the pass's own numbers first, then the two facts
// that are the reason this projector exists rather than a bare SQL loop —
// an unresolvable trigger (a type this analysis claims to cover and cannot
// act on) and a walk that hit the depth cap (a result that may be
// truncated). Both are stated once per standing fact, never once per poll,
// and neither is ever silent.
func (p *Projector) report(b Batch) {
	for _, c := range b.Unresolvable {
		if p.firstTime("unresolvable:" + c.EventType) {
			p.log.Warn("dependencyimpact: trigger event names no resolvable subject — counted, never analysed",
				"event_type", c.EventType, "events_in_log", c.Count,
				"trigger_table", "internal/application/dependencyimpact/trigger.go")
		}
	}
	if b.HitDepthCap > 0 && p.firstTime("depthcap") {
		p.log.Warn("dependencyimpact: a walk reached the depth cap — the impact set may be truncated",
			"cap", MaxHops, "walks_at_cap", b.HitDepthCap)
	}
	if b.Candidates == 0 {
		// A quiet pass says nothing: repeating the standing numbers every
		// interval would bury the passes that did something.
		return
	}
	p.log.Info("dependencyimpact: analysis pass",
		"candidates", b.Candidates, "alerts", b.Emitted, "duplicates", b.Duplicates,
		"pending", b.Pending, "walks_at_depth_cap", b.HitDepthCap)
}

// firstTime reports whether key is being reported for the first time.
func (p *Projector) firstTime(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.warned[key] {
		return false
	}
	p.warned[key] = true
	return true
}
