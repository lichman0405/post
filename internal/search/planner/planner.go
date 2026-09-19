package planner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/lichman0405/post/internal/search"
)

// Planner is the query-planning service: it asks the Provider for a plan
// document, refuses anything that does not survive the schema and the
// identifier guard, and answers with either a validated plan or an explicit
// fallback.
//
// # The fallback is a success path, not an error path
//
// docs/27 §SLO: "Evidence-backed Answer 若调用 LLM，首个 loading state <
// 200ms；完整答案目标 < 10s，超时提供 structured results fallback", and
// docs/32's risk register mitigates "Search 产生看似科学的 hallucination"
// with "planner structured; answer only cited entity ids; fallback structured
// results". So a provider that fails, hangs or hallucinates a shape must cost
// the user the ANSWER, not the SEARCH: the caller still gets the question and
// the scope, runs the structured query, and renders results without a
// synthesized answer.
//
// That is why Plan returns an error only for the two conditions a caller can
// act on — an empty question and an unresolved scope — and never for a
// provider failure. Every provider-shaped failure is a Plan whose Status is
// StatusFallback and whose Reason says which one happened, so the caller's
// control flow has exactly one branch and the fallback rate is countable per
// cause (docs/26 §3 lists "LLM planner failures" among the metrics; with no
// metrics facility in the tree, this package logs the fallback at WARN with
// its reason and the actor — docs/26 §2 keeps the actor on the search's
// correlation trail).
//
// # Every document is validated before any of it is used
//
// The order is: JSON, then the schema (plan_schema.go), then the identifier
// guard, then the typed decode. Nothing reaches a consumer that has not
// passed all four. The guard is the one rule the schema cannot express:
//
//   - The schema's additionalProperties:false refuses a plan that declares a
//     field for entity ids, because no such field is in the vocabulary.
//   - The guard refuses a plan that smuggles an identifier into a field that
//     IS in the vocabulary (a uuid-shaped token in `condition_scope.text`,
//     say). docs/54 #7 is "Search LLM 引用未授权 entity id" and
//     docs/22 §8 allows an answer to cite only what RETRIEVAL returned; a
//     plan is written before retrieval runs, so an identifier in it was
//     named from memory, not from a row the searcher may see.
//
// The guard is a vocabulary rule, not the authorization boundary, and saying
// so precisely matters: the boundary is the read query's visibility filter
// under the caller's resolved scope (internal/search/scope.go,
// internal/persistence/queries/search.sql). A plan carrying no identifier is
// what makes the planner *incapable* of naming an unauthorized entity; the
// scope is what makes retrieval incapable of returning one.
//
// The guard is deliberately shape-based and cheap: it looks for the
// platform's identity form (a uuid) anywhere in the document's values. Free
// text may legitimately contain almost anything — chemical formulas, DOIs,
// numeric ranges — and none of those are identities. A question that really
// does type a uuid ("what is 3f2a...?") loses its plan and keeps its
// structured results, which is the fail-closed direction.
type Planner struct {
	provider Provider
	timeout  time.Duration
	log      *slog.Logger
	schema   *jsonschema.Schema
}

// DefaultTimeout bounds ONE provider call.
//
// The arithmetic behind the number: docs/27 §SLO gives the whole
// evidence-backed answer 10s, and the steps that follow planning are candidate
// retrieval (its own SLO: p95 < 2s) and answer generation. Planning is the
// first step and the cheapest to lose — its failure costs a synthesized
// answer, never the results — so it gets one retrieval-sized slice of the
// budget (2s) and the answer keeps the rest. Nothing upstream waits on it
// beyond that: the expiry IS the fallback.
const DefaultTimeout = 2 * time.Second

// ErrNoProvider is returned by New when no Provider is wired. The service
// refuses to exist rather than answering every question with a fallback: a
// planner with no provider is a configuration defect, not a degraded
// deployment.
var ErrNoProvider = errors.New("planner: no provider")

// ErrEmptyQuery is returned by Plan for a question with no text. It is the
// caller's bug (specs/api POST /search requires `query`), not a provider
// condition, so it is an error rather than a fallback.
var ErrEmptyQuery = errors.New("planner: empty query")

// Deps wires a Planner.
type Deps struct {
	// Provider is required.
	Provider Provider
	// Timeout bounds one provider call; 0 means DefaultTimeout.
	Timeout time.Duration
	// Logger receives the fallback records; nil discards them.
	Logger *slog.Logger
}

