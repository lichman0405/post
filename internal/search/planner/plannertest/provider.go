// Package plannertest holds the planner's test doubles: a deterministic
// Provider and an in-memory ProjectScopeReader.
//
// It is test infrastructure shipped as a normal package, the way
// internal/application/authn/oidctest is: the unit suite and the contract
// fixtures (tests/searchplan) exercise the SAME fake, which a test-only
// package could not provide across those two locations. Nothing in this
// package is wired into a binary — cmd/api and cmd/worker do not import it —
// and nothing here reaches a network, reads a clock it was not given, or
// depends on a random source, so the same script always produces the same
// plan.
package plannertest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/search/planner"
)

// ErrScriptExhausted is returned when a request matches no script entry. A
// test that reaches this is a test whose script does not cover what it asked,
// and it must see that rather than an empty document: an empty answer would
// look like "the provider said nothing", which the planner would report as a
// schema violation — a plausible-looking fallback that hides a broken test.
var ErrScriptExhausted = errors.New("plannertest: no script entry matches the request")

// Entry is one scripted answer.
type Entry struct {
	// Match selects the requests this entry answers: the first entry whose
	// Match is a substring of the request's query wins, in the order the
	// entries were given. An empty Match matches any query, which is how a
	// single-answer script is written.
	Match string
	// Document is the plan document to return, verbatim. It is returned
	// unvalidated on purpose: validation is the planner's, and a fake that
	// pre-validated would make every "invalid plan" test a test of the fake.
	Document string
	// Err, when set, is returned instead of Document.
	Err error
	// Delay postpones the answer, bounded by the request's context — the
	// shape a slow or unreachable provider has.
	Delay time.Duration
}

// Provider is a deterministic planner.Provider driven by a script.
//
// It records every request it was given (Requests), so a test can assert what
// the planner SENDS as well as what it accepts — in particular that no
// principal, project id or scope travels to the provider, which is a
// property of the port that would otherwise be asserted only by reading the
// Request struct.
type Provider struct {
	mu     sync.Mutex
	script []Entry
	seen   []planner.Request
	closed bool
}

// New builds a provider answering from the given script.
func New(script ...Entry) *Provider { return &Provider{script: script} }

// PlanQuery implements planner.Provider.
func (p *Provider) PlanQuery(ctx context.Context, req planner.Request) ([]byte, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("plannertest: provider closed")
	}
	p.seen = append(p.seen, req)
	entry, ok := p.match(req.Query)
	p.mu.Unlock()

	if !ok {
		return nil, fmt.Errorf("%w: query %q", ErrScriptExhausted, req.Query)
	}
	if entry.Delay > 0 {
		select {
		case <-time.After(entry.Delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if entry.Err != nil {
		return nil, entry.Err
	}
	return []byte(entry.Document), nil
}

// match returns the first scripted entry for a query. Callers hold p.mu.
func (p *Provider) match(query string) (Entry, bool) {
	for _, e := range p.script {
		if e.Match == "" || strings.Contains(query, e.Match) {
			return e, true
		}
	}
	return Entry{}, false
}

// Requests returns the requests the provider was given, in order. Copies are
// returned so a test cannot rewrite the record it is asserting on.
func (p *Provider) Requests() []planner.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]planner.Request, len(p.seen))
	copy(out, p.seen)
	return out
}

// Calls returns how many requests the provider was given. A planner that
// "never calls the provider" and one that calls it twice are both bugs a test
// can only see with this number.
func (p *Provider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.seen)
}

// Close makes every later call fail, standing in for a provider that is gone.
func (p *Provider) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
}

// Reply builds a single-entry script answering every query with doc.
func Reply(doc string) *Provider { return New(Entry{Document: doc}) }

// Failing builds a single-entry script whose provider always errors.
func Failing(err error) *Provider { return New(Entry{Err: err}) }

// Slow builds a single-entry script that answers doc after delay.
func Slow(delay time.Duration, doc string) *Provider {
	return New(Entry{Document: doc, Delay: delay})
}

// ScopeReader is an in-memory search.ProjectScopeReader: it answers with a
// fixed membership map, so a test can resolve a real search.Scope without a
// database.
//
// It is here rather than in each test because Scope's fields are unexported
// (by design — see internal/search/scope.go) and ResolveScope is the only way
// to obtain one, so every test that needs a scope needs a reader.
type ScopeReader struct {
	// ByUser maps a user id to the projects that user belongs to.
	ByUser map[string][]domain.Project
	// Err, when set, is returned for every lookup — the unreadable-store
	// case, which must fail closed rather than resolve to an empty scope.
	Err error
}

// ListProjectsForUser implements search.ProjectScopeReader.
func (r ScopeReader) ListProjectsForUser(_ context.Context, userID string) ([]domain.Project, error) {
	if r.Err != nil {
		return nil, r.Err
	}
	return r.ByUser[userID], nil
}
