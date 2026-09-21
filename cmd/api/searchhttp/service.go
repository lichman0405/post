package searchhttp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/answer"
	"github.com/lichman0405/post/internal/search/planner"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// The pipeline's error classes. They are the transport's own vocabulary —
// four shapes a caller is answered differently for — and each wraps the cause
// so the log keeps it (docs/45: the envelope names a class, the log names the
// dependency).
var (
	// errNoActor: the request reached the handler with no authenticated
	// principal. The guard makes this unreachable in production; it is checked
	// rather than assumed because the alternative to checking is searching on
	// behalf of nobody (internal/search/scope.go's ErrNoActor, which this
	// mirrors one layer out).
	errNoActor = errors.New("searchhttp: no actor")
	// ErrInvalidRequest: the request is not a search this surface can run as
	// given. The caller can fix it.
	ErrInvalidRequest = errors.New("searchhttp: invalid request")
	// ErrUnavailable: a dependency failed. The caller can do nothing with the
	// cause, so it is logged and not returned.
	ErrUnavailable = errors.New("searchhttp: unavailable")
	// ErrRecordFailed: the answer was produced and the record of it could not
	// be written. It is its own class because it is its own situation — the
	// search ran and its result is deliberately NOT returned (see search).
	ErrRecordFailed = errors.New("searchhttp: record failed")
)

// service runs the pipeline. It holds no state of its own: everything it
// needs is a dependency handed to it at wiring time.
type service struct {
	scope     search.ProjectScopeReader
	planner   Planner
	retriever Retriever
	ranker    Ranker
	answerer  Answerer
	records   RecordWriter
	limits    retrieval.Limits
}

// newService wires the pipeline, refusing the dependencies a search cannot be
// run without.
//
// The five required ones are required for the same reason and it is not
// defensive style: each is the ONLY producer of something the record or the
// answer must contain, so a nil one does not degrade the route, it changes
// what the route means. A nil scope would have to invent an actor; a nil
// retriever has no candidates to rank; a nil ranker would have to order them
// by something other than the six factors; a nil answerer has no answer
// document; and a nil record writer would publish an answer with an id that
// resolves to nothing.
//
// The refusal is a panic rather than an error because it can only happen at
// wiring time, in one place (cmd/api/main.go), where a nil field is a
// programming mistake rather than a runtime condition — the same shape
// ranking.NewRanker refuses a nil store in.
func newService(deps Deps) *service {
	switch {
	case deps.Scope == nil:
		panic("searchhttp: Deps.Scope is required")
	case deps.Retriever == nil:
		panic("searchhttp: Deps.Retriever is required")
	case deps.Ranker == nil:
		panic("searchhttp: Deps.Ranker is required")
	case deps.Answerer == nil:
		panic("searchhttp: Deps.Answerer is required")
	case deps.Records == nil:
		panic("searchhttp: Deps.Records is required")
	}
	return &service{
		scope:     deps.Scope,
		planner:   deps.Planner,
		retriever: deps.Retriever,
		ranker:    deps.Ranker,
		answerer:  deps.Answerer,
		records:   deps.Records,
		limits:    deps.Limits,
	}
}

// search runs one question through the pipeline and records it, returning the
// answer and the search id.
//
// # The order, and the two places it can stop
//
//	scope -> plan (optional) -> retrieve -> rank -> answer -> record
//
// A caller with no actor stops at the scope. A request the retrieval refuses
// (an empty question, a filter that is not an object, an unknown entity type)
// stops there, which is why the plan runs first and the VALIDATION does not:
// the retrieval's own request validation is the normative one (it is where
// the query and the filters meet), so this function does not pre-empt it with
// a second copy. The transport checks the shape the contract declares
// (handlers.go); retrieval decides what the request means.
//
// # Why a failed record is not a returned answer
//
// If the answer is produced and the record cannot be written, this returns
// ErrRecordFailed and NO answer. docs/22 §8 makes the saved plan, refs and
// citations part of what the Search API does, and the contract addresses a
// search by id afterwards (/search/{searchId}:start-project). An answer
// returned with an id that resolves to nothing is therefore not a partial
// success: it is a citation trail that does not exist, handed to a reader who
// has no way to learn that. The pipeline is idempotent from the caller's side
// — asking again is a new search — so refusing is cheap and honest, and the
// alternative (return the answer, drop the record) is the one outcome where
// the platform silently loses the evidence for what it just said.
func (s *service) search(ctx context.Context, actorID, query string, filters []byte) (answer.Answer, string, error) {
	if actorID == "" {
		return answer.Answer{}, "", errNoActor
	}
	scope, err := search.ResolveScope(ctx, s.scope, actorID)
	if err != nil {
		// A malformed actor id is the caller's session, not its request.
		return answer.Answer{}, "", fmt.Errorf("%w: resolve scope: %w", ErrUnavailable, err)
	}

	// The unplanned plan: the question, and nothing else. When no planner is
	// configured this IS the plan (there is no document, so the retrieval
	// narrows nothing), and every question about it is answered below without
	// a provider call.
	//
	// planDoc stays nil in that case, so the record's plan column is NULL
	// rather than holding an envelope for a planning step that never ran.
	// "This deployment has no model" and "the model was asked and failed" are
	// different rows, and only the second one has a plan to store.
	plan := planner.Plan{Query: query}
	var planDoc []byte
	if s.planner != nil {
		plan, err = s.planner.Plan(ctx, scope, planner.Request{Query: query})
		if err != nil {
			return answer.Answer{}, "", classify(ctx, "plan", err)
		}
		// The plan document's OWN canonical rendering (planner.Plan.CanonicalJSON),
		// not a shape invented here: it carries the plan's status and fallback
		// reason beside the document, which is what makes a stored plan say
		// whether the question was planned or answered unplanned, and it is
		// the rendering tests/searchplan already pins.
		if planDoc, err = plan.CanonicalJSON(); err != nil {
			return answer.Answer{}, "", fmt.Errorf("%w: render plan: %w", ErrRecordFailed, err)
		}
	}

	req := retrieval.RequestFromPlan(plan, filters, s.limits)
	recalled, err := s.retriever.Retrieve(ctx, scope, req)
	if err != nil {
		return answer.Answer{}, "", classify(ctx, "retrieve", err)
	}
	ranked, err := s.ranker.Rank(ctx, scope, req, recalled)
	if err != nil {
		return answer.Answer{}, "", classify(ctx, "rank", err)
	}
	ans, err := s.answerer.Answer(ctx, answer.Input{Result: ranked, Signals: recalled.Signals})
	if err != nil {
		return answer.Answer{}, "", classify(ctx, "answer", err)
	}

	searchID, err := s.record(ctx, actorID, query, filters, planDoc, recalled, ranked, ans)
	if err != nil {
		return answer.Answer{}, "", err
	}
	return ans, searchID, nil
}