// New builds a Planner. It compiles the packaged plan schema here, once, so a
// broken schema is a startup failure instead of a search that silently falls
// back forever.
func New(deps Deps) (*Planner, error) {
	if deps.Provider == nil {
		return nil, ErrNoProvider
	}
	schema, err := compilePlanSchema()
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
	return &Planner{provider: deps.Provider, timeout: timeout, log: log, schema: schema}, nil
}

// Plan turns one question into one plan, under the scope of the actor asking
// it.
//
// The scope parameter is required even though planning reads no rows, and
// that is deliberate. A plan is the first half of a search, and a search
// without a principal has no scope, so this is the place "no actor" becomes
// impossible rather than merely unusual: a Scope is only obtainable from
// search.ResolveScope, its fields are unexported, and its zero value is
// rejected here (search.ErrNoActor). The alternative — planning for nobody
// and letting retrieval decide later — puts the unauthorized case on a path
// where the only remaining question is whether somebody remembered to check.
func (p *Planner) Plan(ctx context.Context, scope search.Scope, req Request) (Plan, error) {
	if req.Query == "" {
		return Plan{}, ErrEmptyQuery
	}
	if !scope.Authenticated() {
		return Plan{}, fmt.Errorf("planner: plan %q: %w", req.Query, search.ErrNoActor)
	}
	req.PlanSchema = PlanSchema()

	planCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	doc, err := p.provider.PlanQuery(planCtx, req)
	switch {
	case ctx.Err() != nil:
		// The CALLER'S context ended — cancelled, or the caller's own
		// deadline passed. That is not this planner's timeout and not a
		// provider failure; the caller has stopped waiting, and a fallback
		// nobody reads would hide its own cancellation from it.
		return Plan{}, ctx.Err()
	case err != nil:
		if errors.Is(err, context.DeadlineExceeded) || planCtx.Err() != nil {
			return p.fallback(ctx, scope, req, ReasonTimeout, err), nil
		}
		return p.fallback(ctx, scope, req, ReasonProviderError, err), nil
	}

	var raw any
	if err := json.Unmarshal(doc, &raw); err != nil {
		return p.fallback(ctx, scope, req, ReasonInvalidPlan, err), nil
	}
	if err := p.schema.Validate(raw); err != nil {
		return p.fallback(ctx, scope, req, ReasonInvalidPlan, err), nil
	}
	if paths := identifierPaths(raw); len(paths) > 0 {
		return p.fallback(ctx, scope, req, ReasonIdentifier,
			fmt.Errorf("plan carries an entity-identifier-shaped value at %v", paths)), nil
	}

	var decoded Document
	if err := json.Unmarshal(doc, &decoded); err != nil {
		// Unreachable while the schema and the struct agree; kept because
		// "the schema said yes" is not a reason to trust an unchecked decode.
		return p.fallback(ctx, scope, req, ReasonInvalidPlan, err), nil
	}
	return Plan{Status: StatusPlanned, Query: req.Query, Document: &decoded}, nil
}

// fallback builds the degraded plan and records why. It is the single exit
// for every provider-shaped failure, so a new failure mode cannot be added
// without a reason to name it.
func (p *Planner) fallback(ctx context.Context, scope search.Scope, req Request, reason Reason, cause error) Plan {
	p.log.WarnContext(ctx, "search planning fell back to structured results",
		slog.String("reason", string(reason)),
		slog.String("actor_id", scope.ActorID()),
		slog.Int("query_length", len(req.Query)),
		slog.String("cause", truncateForLog(cause)),
	)
	return Plan{Status: StatusFallback, Reason: reason, Query: req.Query}
}

// truncateForLog bounds a cause message: schema-validation causes echo parts
// of the offending document, and a log line is not a document store.
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

// identityToken matches the platform's entity identity form: a uuid, the
// primary key every entity table in this repository uses (docs/21 §8's object
// ids, the Id fields of internal/persistence/sqlc, the @allowed_project_ids
// of the read query). It is matched anywhere inside a value, so an identifier
// prefixed by an entity kind ("asset:<uuid>") is caught too.
var identityToken = regexp.MustCompile(
	`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// identifierPaths returns the path of every string in a decoded document that
// carries an identity-shaped token, in a deterministic order (map keys
// sorted), so the same document always reports the same paths.
func identifierPaths(doc any) []string {
	var found []string
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch node := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(node))
			for k := range node {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(node[k], path+"."+k)
			}
		case []any:
			for i, item := range node {
				walk(item, fmt.Sprintf("%s[%d]", path, i))
			}
		case string:
			if identityToken.MatchString(node) {
				found = append(found, path)
			}
		}
	}
	walk(doc, "")
	return found
}
