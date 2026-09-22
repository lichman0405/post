package retrieval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/embedding"
	"github.com/lichman0405/post/internal/search/planner"
)

// RefKindObjectVersion is the ref kind of a scientific object version
// reached by traversal. It is NOT one of the projection's entity types: a
// scientific object version is not a search_documents row, and giving it the
// projection's kind would claim it is one (search.EntityRef's kind position
// is the vocabulary the read query selects on). The object's own scientific
// type travels in Candidate.ObjectType instead, so a citation can still say
// "this is a claim".
const RefKindObjectVersion = "object_version"

// The defaults. They are small on purpose: retrieval's output is a
// candidate set an answer layer CITES, not a result list a person scrolls,
// and a citation list that runs to hundreds is not a citation list.
const (
	// DefaultPerSignal is how many rows one signal may contribute.
	DefaultPerSignal = 20
	// DefaultMaxCandidates is the size of the returned candidate list.
	DefaultMaxCandidates = 20
	// DefaultDepth is the traversal depth: one hop. docs/14 §2 asks for a
	// graph traversal, not for a neighbourhood dump, and one hop is what
	// reaches the objects a recalled document is directly about (a
	// finding's claims, a claim's material).
	DefaultDepth = 1
	// DefaultSeeds is how many recalled documents seed the traversal. It is
	// smaller than the candidate cap because a traversal from every
	// candidate would make the graph signal dominate the fusion by count.
	DefaultSeeds = 5
	// MaxDepth is the traversal's hard ceiling, mirroring
	// rsg.MaxQueryDepth's role on the RSG query surface.
	MaxDepth = 2
	// MaxPerSignal and MaxCandidates bound what a caller may ask for.
	MaxPerSignal    = 200
	MaxCandidates   = 200
	MaxSeeds        = 50
	maxQueryRunes   = 2000
	maxEntityTypes  = 8
	rrfK            = 60
	signalSeparator = "+"
)

// The retrieval's sentinels.
var (
	// ErrNoScope: the caller has no resolved scope. It wraps
	// search.ErrNoActor so a caller that already handles the scope layer's
	// refusal handles this one too — there is one reason and one error for
	// it.
	ErrNoScope = fmt.Errorf("retrieval: %w", search.ErrNoActor)
	// ErrNoQuery: there is nothing to retrieve for (an empty question and
	// no facet filter).
	ErrNoQuery = errors.New("retrieval: no query and no filters")
	// ErrUnknownEntityType: the plan named an entity type the projection
	// does not have. It is refused rather than passed to SQL: an unknown
	// type can only ever match nothing, and silently returning no
	// candidates for a plan the planner accepted would hide the defect
	// where it happened.
	ErrUnknownEntityType = errors.New("retrieval: unknown entity type")
	// ErrBadFilter: the structured filter is not a JSON object. A filter
	// that cannot be a containment is refused for the same reason.
	ErrBadFilter = errors.New("retrieval: structured filter must be a JSON object")
	// ErrTooLong: a limit outside the accepted range.
	ErrTooLong = errors.New("retrieval: limit out of range")
	// ErrStore: a read failed. The caller fails the search; a partial
	// candidate set is never returned, because "we could not read some of
	// the corpus" and "the corpus has nothing" are different answers and a
	// searcher cannot tell them apart in a result list.
	ErrStore = errors.New("retrieval: store")
	// ErrEmbedding: the embedder could not embed the question, so the
	// vector signal did not run.
	ErrEmbedding = errors.New("retrieval: embed query")
)

// Limits bounds one retrieval. The zero Limits is not "no limits" — it is
// normalized to the defaults, so a caller that forgets to set them gets a
// bounded search rather than an unbounded one.
type Limits struct {
	// PerSignal is the rows each signal may contribute (DefaultPerSignal).
	PerSignal int
	// MaxCandidates is the size of the returned list (DefaultMaxCandidates).
	MaxCandidates int
	// Depth is the traversal depth, 0..MaxDepth (DefaultDepth). Zero means
	// the default, like the other three — a bound is not a switch, and the
	// traversal is not optional: it is the graph half of the pipeline
	// (docs/14 §2), and what a caller controls is how far it walks. A
	// retrieval with no seeds reports the graph signal as skipped
	// (SkippedNoSeeds) whatever the depth is.
	Depth int
	// Seeds is how many recalled documents seed the traversal
	// (DefaultSeeds).
	Seeds int
}

