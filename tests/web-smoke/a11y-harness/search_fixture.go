package main

// The search fixture (T1226): the document the scanned question really
// matches, and the answer provider that turns what retrieval finds into an
// answer WITH citations.
//
// # The gap this closes
//
// `/search` is one of docs/42's core pages and the a11y suite scans it as
// one. What that page renders is decided by the search pipeline, and the
// pipeline has two ready states: an ANSWER (a summary plus the citations it
// leans on) and the structured FALLBACK (no summary, the reason, and the
// sources). The page carries `data-search-answer` in BOTH — it is the ready
// state's marker, not the answered one's — so a route keyed on that marker
// alone counts the fallback as a core page "scanned with data". Before this
// fixture the scan was doing exactly that: retrieval returned zero ranked
// sources for `catalyst`, the generator answered `no_sources`
// (internal/search/answer/generator.go), and the coverage table recorded the
// fallback under 有数据. The marker now names the state the scan claims (see
// a11y-smoke.mjs's route table).
//
// Two pieces of fixture state are needed to reach the cited path, and both
// live here rather than in the product:
//
//  1. A DOCUMENT IN THE PROJECTION. `search_documents` is empty in this
//     harness until something projects the seeded corpus, so there was
//     nothing for any question to find. projectSearchCorpus below runs the
//     production projection (internal/search/rebuild.go — read-only with
//     respect to every source table, one transaction) over the corpus seed()
//     has written, and seedSearchDocument adds one published asset whose
//     title and slug carry the scanned question. The row /search then finds
//     is written by the projection, not hand-built here.
//
//  2. A PROVIDER. Sources alone do not reach the cited path: the generator
//     checks its provider AFTER it has sources
//     (internal/search/answer/generator.go), and cmd/api wires
//     answer.Deps.Provider as nil — a supported deployment state whose every
//     answer is the structured fallback with `no_provider`. So this harness
//     supplies the deterministic provider below, standing in for the model a
//     deployment with an answer model would call. It reaches no network, reads
//     no clock or random source, and answers every request for the same
//     question with the same document — which is what lets the i18n suite
//     compare two loads of the page byte for byte.
//
// # What this fixture does NOT exercise
//
// The fallback branches. `no_sources` is what the i18n suite's second search
// route drives (a question no fixture document matches); `no_provider` is
// what cmd/api's own wiring still produces. The answer package's unit suite
// (internal/search/answer), the schema fixtures (tests/answer) and the
// integration suite (tests/integration/search_answer_test.go) cover the
// provider-shape failures this harness never produces.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/answer"
)

// The scanned question and the document that answers it.
//
// The question is the one both suites already ask (`catalyst`): the a11y
// route table, the keyboard walk's search submit and the i18n route all type
// it, and changing the word here would be a change in three places for no
// gain. What changes is that the corpus now HAS a document matching it.
const (
	searchFixtureQuery   = "catalyst"
	searchFixtureSlug    = "a11y-catalyst"
	searchFixtureTitle   = "Catalyst screening dataset (a11y search fixture)"
	searchFixtureVersion = "1.0.0"
)

// seedSearchDocument inserts the published asset the scanned question matches
// and returns the citation ref the projection will write for it.
//
// The asset is published in the same sense the other fixtures are: a public
// version in the (public) a11y project, which is what makes the projected
// row's visibility public (search.projectedVisibility). No release is named
// as the version's source (`source_release_id` stays NULL) so this fixture
// does not become content of the release page's own fixture.
func (a *api) seedSearchDocument(ctx context.Context, projectID, ownerID string) (string, error) {
	var assetID, pid string
	err := a.pool.QueryRow(ctx, `
		INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		VALUES ('dataset', $1, $2, $3)
		RETURNING id, pid`, searchFixtureSlug, searchFixtureTitle, projectID).Scan(&assetID, &pid)
	if err != nil {
		return "", fmt.Errorf("seed search fixture asset: %w", err)
	}
	manifest := `{"version":1,"asset_type":"dataset","metadata":{"purpose":"a11y search fixture"},"dependency_pins":[]}`
	rights := `{"licenses":[],"holders":[]}`
	_, err = a.pool.Exec(ctx, `
		INSERT INTO research_asset_versions
			(asset_id, version, source_release_id, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		VALUES ($1, $2, NULL, $3, $4, 'public', 'a11y-catalyst-hash', $5, ARRAY['project:' || $6::text])`,
		assetID, searchFixtureVersion, manifest, rights, ownerID, projectID)
	if err != nil {
		return "", fmt.Errorf("seed search fixture asset version: %w", err)
	}
	// The ref the candidate and the citation will carry: the projection's own
	// entity ref (search.EntityRef) with the version facet it writes for an
	// asset (search.PinnedVersion, pinnedVersionFacet).
	return search.EntityRef(search.EntityAsset, pid) + "@" + searchFixtureVersion, nil
}

// projectSearchCorpus writes search_documents from the seeded corpus, with
// the same rebuild an operator runs.
//
// It is REPORTED rather than silent: the count and the per-type breakdown go
// to stderr (stdout carries the READY line the caller parses), so a corpus
// that stopped projecting is visible in the harness log instead of showing up
// later as a page that quietly went back to its fallback.
func (a *api) projectSearchCorpus(ctx context.Context) error {
	report, err := search.NewProjector(a.pool).Rebuild(ctx)
	if err != nil {
		return fmt.Errorf("project the search corpus: %w", err)
	}
	fmt.Fprintf(os.Stderr, "a11y-harness: projected %d search document(s) %v\n",
		report.Projected, report.EntityTypes())
	return nil
}

// fixtureAnswerProvider is the deterministic stand-in for an answer model.
//
// It is a Provider (internal/search/answer/provider.go) and nothing else: the
// generator validates what it returns against the packaged schema and then
// against the grounding guard, so everything this type is trusted with is the
// same trust an adapter for a real model gets. The document it writes names no
// entity — the citations are the only refs in it, taken verbatim from the
// vocabulary the generator passed in — so the guard has nothing to refuse and
// the a11y scan's page shows what an answered search really renders.
type fixtureAnswerProvider struct{}

var _ answer.Provider = fixtureAnswerProvider{}

// AnswerQuestion returns the answer document for req.
//
// An empty vocabulary is an ERROR rather than an empty document: the
// generator does not ask a provider to answer a search with nothing to cite
// (ReasonNoSources — see generator.go), so reaching this branch would mean
// this fixture is writing an answer with no evidence, which is the one thing
// an answer here must never do silently. The error makes the search fall back
// with `provider_error`, which reds the scan naming its cause.
func (fixtureAnswerProvider) AnswerQuestion(ctx context.Context, req answer.Request) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(req.Citable) == 0 {
		return nil, fmt.Errorf("a11y-harness: no citable source for %q", req.Query)
	}
	citations := make([]string, 0, len(req.Citable))
	for _, entity := range req.Citable {
		citations = append(citations, entity.Ref)
	}
	noun := "source"
	if len(citations) != 1 {
		noun = "sources"
	}
	// The schema in internal/search/answer/schema.go bounds this document:
	// three fields, no others, a summary of 1..4000 characters and 1..40
	// unique citations. The sentence states only what the source list shows —
	// how many sources the search returned, and that the ranked list below is
	// what they are — so no entity is named in prose.
	document := map[string]any{
		"answer_version": answer.AnswerVersion,
		"summary": fmt.Sprintf(
			"The search returned %d %s for this question; the citations name every one of them and the ranked list below is what the network holds.",
			len(citations), noun),
		"citations": citations,
	}
	return json.Marshal(document)
}
