package planner_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/planner"
	"github.com/lichman0405/post/internal/search/planner/plannertest"
)

// The question every test in this file plans for, and a document that answers
// it. The document is the shipped vocabulary in its most complete form: one
// value for each of docs/14 §2's eight items.
const (
	question = "Which MOF materials show CO2 uptake above 3 mmol/g at 298 K, and has that been reproduced?"

	fullDocument = `{
  "plan_version": "1",
  "intent": "answer",
  "target_object": {"entity_types": ["asset", "knowledge"]},
  "property": {"names": ["CO2 uptake"]},
  "condition_scope": {
    "text": "at 298 K",
    "comparators": [
      {"property": "CO2 uptake", "op": "gt", "value": "3", "unit": "mmol/g"},
      {"property": "temperature", "op": "eq", "value": "298", "unit": "K"}
    ]
  },
  "evidence_preference": {"types": ["experimental", "computational"], "prefer": ["independently_reproduced"]},
  "network_scope": "platform",
  "visibility": "accessible",
  "ranking_constraints": {"order_by": ["query_scope_match", "evidence_profile"]}
}`
)

// scopeFor resolves a real search.Scope for a user, the only way one can be
// built (search.Scope's fields are unexported on purpose).
func scopeFor(t *testing.T, userID string, projectIDs ...string) search.Scope {
	t.Helper()
	projects := make([]domain.Project, 0, len(projectIDs))
	for _, id := range projectIDs {
		projects = append(projects, domain.Project{ID: id})
	}
	reader := plannertest.ScopeReader{ByUser: map[string][]domain.Project{userID: projects}}
	scope, err := search.ResolveScope(context.Background(), reader, userID)
	if err != nil {
		t.Fatalf("resolve scope: %v", err)
	}
	return scope
}

// newPlanner builds a planner on a script, or fails the test.
func newPlanner(t *testing.T, deps planner.Deps) *planner.Planner {
	t.Helper()
	if deps.Provider == nil {
		deps.Provider = plannertest.Reply(fullDocument)
	}
	p, err := planner.New(deps)
	if err != nil {
		t.Fatalf("planner.New: %v", err)
	}
	return p
}

// projectID is a well-formed uuid: the identity shape the identifier guard
// looks for.
const projectID = "3f2a5b7c-1d4e-4a6b-8c9d-0e1f2a3b4c5d"

// TestPlanAcceptsValidDocument is the positive direction of the "invalid plan
// fallback" acceptance criterion: a legal document must be ACCEPTED and
// decoded, or an implementation that fell back unconditionally would satisfy
// the negative direction on its own.
func TestPlanAcceptsValidDocument(t *testing.T) {
	prov := plannertest.Reply(fullDocument)
	p := newPlanner(t, planner.Deps{Provider: prov})

	got, err := p.Plan(context.Background(), scopeFor(t, "u-1", projectID), planner.Request{Query: question})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got.Status != planner.StatusPlanned || got.Reason != "" {
		t.Fatalf("status = %q reason = %q, want planned with no reason", got.Status, got.Reason)
	}
	if !got.Planned() {
		t.Fatalf("Planned() = false on a planned result")
	}
	if got.Query != question {
		t.Errorf("Query = %q, want the question verbatim", got.Query)
	}
	doc := got.Document
	if doc == nil {
		t.Fatal("Document is nil on a planned result")
	}
	// One assertion per spec item, so a field dropped from the vocabulary
	// fails here rather than in a consumer.
	if doc.PlanVersion != planner.PlanVersion {
		t.Errorf("plan_version = %q, want %q", doc.PlanVersion, planner.PlanVersion)
	}
	if doc.Intent != planner.IntentAnswer {
		t.Errorf("intent = %q, want %q", doc.Intent, planner.IntentAnswer)
	}
	if want := []string{search.EntityAsset, search.EntityKnowledge}; !reflect.DeepEqual(doc.TargetObject.EntityTypes, want) {
		t.Errorf("target_object.entity_types = %v, want %v", doc.TargetObject.EntityTypes, want)
	}
	if doc.Property == nil || !reflect.DeepEqual(doc.Property.Names, []string{"CO2 uptake"}) {
		t.Errorf("property = %+v, want names [CO2 uptake]", doc.Property)
	}
	if doc.ConditionScope == nil || doc.ConditionScope.Text != "at 298 K" || len(doc.ConditionScope.Comparators) != 2 {
		t.Errorf("condition_scope = %+v, want text and two comparators", doc.ConditionScope)
	}
	if doc.EvidencePreference == nil ||
		!reflect.DeepEqual(doc.EvidencePreference.Types, []string{string(domain.EvidenceTypeExperimental), string(domain.EvidenceTypeComputational)}) ||
		!reflect.DeepEqual(doc.EvidencePreference.Prefer, []string{planner.EvidencePreferIndependentlyReproduced}) {
		t.Errorf("evidence_preference = %+v", doc.EvidencePreference)
	}
	if doc.NetworkScope != planner.NetworkScopePlatform {
		t.Errorf("network_scope = %q", doc.NetworkScope)
	}
	if doc.Visibility != planner.VisibilityAccessible {
		t.Errorf("visibility = %q", doc.Visibility)
	}
	if doc.RankingConstraints == nil ||
		!reflect.DeepEqual(doc.RankingConstraints.OrderBy, []string{planner.RankQueryScopeMatch, planner.RankEvidenceProfile}) {
		t.Errorf("ranking_constraints = %+v", doc.RankingConstraints)
	}
	if prov.Calls() != 1 {
		t.Errorf("provider calls = %d, want 1", prov.Calls())
	}
}