// normalize fills the defaults and refuses a limit outside the accepted
// range. Silence is not an option for a negative or absurd value: it would
// either mean "no limit" (unbounded work) or "nothing" (an empty answer),
// and neither is what the caller wrote.
//
// Zero means "use the default" for all four, deliberately and uniformly: a
// caller that filled in one field and left the rest gets a bounded search
// rather than a mix of defaults and absences, and there is no field whose
// zero value means something a caller would not guess (MaxDepth would be the
// one trap, so 0 is normalized away before the range check reads it).
func (l Limits) normalize() (Limits, error) {
	if l.PerSignal == 0 {
		l.PerSignal = DefaultPerSignal
	}
	if l.MaxCandidates == 0 {
		l.MaxCandidates = DefaultMaxCandidates
	}
	if l.Seeds == 0 {
		l.Seeds = DefaultSeeds
	}
	if l.Depth == 0 {
		l.Depth = DefaultDepth
	}
	if l.PerSignal < 0 || l.PerSignal > MaxPerSignal {
		return Limits{}, fmt.Errorf("%w: per_signal %d, want 1..%d", ErrTooLong, l.PerSignal, MaxPerSignal)
	}
	if l.MaxCandidates < 0 || l.MaxCandidates > MaxCandidates {
		return Limits{}, fmt.Errorf("%w: max_candidates %d, want 1..%d", ErrTooLong, l.MaxCandidates, MaxCandidates)
	}
	if l.Seeds < 0 || l.Seeds > MaxSeeds {
		return Limits{}, fmt.Errorf("%w: seeds %d, want 1..%d", ErrTooLong, l.Seeds, MaxSeeds)
	}
	if l.Depth < 0 || l.Depth > MaxDepth {
		return Limits{}, fmt.Errorf("%w: depth %d, want 1..%d", ErrTooLong, l.Depth, MaxDepth)
	}
	return l, nil
}

// Request is one retrieval: what to look for, and how much of it.
//
// It is deliberately NOT a planner.Document. The retrieval is reachable
// without a plan — the planner falls back to structured results whenever the
// model cannot be trusted (docs/27 §SLO, docs/32), and a fallback that could
// not retrieve would be a fallback to nothing — so the two are separate
// types joined by RequestFromPlan, which is the ONE place the plan's
// vocabulary becomes a retrieval's.
type Request struct {
	// Query is the question, verbatim (planner.Plan.Query — in the fallback
	// case it is the only thing there is).
	Query string
	// EntityTypes narrows recall to the projection's entity types, from
	// the plan's target_object. Nil means no narrowing.
	EntityTypes []string
	// PublicOnly narrows recall to rows the read query returns to anybody,
	// from the plan's visibility item. It can only ever remove rows.
	PublicOnly bool
	// Facets is the caller's structured filter (specs/api POST /search:
	// `filters`), a JSON object matched against a row's facets as jsonb
	// containment. Nil means no facet filter — and no facet signal.
	Facets []byte
	// Limits bounds the retrieval.
	Limits Limits
}

