package answer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// Generator turns a ranking into an answer: it asks a provider for a summary
// of the retrieved sources, refuses anything that does not survive the schema
// and the grounding guard, and answers with either the summary or the
// structured result plus the reason there is no summary.
//
// # The fallback is a success path, not an error path
//
// docs/27 §SLO: "完整答案目标 < 10s，超时提供 structured results fallback".
// The same shape planner.Planner documents for planning, one step later: a
// provider that is absent, slow, broken or untrustworthy costs the user the
// written answer and never the search. Every one of those outcomes returns an
// Answer with Status fallback, a Reason a reader can act on, and the ranked
// sources — so a caller renders one document in every case and never has to
// decide whether an empty summary was an answer.
//
// # What the provider is and is not given
//
// Request carries the question and the citation vocabulary (provider.go
// records what is deliberately absent: the principal, the scope, the project
// membership, any entity retrieval did not return). The provider's reply is
// BYTES, and nothing in it is believed before the schema in schema.go and the
// guard in grounding.go have both passed it.
type Generator struct {
	provider Provider
	timeout  time.Duration
	log      *slog.Logger
	schema   *jsonschema.Schema
}

// DefaultTimeout bounds ONE provider call.
//
// The arithmetic, continuing planner.DefaultTimeout's: docs/27 §SLO gives the
// whole evidence-backed answer 10s, planning takes 2s of it and candidate
// retrieval p95 < 2s, so the answer keeps the remainder and rounds down. A
// provider that needs longer than this is a provider whose latency is part of
// the answer's cost, and the fallback exists precisely so that cost is
// bounded.
const DefaultTimeout = 6 * time.Second

// Deps are a Generator's dependencies.
type Deps struct {
	// Provider is the answer provider. It is OPTIONAL, and its absence is a
	// supported deployment state rather than a configuration defect: no
	// document in this repository names a vendor or model for answer
	// writing, and whether a question and its retrieved titles may leave the
	// platform for a third-party service is a product/privacy decision that
	// has not been taken (doc.go). Every answer is then the structured
	// fallback — ReasonNoProvider — which is the honest answer to "what does
	// the network hold about this" that the platform can give without a
	// model.
	//
	// This is the one place this package differs deliberately from
	// planner.New, which refuses to exist without a provider: planning's
	// absence changes what is READ (no narrowing, no visibility filter), so
	// it has to be decided; an answer's absence changes only whether a
	// sentence is written.
	Provider Provider
	// Timeout bounds one provider call; 0 means DefaultTimeout.
	Timeout time.Duration
	// Logger receives the fallback records; nil discards them.
	Logger *slog.Logger
}

// New builds a Generator. It compiles the packaged answer schema here, once,
// so a broken schema is a startup failure instead of every answer silently
// falling back forever.
func New(deps Deps) (*Generator, error) {
	schema, err := compileAnswerSchema()
	if err != nil {
		return nil, err
	}
	timeout := deps.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	log := deps.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Generator{provider: deps.Provider, timeout: timeout, log: log, schema: schema}, nil
}

// Input is one answer request: the ranking to answer from, and the retrieval's
// own report of which signals ran.
//
// The signal report travels separately from the ranking because it is a
// property of the RETRIEVAL and not of the ordering (retrieval.Result.Signals,
// which ranking.Result deliberately does not carry). It is how a limitation
// can say "the vector signal did not run" — a fact about this answer's
// coverage that no source's own factors state.
type Input struct {
	// Result is the ranking to answer from. Its Query becomes the answer's.
	Result ranking.Result
	// Signals is the retrieval's signal report (retrieval.Result.Signals).
	Signals []retrieval.SignalReport
}