// TestPlanFallsBackOnRefusedDocuments is the negative direction of the same
// criterion: a provider that can only produce a document this package refuses
// must not crash, must not hang and must not return a half-parsed plan — it
// falls back, with the question intact so the structured path still runs.
//
// Each case is a shape a real provider plausibly emits: the two unknown-key
// cases are the ones that matter most (docs/54 #7 — a provider offering
// candidate entity ids), the rest are ordinary schema violations.
func TestPlanFallsBackOnRefusedDocuments(t *testing.T) {
	uuid := "3f2a5b7c-1d4e-4a6b-8c9d-0e1f2a3b4c5d"
	cases := []struct {
		name string
		doc  string
	}{
		{"empty response", ``},
		{"not JSON", `{"plan_version": "1"`},
		{"JSON null", `null`},
		{"JSON array", `[]`},
		{"unknown field entity_ids", strings.Replace(fullDocument,
			`"ranking_constraints"`, `"entity_ids": ["`+uuid+`"], "ranking_constraints"`, 1)},
		{"unknown field candidate_sources", strings.Replace(fullDocument,
			`"plan_version": "1",`, `"plan_version": "1", "candidate_sources": ["asset:uio-66"],`, 1)},
		{"unknown nested field", strings.Replace(fullDocument,
			`"entity_types": ["asset", "knowledge"]`, `"entity_types": ["asset", "knowledge"], "ids": ["`+uuid+`"]`, 1)},
		{"intent outside the vocabulary", strings.Replace(fullDocument, `"intent": "answer"`, `"intent": "summarize"`, 1)},
		{"missing target_object", strings.Replace(fullDocument, `"target_object": {"entity_types": ["asset", "knowledge"]},`, ``, 1)},
		{"empty target set", strings.Replace(fullDocument, `["asset", "knowledge"]`, `[]`, 1)},
		{"duplicate entity types", strings.Replace(fullDocument, `["asset", "knowledge"]`, `["asset", "asset"]`, 1)},
		{"comparator op outside the vocabulary", strings.Replace(fullDocument, `"op": "gt"`, `"op": "greater"`, 1)},
		{"plan_version not the pinned one", strings.Replace(fullDocument, `"plan_version": "1"`, `"plan_version": "2"`, 1)},
		{"intent not a string", strings.Replace(fullDocument, `"intent": "answer"`, `"intent": 7`, 1)},
		{"ranking criteria outside docs/14 §3", strings.Replace(fullDocument,
			`["query_scope_match", "evidence_profile"]`, `["popularity"]`, 1)},
		{"evidence type outside the canonical list", strings.Replace(fullDocument,
			`["experimental", "computational"]`, `["vibes"]`, 1)},
		{"empty condition_scope object", strings.Replace(fullDocument,
			`"condition_scope": {
    "text": "at 298 K",
    "comparators": [
      {"property": "CO2 uptake", "op": "gt", "value": "3", "unit": "mmol/g"},
      {"property": "temperature", "op": "eq", "value": "298", "unit": "K"}
    ]
  },`, `"condition_scope": {},`, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := plannertest.Reply(tc.doc)
			p := newPlanner(t, planner.Deps{Provider: prov})

			got, err := p.Plan(context.Background(), scopeFor(t, "u-1", projectID), planner.Request{Query: question})
			if err != nil {
				t.Fatalf("Plan returned an error for a refused document: %v", err)
			}
			if got.Status != planner.StatusFallback {
				t.Fatalf("status = %q, want %q", got.Status, planner.StatusFallback)
			}
			if got.Reason != planner.ReasonInvalidPlan {
				t.Errorf("reason = %q, want %q", got.Reason, planner.ReasonInvalidPlan)
			}
			if got.Document != nil {
				t.Errorf("Document is non-nil on a fallback: %+v", got.Document)
			}
			// The exit the SLO promises: structured results are still
			// computable, which needs the question and nothing else.
			if got.Query != question {
				t.Errorf("Query = %q, want the question verbatim", got.Query)
			}
			if got.Planned() {
				t.Error("Planned() = true on a fallback")
			}
		})
	}
}