// normalize validates the request and fills the defaults.
func (r Request) normalize() (Request, error) {
	r.Query = strings.TrimSpace(r.Query)
	if r.Query == "" && len(r.Facets) == 0 {
		return Request{}, ErrNoQuery
	}
	if len([]rune(r.Query)) > maxQueryRunes {
		return Request{}, fmt.Errorf("%w: query is %d runes, want at most %d",
			ErrTooLong, len([]rune(r.Query)), maxQueryRunes)
	}
	if len(r.EntityTypes) > maxEntityTypes {
		return Request{}, fmt.Errorf("%w: %d entity types, want at most %d",
			ErrTooLong, len(r.EntityTypes), maxEntityTypes)
	}
	known := make(map[string]bool, len(search.EntityTypes()))
	for _, t := range search.EntityTypes() {
		known[t] = true
	}
	seen := make(map[string]bool, len(r.EntityTypes))
	out := make([]string, 0, len(r.EntityTypes))
	for _, t := range r.EntityTypes {
		if !known[t] {
			return Request{}, fmt.Errorf("%w: %q", ErrUnknownEntityType, t)
		}
		if seen[t] {
			// A duplicate would not change the answer, but the SQL
			// parameter's text form would then depend on the caller's
			// argument order; the narrowing is a SET.
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	if len(out) == 0 {
		// "No narrowing" has to be spelled nil, not an empty slice: the
		// query's guard is `@entity_types::text[] IS NULL`, and an empty
		// ARRAY is not NULL, so an empty list there means "match an entity
		// type in the empty set" — no rows at all. See SQLStore.noNarrowing,
		// which is the second half of the same fact.
		out = nil
	}
	r.EntityTypes = out
	if len(r.Facets) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(r.Facets, &obj); err != nil {
			return Request{}, fmt.Errorf("%w: %v", ErrBadFilter, err)
		}
		if obj == nil {
			return Request{}, fmt.Errorf("%w: null", ErrBadFilter)
		}
	}
	limits, err := r.Limits.normalize()
	if err != nil {
		return Request{}, err
	}
	r.Limits = limits
	return r, nil
}

// RequestFromPlan translates a planner plan into a retrieval request.
//
// It is the whole of the plan's influence on recall, and it is deliberately
// one-directional: the plan may NARROW (which entity kinds, which facet
// filter, public-only) and may never widen — the scope, which is the only
// thing that grants anything, is not the plan's to touch and is not a
// parameter of this function. docs/54 #7 ("Search LLM 引用未授权 entity
// id") is the scenario this shape exists for: a plan that has been talked
// into naming a kind, a filter or a visibility can still only ever ask for
// less than the scope already allows.
//
// A plan that fell back carries no document, and it produces the same
// request as a plan whose document named nothing: the question alone. That
// is docs/27 §SLO's "超时提供 structured results fallback" — the fallback is
// a retrieval, not an error.
//
// filters is the CALLER's facet filter (specs/api POST /search's `filters`),
// which the plan does not carry: the plan's vocabulary is docs/14 §2's eight
// items and has no facet field, and a model-authored filter would be a model
// choosing what to read. They are passed through unchanged.
func RequestFromPlan(plan planner.Plan, filters []byte, limits Limits) Request {
	req := Request{Query: plan.Query, Facets: filters, Limits: limits}
	doc := plan.Document
	if doc == nil {
		return req
	}
	req.EntityTypes = doc.TargetObject.EntityTypes
	req.PublicOnly = doc.Visibility == planner.VisibilityPublic
	return req
}

// SignalReport says what one signal did, including that it did not run.
//
// "Did not run" is reported rather than omitted because the two absences
// mean opposite things to a reader: a vector signal that was skipped for
// want of an embedder has not told us the corpus disagrees, and a signal
// that ran and matched nothing has.
type SignalReport struct {
	// Signal is the signal name.
	Signal string `json:"signal"`
	// Ran reports whether the signal was issued.
	Ran bool `json:"ran"`
	// Skipped says why it was not, when Ran is false.
	Skipped string `json:"skipped,omitempty"`
	// Hits is how many rows or nodes it contributed.
	Hits int `json:"hits"`
}

// Skipped reasons.
const (
	// SkippedNoEmbedder: no embedder is configured, so the question cannot
	// be embedded and no vector can be compared. This is the state of a
	// deployment that has not answered "may platform content go to a
	// third-party embedding service" (internal/search/embedding/doc.go);
	// it is a missing signal, not an empty corpus.
	SkippedNoEmbedder = "no_embedder"
	// SkippedNoFacets: the query carried no facet filter, and the facet
	// signal without one would be "the first N rows of the index".
	SkippedNoFacets = "no_facets"
	// SkippedNoSeeds: nothing text-recalled resolved to a scientific object
	// version, so the traversal had nowhere to start. A corpus of assets,
	// releases and states is exactly this case, and it is not a failure.
	SkippedNoSeeds = "no_seeds"
)

// Result is one retrieval's outcome.
type Result struct {
	// Query is the question the retrieval ran (the request's, trimmed).
	Query string `json:"query"`
	// Candidates are the recalled entities, best-fused first. Every one of
	// them is version-pinned or is a state (see Candidate.Pinned).
	Candidates []Candidate `json:"candidates"`
	// Signals reports every signal's participation, including the ones that
	// did not run.
	Signals []SignalReport `json:"signals"`
}

// Retriever runs hybrid retrieval against a Store.
//
// The embedder is optional and its absence is a supported state, not a
// degraded one to be papered over: without it the vector signal does not run
// and the result SAYS SO (SignalReport.Skipped), because the alternative —
// substituting a different embedder for the query than the one that produced
// the stored vectors — would compare vectors from two models and return
// confident nonsense.
type Retriever struct {
	store    Store
	embedder embedding.Embedder
	log      *slog.Logger
}

// Option tunes a Retriever.
type Option func(*Retriever)

// WithLogger sets the retriever's logger (default slog.Default()).
func WithLogger(log *slog.Logger) Option {
	return func(r *Retriever) { r.log = log }
}

// NewRetriever builds a retriever over store. A nil store is refused: a
// retriever that cannot read is not a retriever, and the alternative is a
// nil-pointer panic at the first search.
//
// A nil embedder is ACCEPTED and is the supported state of a deployment that
// has not answered "may platform content go to a third-party embedding
// service" (internal/search/embedding/doc.go): the vector signal then does
// not run and the result says which signal was missing, rather than the
// service being unusable.
func NewRetriever(store Store, emb embedding.Embedder, opts ...Option) (*Retriever, error) {
	if store == nil {
		return nil, errors.New("retrieval: a retriever needs a store")
	}
	r := &Retriever{store: store, embedder: emb, log: slog.Default()}
	for _, opt := range opts {
		opt(r)
	}
	return r, nil
}

// The search-latency outcome vocabulary. "refused" is kept apart from
// "error" on purpose: ErrNoScope means the CALLER was not allowed to search,
// which is a permission outcome (docs/26 §3 "permission denied rates"), not
// a search that could not run — and lumping the two would make an
// access-control refusal look like the "search unavailable" P2 alert.
const (
	searchOutcomeOK      = "ok"
	searchOutcomeRefused = "refused"
	searchOutcomeError   = "error"
)

func searchOutcome(err error) string {
	switch {
	case err == nil:
		return searchOutcomeOK
	case errors.Is(err, ErrNoScope):
		return searchOutcomeRefused
	default:
		return searchOutcomeError
	}
}

// Retrieve runs one search: the three document signals, the fusion, and the
// traversal from the fused seeds.
func (r *Retriever) Retrieve(ctx context.Context, scope search.Scope, req Request) (res Result, retErr error) {
	// The query-latency observation (docs/26 §3 "search latency"). It is
	// taken over the WHOLE call — including the early refusals below —
	// because the number an SLO is written against is what the caller waits
	// for. Timing only the successes would let an outage make the histogram
	// look fast.
	start := time.Now()
	defer func() {
		observability.Default().ObserveSearchQuery(searchOutcome(retErr), time.Since(start).Seconds())
	}()

	if !scope.Authenticated() {
		return Result{}, ErrNoScope
	}
	req, err := req.normalize()
	if err != nil {
		return Result{}, err
	}

	acc := newAccumulator()
	report := &signalReports{}

	// ------------------------------------------------------------------
	// 1. The document signals.
	docQuery := DocumentQuery{
		Scope:            scope,
		Query:            req.Query,
		EntityTypes:      req.EntityTypes,
		StructuredFilter: req.Facets,
		PublicOnly:       req.PublicOnly,
		PageSize:         req.Limits.PerSignal,
	}
	if req.Query != "" {
		hits, err := r.store.FullText(ctx, docQuery)
		if err != nil {
			return Result{}, fmt.Errorf("%w: full text: %w", ErrStore, err)
		}
		acc.addHits(SignalFullText, hits)
		report.done(SignalFullText, len(hits))
	} else {
		report.skipped(SignalFullText, SkippedNoFacets)
	}

	if req.Query != "" && r.embedder != nil {
		if err := r.addVectorSignal(ctx, scope, docQuery, acc, report); err != nil {
			return Result{}, err
		}
	} else if r.embedder == nil {
		report.skipped(SignalVector, SkippedNoEmbedder)
	} else {
		report.skipped(SignalVector, SkippedNoFacets)
	}

	if len(req.Facets) > 0 {
		hits, err := r.store.Facets(ctx, docQuery)
		if err != nil {
			return Result{}, fmt.Errorf("%w: facets: %w", ErrStore, err)
		}
		acc.addHits(SignalFacets, hits)
		report.done(SignalFacets, len(hits))
	} else {
		report.skipped(SignalFacets, SkippedNoFacets)
	}

	// ------------------------------------------------------------------
	// 2. The traversal, seeded by the fused documents.
	if err := r.expand(ctx, scope, req, acc, report); err != nil {
		return Result{}, err
	}

	// ------------------------------------------------------------------
	// 3. Cut to size.
	cands := acc.ranked()
	if len(cands) > req.Limits.MaxCandidates {
		cands = cands[:req.Limits.MaxCandidates]
	}
	return Result{Query: req.Query, Candidates: cands, Signals: report.list()}, nil
}

// addVectorSignal embeds the question and issues the vector read.
//
// The embedding's dimensions are checked against the column's width before
// the read is issued, for the same reason the batch job checks them: a
// wrong-width vector is refused by the server with a type error, and the
// useful place to say "the provider is not the model this column was built
// for" is here.
func (r *Retriever) addVectorSignal(ctx context.Context, scope search.Scope, docQuery DocumentQuery, acc *accumulator, report *signalReports) error {
	vectors, err := r.embedder.Embed(ctx, []string{docQuery.Query})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrEmbedding, err)
	}
	if len(vectors) != 1 {
		return fmt.Errorf("%w: provider returned %d vectors for 1 text", ErrEmbedding, len(vectors))
	}
	if len(vectors[0]) != embedding.Dimensions {
		return fmt.Errorf("%w: provider returned a %d-wide vector, want %d",
			ErrEmbedding, len(vectors[0]), embedding.Dimensions)
	}
	model := r.embedder.Model()
	hits, err := r.store.Vector(ctx, VectorQuery{
		DocumentQuery: docQuery,
		Embedding:     embedding.FormatVector(vectors[0]),
		Model:         model,
	})
	if err != nil {
		return fmt.Errorf("%w: vector: %w", ErrStore, err)
	}
	acc.addHits(SignalVector, hits)
	report.done(SignalVector, len(hits))
	return nil
}