// record writes the row docs/22 §8 requires, and is the only writer of it.
//
// Every value it stores was produced by the step that owns it: the plan by
// the planner, the refs by the retrieval-and-ranking, the citations by the
// answer. Nothing is derived here — see internal/persistence/queries/search.sql
// for why a third derivation of the citation rule could only disagree with
// the two that already exist (the answer package's guard and the table's
// CHECK).
func (s *service) record(
	ctx context.Context,
	actorID, query string,
	filters []byte,
	planDoc []byte,
	recalled retrieval.Result,
	ranked ranking.Result,
	ans answer.Answer,
) (string, error) {
	signals, err := json.Marshal(recalled.Signals)
	if err != nil {
		return "", fmt.Errorf("%w: render signals: %w", ErrRecordFailed, err)
	}
	answerDoc, err := ans.CanonicalJSON()
	if err != nil {
		return "", fmt.Errorf("%w: render answer: %w", ErrRecordFailed, err)
	}

	refs := make([]string, 0, len(ranked.Ranked))
	for _, r := range ranked.Ranked {
		refs = append(refs, r.Ref)
	}
	searchID, err := s.records.Save(ctx, persistence.SearchRecord{
		ActorID:      actorID,
		Query:        query,
		Filters:      filters,
		Plan:         planDoc,
		Signals:      signals,
		SelectedRefs: refs,
		Citations:    ans.Citations,
		Answer:       answerDoc,
	})
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrRecordFailed, err)
	}
	return searchID, nil
}

// classify turns one step's error into the class the wire answers with.
//
// The rule is the caller's control over the outcome, not the depth of the
// stack: a request the SEARCH cannot run as given is 400 and the caller can
// fix it (an empty question, a filter that is not an object, an entity type
// the projection does not have, a limit out of range), and everything else is
// 503 — the caller asked a well-formed question and a dependency of the
// platform failed. A context that ended is neither: it is the caller that
// stopped waiting, and it is classified as unavailable so the log records the
// step while the (probably unwritable) response says nothing about it.
func classify(ctx context.Context, step string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %s: caller's context ended: %w", ErrUnavailable, step, err)
	}
	if isCallerFault(err) {
		return fmt.Errorf("%w: %s: %w", ErrInvalidRequest, step, err)
	}
	return fmt.Errorf("%w: %s: %w", ErrUnavailable, step, err)
}

// isCallerFault reports whether an error names something the caller asked for
// rather than something the platform could not do.
//
// It is one switch over the pipeline's refusal vocabulary and it is closed on
// purpose: every case below is a value where the ANSWER is "send a different
// request". A new error added to one of those packages without a case here is
// answered 503, which is the fail-safe direction — a false 503 is retried and
// investigated, a false 400 tells the caller to change a request that was
// fine.
func isCallerFault(err error) bool {
	switch {
	case errors.Is(err, planner.ErrEmptyQuery),
		errors.Is(err, retrieval.ErrNoQuery),
		errors.Is(err, retrieval.ErrBadFilter),
		errors.Is(err, retrieval.ErrUnknownEntityType),
		errors.Is(err, retrieval.ErrTooLong):
		return true
	default:
		return false
	}
}