// Answer produces the answer for in.
//
// The only error it returns is the CALLER'S context ending (cancelled or past
// its deadline) — planner.Planner's rule, for its reason: a caller that has
// stopped waiting is not served by a fallback nobody will read, and hiding a
// cancellation inside a successful return would tell the caller a search
// happened that did not. Everything else — no provider, no source, a provider
// error, a slow provider, a document that violates the schema, a document
// that cites what retrieval did not return — is an Answer with Status
// fallback and a Reason.
func (g *Generator) Answer(ctx context.Context, in Input) (Answer, error) {
	query := in.Result.Query
	ranked := in.Result.Ranked
	sources := toSources(ranked)

	// A search with nothing to cite is not asked about. The provider is not
	// called at all — not even to say "I found nothing" — because a model
	// asked to answer with an empty citation vocabulary has exactly one
	// material to work from, and it is not the platform's.
	if len(sources) == 0 {
		return g.fallback(ctx, query, sources, in.Signals, ReasonNoSources, nil), nil
	}
	if g.provider == nil {
		return g.fallback(ctx, query, sources, in.Signals, ReasonNoProvider, nil), nil
	}

	req := Request{
		Query:        query,
		Citable:      toCitable(ranked),
		AnswerSchema: AnswerSchema(),
	}
	ansCtx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	doc, err := g.provider.AnswerQuestion(ansCtx, req)
	switch {
	case ctx.Err() != nil:
		// The CALLER'S context ended. Not this generator's timeout and not a
		// provider failure: the caller has stopped waiting.
		return Answer{}, ctx.Err()
	case err != nil:
		if errors.Is(err, context.DeadlineExceeded) || ansCtx.Err() != nil {
			return g.fallback(ctx, query, sources, in.Signals, ReasonTimeout, err), nil
		}
		return g.fallback(ctx, query, sources, in.Signals, ReasonProviderError, err), nil
	}

	var raw any
	if err := json.Unmarshal(doc, &raw); err != nil {
		return g.fallback(ctx, query, sources, in.Signals, ReasonInvalidAnswer, err), nil
	}
	if err := g.schema.Validate(raw); err != nil {
		return g.fallback(ctx, query, sources, in.Signals, ReasonInvalidAnswer, err), nil
	}
	var decoded document
	if err := json.Unmarshal(doc, &decoded); err != nil {
		// Unreachable while the schema and the struct agree; kept because
		// "the schema said yes" is not a reason to trust an unchecked decode
		// (planner.Planner's identical branch).
		return g.fallback(ctx, query, sources, in.Signals, ReasonInvalidAnswer, err), nil
	}

	// The guard. It is the last check and the one that cannot be expressed as
	// a schema, so it runs on the DECODED document: a violation anywhere —
	// a citation outside the vocabulary, an entity named in the summary —
	// drops the whole document rather than pruning it (grounding.go).
	if violations := checkDocument(decoded, vocabularyFor(ranked)); len(violations) > 0 {
		return g.fallback(ctx, query, sources, in.Signals, ReasonUngroundedCitation,
			errors.New(violationsError(violations))), nil
	}

	return buildAnswer(query, sources, in.Signals, StatusAnswered, "", decoded.Summary, decoded.Citations), nil
}

// fallback builds the structured answer and records why there is no summary.
// It is the single exit for every provider-shaped failure, so a new failure
// mode cannot be added without a reason to name it (planner.Planner's rule).
func (g *Generator) fallback(ctx context.Context, query string, sources []Source, signals []retrieval.SignalReport, reason Reason, cause error) Answer {
	g.log.WarnContext(ctx, "search answer fell back to structured results",
		slog.String("reason", string(reason)),
		slog.Int("query_length", len(query)),
		slog.Int("sources", len(sources)),
		slog.String("cause", truncateForLog(cause)),
	)
	return buildAnswer(query, sources, signals, StatusFallback, reason, "", nil)
}