// expand runs the traversal from the fused documents, one level at a time.
//
// # Why the scope is re-checked at every level
//
// The hop query and the node query both carry the caller's projects, so a
// level that produces an id outside them returns nothing for it. That is the
// second line, not the first: the level's ids come only from edges the
// previous level admitted.
//
// # Why a level is synchronous
//
// A level's frontier is completed before the next level starts, and it is
// built in sorted id order rather than in the order the edges arrived. A
// breadth-first walk whose frontier depended on the database's return order
// for equal rows would reach the same nodes but discover them in a different
// order, and the graph signal's ranking is a discovery order — so the same
// query could return a different candidate order run to run.
func (r *Retriever) expand(ctx context.Context, scope search.Scope, req Request, acc *accumulator, report *signalReports) error {
	seeds, err := r.seeds(ctx, scope, acc.rankedPointers(), req.Limits.Seeds)
	if err != nil {
		return err
	}
	if len(seeds) == 0 {
		report.skipped(SignalGraph, SkippedNoSeeds)
		return nil
	}

	// visited holds every object version id the traversal has already
	// accounted for, so a cycle (and the relation graph is full of them —
	// supports/contradicts run both ways between the same two claims) cannot
	// make a level re-emit a node it already returned.
	visited := make(map[string]bool, len(seeds))
	frontier := make([]string, 0, len(seeds))
	fromRef := make(map[string]string, len(seeds))
	for _, seed := range seeds {
		if visited[seed.versionID] {
			continue
		}
		visited[seed.versionID] = true
		// The seed itself is NOT returned as a candidate: seeds() pinned it
		// to the document candidate that supplied it, so the document IS the
		// candidate for this version — with its visibility and its text
		// score — and emitting the bare object version alongside would be two
		// citations of one thing. A hop that comes back to the seed attaches
		// to that candidate by object version id.
		frontier = append(frontier, seed.versionID)
		fromRef[seed.versionID] = seed.ref
	}

	// rank orders the graph signal's own list: level-major, then the id
	// order the queries returned. It is a discovery order and not a
	// relevance order, and it is deterministic, which is what fusion and
	// the reproducibility expectation need from it.
	rank := 0
	for depth := 1; depth <= req.Limits.Depth && len(frontier) > 0; depth++ {
		edges, err := r.store.AdjacentRelations(ctx, scope, frontier)
		if err != nil {
			return fmt.Errorf("%w: graph hop: %w", ErrStore, err)
		}
		frontierSet := make(map[string]bool, len(frontier))
		for _, id := range frontier {
			frontierSet[id] = true
		}

		var (
			nextIDs []string
			nextHop = map[string]Hop{}
			seen    = map[string]bool{}
		)
		for _, edge := range edges {
			for _, side := range []struct {
				near, far, project string
				direction          string
			}{
				{edge.SourceVersionID, edge.TargetVersionID, edge.TargetProjectID, DirectionOut},
				{edge.TargetVersionID, edge.SourceVersionID, edge.SourceProjectID, DirectionIn},
			} {
				if !frontierSet[side.near] {
					continue
				}
				hop := Hop{
					RelationType: edge.RelationType,
					Direction:    side.direction,
					FromRef:      fromRef[side.near],
					Depth:        depth,
				}
				// A hop whose far end is ALREADY a candidate is recorded on
				// it, and the node is not emitted again. This is the common
				// case rather than an edge case: every published knowledge
				// document is also a seed, so a traversal that reaches a
				// document recalled independently has to say the two are
				// connected — that connection is the whole of what the graph
				// signal contributes to a candidate another signal found.
				if existing := acc.byObjectVersion(side.far); existing != nil {
					if !recordHop(existing, hop) {
						// The same edge, already recorded: counting it again
						// would inflate the graph signal's rank for a hop the
						// result does not actually have twice.
						continue
					}
					rank++
					existing.Signals = append(existing.Signals, SignalHit{Signal: SignalGraph, Rank: rank})
					existing.Score += rrf(rank)
					continue
				}
				if seen[side.far] || visited[side.far] {
					continue
				}
				seen[side.far] = true
				nextIDs = append(nextIDs, side.far)
				nextHop[side.far] = hop
			}
		}
		if len(nextIDs) == 0 {
			break
		}
		// The node query returns one deterministic order (by id), and the
		// frontier for the next level is built in that order rather than in
		// the order the edges happened to arrive: a level's output must not
		// depend on the database's return order for equal rows.
		sort.Strings(nextIDs)
		nodes, err := r.store.ObjectVersions(ctx, scope, nextIDs)
		if err != nil {
			return fmt.Errorf("%w: graph nodes: %w", ErrStore, err)
		}
		frontier = frontier[:0]
		for _, node := range nodes {
			visited[node.ObjectVersionID] = true
			rank++
			cand := objectCandidate(node)
			cand.Hops = []Hop{nextHop[node.ObjectVersionID]}
			cand.Signals = []SignalHit{{Signal: SignalGraph, Rank: rank}}
			cand.Score = rrf(rank)
			acc.put(cand)
			fromRef[node.ObjectVersionID] = cand.Ref
			frontier = append(frontier, node.ObjectVersionID)
		}
	}
	report.done(SignalGraph, acc.countSignal(SignalGraph))
	return nil
}