// TestPlanFallsBackOnProviderError: the provider failing is a fallback, not an
// error to the caller — the user loses the synthesized answer, not the search.
func TestPlanFallsBackOnProviderError(t *testing.T) {
	prov := plannertest.Failing(errors.New("upstream refused"))
	p := newPlanner(t, planner.Deps{Provider: prov})

	got, err := p.Plan(context.Background(), scopeFor(t, "u-1", projectID), planner.Request{Query: question})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got.Status != planner.StatusFallback || got.Reason != planner.ReasonProviderError {
		t.Fatalf("status = %q reason = %q, want fallback/provider_error", got.Status, got.Reason)
	}
	if got.Query != question {
		t.Errorf("Query = %q, want the question verbatim", got.Query)
	}
}

// TestPlanFallsBackOnProviderTimeout pins the SLO's own trigger
// (docs/27 §SLO: "超时提供 structured results fallback") and the "does not
// spin" half of the acceptance criterion: the call returns on the planner's
// budget, not on the provider's schedule, and it does not retry.
func TestPlanFallsBackOnProviderTimeout(t *testing.T) {
	prov := plannertest.Slow(30*time.Second, fullDocument)
	p := newPlanner(t, planner.Deps{Provider: prov, Timeout: 30 * time.Millisecond})

	start := time.Now()
	got, err := p.Plan(context.Background(), scopeFor(t, "u-1", projectID), planner.Request{Query: question})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got.Status != planner.StatusFallback || got.Reason != planner.ReasonTimeout {
		t.Fatalf("status = %q reason = %q, want fallback/provider_timeout", got.Status, got.Reason)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Plan took %s for a 30ms budget: it waited on the provider", elapsed)
	}
	if prov.Calls() != 1 {
		t.Errorf("provider calls = %d, want 1 (no retry loop)", prov.Calls())
	}
}

// TestPlanFallsBackOnIdentifierInPlan is the automated negative test docs/54
// §19 requires for scenario #7 ("Search LLM 引用未授权 entity id"), in the
// form the threat actually takes once the vocabulary has no id field: an
// identifier smuggled into a free-text value the schema accepts.
//
// The counter-proof is that the same document WITHOUT the identifier is
// accepted — otherwise this test would pass on a planner that refuses
// everything.
func TestPlanFallsBackOnIdentifierInPlan(t *testing.T) {
	for _, field := range []string{"condition_scope.text", "property.names", "condition_scope.comparators[0].value"} {
		t.Run(field, func(t *testing.T) {
			var doc string
			switch field {
			case "condition_scope.text":
				doc = strings.Replace(fullDocument, `"text": "at 298 K"`, `"text": "the one at `+projectID+`"`, 1)
			case "property.names":
				doc = strings.Replace(fullDocument, `"names": ["CO2 uptake"]`, `"names": ["`+projectID+`"]`, 1)
			case "condition_scope.comparators[0].value":
				doc = strings.Replace(fullDocument, `"value": "3"`, `"value": "`+projectID+`"`, 1)
			}
			prov := plannertest.Reply(doc)
			p := newPlanner(t, planner.Deps{Provider: prov})

			got, err := p.Plan(context.Background(), scopeFor(t, "u-1", projectID), planner.Request{Query: question})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if got.Status != planner.StatusFallback || got.Reason != planner.ReasonIdentifier {
				t.Fatalf("status = %q reason = %q, want fallback/identifier_in_plan", got.Status, got.Reason)
			}
			if got.Document != nil {
				t.Errorf("Document is non-nil on a fallback: %+v", got.Document)
			}
		})
	}

	// The counter-proof: the identifier-free document is planned, so the
	// refusals above are the guard firing and not a planner that refuses
	// every document.
	p := newPlanner(t, planner.Deps{Provider: plannertest.Reply(fullDocument)})
	got, err := p.Plan(context.Background(), scopeFor(t, "u-1", projectID), planner.Request{Query: question})
	if err != nil || !got.Planned() {
		t.Fatalf("the identifier-free document was not planned: err=%v status=%q", err, got.Status)
	}
}

// TestPlanRefusesWithoutActor: no actor is no scope is no search. The planner
// must not plan for nobody, and must not call the provider while doing it —
// the fail-open shape would be "no actor, so plan an unrestricted search".
func TestPlanRefusesWithoutActor(t *testing.T) {
	prov := plannertest.Reply(fullDocument)
	p := newPlanner(t, planner.Deps{Provider: prov})

	_, err := p.Plan(context.Background(), search.Scope{}, planner.Request{Query: question})
	if !errors.Is(err, search.ErrNoActor) {
		t.Fatalf("err = %v, want it to wrap search.ErrNoActor", err)
	}
	if prov.Calls() != 0 {
		t.Errorf("provider calls = %d, want 0: an unauthorized search must not reach the provider", prov.Calls())
	}
}