// buildAnswer assembles the answer document from parts that have already been
// decided. Every slice it writes is non-nil, so the canonical rendering says
// [] rather than null for an empty section — a client that has to distinguish
// "no citations" from "the field is missing" is a client reading a document
// this package did not mean to write.
func buildAnswer(query string, sources []Source, signals []retrieval.SignalReport, status Status, reason Reason, summary string, citations []string) Answer {
	cited := make(map[string]bool, len(citations))
	for _, ref := range citations {
		cited[ref] = true
	}
	for i := range sources {
		sources[i].Cited = cited[sources[i].Ref]
	}
	if citations == nil {
		citations = []string{}
	}
	if sources == nil {
		sources = []Source{}
	}
	return Answer{
		Version:     AnswerVersion,
		Status:      status,
		Reason:      reason,
		Query:       query,
		AnswerView:  status == StatusAnswered,
		Summary:     summary,
		Citations:   citations,
		Limitations: deriveLimitations(sources, signals, reason),
		Conflicts:   deriveConflicts(sources),
		Sources:     sources,
	}
}

// toSources renders the ranked candidates as answer sources, in rank order.
//
// Every field is copied rather than re-derived — the ref, the version, the
// labels and the six factors are the ranking's own values — because the
// answer's job is to show what the ranking decided, not to decide it again. A
// source is not re-read from the database here: this package reads no row of
// its own (doc.go), and a second read could only disagree with the one that
// produced the order.
func toSources(ranked []ranking.Ranked) []Source {
	out := make([]Source, 0, len(ranked))
	for i, r := range ranked {
		rank := r.Rank
		if rank <= 0 {
			// The ranking always sets it; a hand-built result may not, and a
			// source list whose ranks are all zero would be unreadable.
			rank = i + 1
		}
		out = append(out, Source{
			Rank:       rank,
			Ref:        r.Ref,
			Kind:       r.Kind,
			EntityType: r.EntityType,
			ObjectType: r.ObjectType,
			Title:      r.Title,
			Version:    r.Version,
			Href:       locate(r),
			ProjectID:  r.Candidate.ProjectID,
			Labels:     sortedCopy(r.Labels),
			Factors:    factorCopy(r.Factors),
		})
	}
	return out
}

// toCitable renders the ranked candidates as the provider's citation
// vocabulary, in rank order. The reasons are the ranking's own sentences,
// verbatim, so an adapter cannot invent a second account of a source's
// evidence (provider.go).
func toCitable(ranked []ranking.Ranked) []CitableEntity {
	out := make([]CitableEntity, 0, len(ranked))
	for _, r := range ranked {
		reasons := make([]string, 0, len(r.Factors))
		for _, a := range r.Factors {
			reasons = append(reasons, a.Reason)
		}
		out = append(out, CitableEntity{
			Ref:        r.Ref,
			Kind:       r.Kind,
			EntityType: r.EntityType,
			ObjectType: r.ObjectType,
			Title:      r.Title,
			Version:    r.Version,
			Labels:     sortedCopy(r.Labels),
			Reasons:    reasons,
		})
	}
	return out
}

// sortedCopy copies a label list without sharing its backing array with the
// caller. The order is the ranking's (ranking sorts labels into its own
// order), and this package does not re-sort: a second ordering would be a
// second vocabulary.
func sortedCopy(in []string) []string {
	out := make([]string, 0, len(in))
	return append(out, in...)
}

// factorCopy copies the six assessments. It renders as [] rather than null
// when the ranking returned none, which only a hand-built result can do.
func factorCopy(in []ranking.Assessment) []ranking.Assessment {
	out := make([]ranking.Assessment, 0, len(in))
	return append(out, in...)
}

// truncateForLog bounds a cause message: schema-validation causes echo parts
// of the offending document, and a log line is not a document store
// (planner.truncateForLog).
func truncateForLog(err error) string {
	if err == nil {
		return ""
	}
	const max = 200
	msg := err.Error()
	if len(msg) <= max {
		return msg
	}
	return msg[:max] + "…"
}

// String renders an answer's status and reason for a log line or a test
// failure: "fallback (ungrounded_citation)".
func (a Answer) String() string {
	if a.Reason == "" {
		return string(a.Status)
	}
	return fmt.Sprintf("%s (%s)", a.Status, a.Reason)
}