// seed is one traversal origin: the object version id the traversal starts
// from, and the candidate ref it is reached from (so a hop can say where it
// came from).
type seed struct {
	versionID string
	ref       string
}

// seeds resolves the fused documents into object version ids, pinning each
// resolved version back onto the document candidate that supplied it.
//
// # Which candidates can seed, and which cannot
//
// The pid comes from the document's own identity, which is the publication
// pid by construction (the projection addresses a knowledge document by its
// pid — projection.go, EntityRef), and the only join from a candidate into
// the scientific graph is knowledge_publications.object_version_id. The
// asset, release and state candidates are therefore skipped rather than
// guessed at: an asset and a release are bundles whose manifests are their
// own content, a state is a transition, and none of the three has a typed pin
// to an object version. Inventing one ("the project's newest objects",
// "whatever the manifest mentions") would be inventing what the question is
// about.
//
// # Why the document is mutated rather than duplicated
//
// A resolved version makes the document and the graph node the SAME entity
// seen from two sides, so the candidate is enriched with the id rather than
// a second candidate being created for it. That is what lets the traversal
// recognise a node it has already accounted for (accumulator.byObjectVersion)
// and attach the hop to the document instead of emitting a bare object
// version next to it.
//
// # Why the seed is authorized here
//
// Two checks, and the reason both are here rather than only in SQL.
//
// The seed read is the one graph read that CANNOT be scope-filtered, because
// its input is not a scope — it is a list of pids the caller has already been
// authorized to see (they came out of the access-predicated document read).
// So the scope is applied to its OUTPUT: a version whose project is not one
// the actor is in is refused. Without that, the graph half of a public search
// would start from a version the caller's scope does not cover and hand its
// id to the traversal.
//
// The second check is that the version belongs to the SAME project as the
// document it was resolved from. That is a correctness guard rather than an
// authorization one: a publication pid is a public identity, and a
// mis-joined row must not become a traversal origin just because it resolved.
//
// Both refusals are logged and then dropped, and the caller reports the graph
// signal as skipped for want of seeds: a version the scope does not cover is
// not an error, it is content the caller's graph has no part of. The hop
// query applies the same project rule to every level (and to the relation's
// own project, which a seed has no relation to yet), so this is the second
// line and not the only one.
func (r *Retriever) seeds(ctx context.Context, scope search.Scope, docs []*Candidate, limit int) ([]seed, error) {
	allowed := make(map[string]bool)
	for _, id := range scope.AllowedProjectIDs() {
		allowed[id] = true
	}
	pids := make([]string, 0, limit)
	byPID := make(map[string]*Candidate, limit)
	for _, doc := range docs {
		if len(pids) >= limit {
			break
		}
		if doc.EntityType != search.EntityKnowledge || doc.Identity == "" {
			continue
		}
		if _, ok := byPID[doc.Identity]; ok {
			continue
		}
		byPID[doc.Identity] = doc
		pids = append(pids, doc.Identity)
	}
	if len(pids) == 0 {
		return nil, nil
	}
	rows, err := r.store.SeedObjectVersions(ctx, scope, pids)
	if err != nil {
		return nil, fmt.Errorf("%w: graph seeds: %w", ErrStore, err)
	}
	out := make([]seed, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		doc, ok := byPID[row.Pid]
		if !ok {
			// A row for a pid nobody asked about: the store returned
			// something the caller did not request, and the honest reading
			// of a row we cannot tie to a document is to drop it.
			continue
		}
		if row.GraphObject.ObjectVersionID == "" || seen[row.GraphObject.ObjectVersionID] {
			continue
		}
		if !allowed[row.GraphObject.ProjectID] {
			r.log.Warn("search: retrieval seed outside the caller's scope",
				"ref", doc.Ref, "version_project", row.GraphObject.ProjectID)
			continue
		}
		if doc.ProjectID != "" && doc.ProjectID != row.GraphObject.ProjectID {
			r.log.Warn("search: retrieval seed for a document in another project",
				"ref", doc.Ref, "document_project", doc.ProjectID, "version_project", row.GraphObject.ProjectID)
			continue
		}
		seen[row.GraphObject.ObjectVersionID] = true
		doc.ObjectID = row.GraphObject.ObjectID
		doc.ObjectVersionID = row.GraphObject.ObjectVersionID
		doc.VersionNo = row.GraphObject.VersionNo
		out = append(out, seed{versionID: row.GraphObject.ObjectVersionID, ref: doc.Ref})
	}
	return out, nil
}