// TestPlanRefusesEmptyQuery: the contract requires a question
// (specs/api POST /search: required [query]); asking with none is the
// caller's bug, so it is an error rather than a fallback.
func TestPlanRefusesEmptyQuery(t *testing.T) {
	prov := plannertest.Reply(fullDocument)
	p := newPlanner(t, planner.Deps{Provider: prov})

	if _, err := p.Plan(context.Background(), scopeFor(t, "u-1"), planner.Request{}); !errors.Is(err, planner.ErrEmptyQuery) {
		t.Fatalf("err = %v, want planner.ErrEmptyQuery", err)
	}
	if prov.Calls() != 0 {
		t.Errorf("provider calls = %d, want 0", prov.Calls())
	}
}

// TestPlanReturnsCallerCancellation: when the CALLER stops waiting, the
// planner reports that, rather than logging a provider timeout and handing
// back a plan nobody is left to use.
func TestPlanReturnsCallerCancellation(t *testing.T) {
	prov := plannertest.Slow(30*time.Second, fullDocument)
	p := newPlanner(t, planner.Deps{Provider: prov, Timeout: time.Minute})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := p.Plan(ctx, scopeFor(t, "u-1"), planner.Request{Query: question})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got.Status == planner.StatusFallback {
		t.Error("a caller cancellation was reported as a planner fallback")
	}
}

// TestNewRejectsMissingProvider: a planner with no provider is a wiring
// defect, not a deployment that degrades to fallback forever.
func TestNewRejectsMissingProvider(t *testing.T) {
	if _, err := planner.New(planner.Deps{}); !errors.Is(err, planner.ErrNoProvider) {
		t.Fatalf("err = %v, want planner.ErrNoProvider", err)
	}
}

// TestProviderRequestCarriesNothingAboutTheActor pins the port's INPUT SHAPE,
// which is what makes "the provider never learns who is asking" a property of
// the type rather than a promise in a comment: Request has exactly the
// question and the schema, so an adapter cannot be handed a principal, a
// project id or a scope (docs/54 #1/#7; docs/21 §9 keeps authorization in the
// query layer, not in the question).
//
// Adding a field here is a deliberate change with a security review, and this
// test is where it is forced to be deliberate.
func TestProviderRequestCarriesNothingAboutTheActor(t *testing.T) {
	fields := reflect.VisibleFields(reflect.TypeOf(planner.Request{}))
	got := make([]string, 0, len(fields))
	for _, f := range fields {
		got = append(got, f.Name)
	}
	want := []string{"Query", "PlanSchema"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("planner.Request fields = %v, want %v", got, want)
	}

	// And the values it does carry: the question, and the schema the answer
	// must conform to.
	prov := plannertest.Reply(fullDocument)
	p := newPlanner(t, planner.Deps{Provider: prov})
	if _, err := p.Plan(context.Background(), scopeFor(t, "u-1", projectID), planner.Request{Query: question}); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	calls := prov.Requests()
	if len(calls) != 1 {
		t.Fatalf("provider saw %d requests, want 1", len(calls))
	}
	if calls[0].Query != question {
		t.Errorf("request query = %q, want the question verbatim", calls[0].Query)
	}
	schema := string(calls[0].PlanSchema)
	if schema == "" {
		t.Fatal("request carried no plan schema: the provider cannot be asked for schema-constrained output")
	}
	if !strings.Contains(schema, `"intent"`) {
		t.Errorf("the schema sent to the provider does not describe the plan vocabulary: %s", schema[:min(len(schema), 80)])
	}
	// The scope's project id must not appear anywhere in what the provider
	// was given — not in the question, not in the schema.
	if strings.Contains(calls[0].Query, projectID) || strings.Contains(schema, projectID) {
		t.Error("the resolved scope leaked into the provider request")
	}
}

// TestPlanDoesNotRetry: one question, one provider call. A planner that
// retried would multiply the latency the SLO budgets for planning and would
// make the fallback rate uncountable.
func TestPlanDoesNotRetry(t *testing.T) {
	prov := plannertest.Failing(fmt.Errorf("boom"))
	p := newPlanner(t, planner.Deps{Provider: prov})
	for i := 0; i < 3; i++ {
		if _, err := p.Plan(context.Background(), scopeFor(t, "u-1"), planner.Request{Query: question}); err != nil {
			t.Fatalf("Plan: %v", err)
		}
	}
	if prov.Calls() != 3 {
		t.Errorf("provider calls = %d, want 3 (one per Plan call)", prov.Calls())
	}
}