// objectCandidate turns a graph node into a candidate.
//
// The ref renders the object and its ordinal ("object_version:<uuid>@3"),
// which is the citation shape examples/search-answer.example.json uses for
// an object version (CLM-DEMO-001@2) and the one internal/assets/dependency.go
// adopts for a dependency pin.
func objectCandidate(node GraphObject) Candidate {
	identity := node.ObjectID
	if identity == "" {
		identity = node.ObjectVersionID
	}
	ref := search.EntityRef(RefKindObjectVersion, identity)
	version := ""
	if node.VersionNo > 0 {
		version = strconv.Itoa(node.VersionNo)
	}
	return Candidate{
		Ref:             renderRef(ref, version),
		Kind:            KindObjectVersion,
		Identity:        identity,
		Version:         version,
		Title:           node.Title,
		ProjectID:       node.ProjectID,
		ObjectType:      node.ObjectType,
		ObjectID:        node.ObjectID,
		ObjectVersionID: node.ObjectVersionID,
		VersionNo:       node.VersionNo,
	}
}

// recordHop adds a hop to a candidate unless the same edge was already
// recorded, and reports whether it was added.
//
// The hop list is a SET: the same relation version can be reached from two
// frontier nodes in one level, and re-adding it would both report a graph
// shape the corpus does not have and (because the caller then advances the
// graph signal's rank) let one connection occupy two positions in the fused
// ranking.
func recordHop(cand *Candidate, hop Hop) bool {
	for _, h := range cand.Hops {
		if h == hop {
			return false
		}
	}
	cand.Hops = append(cand.Hops, hop)
	return true
}
