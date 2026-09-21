package answer

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// This file is the acceptance criterion of T0906: "构造 hallucination test：
// 模型不可引用不存在 id". It is the half of the answer contract a schema
// cannot express — a JSON Schema can say a citation is a string, and only
// this file can say it is a string retrieval returned.
//
// # What grounds an answer
//
// The candidate set, and nothing else. `citations` must be a subset of it,
// token by token, and the summary may not NAME an entity outside it either:
// an answer that says "MOF-5 was reproduced by CLM-9f2c…" about a claim
// nobody retrieved is the same defect whether the id sits in a citation
// array or in a sentence, and a reader checking the sentence against the
// source list would find nothing to check it against.
//
// # Why a violation drops the whole document
//
// The alternative is pruning: drop the offending citation and keep the
// summary. That is worse than the fallback in both directions. The summary
// still says what it said — "three groups reproduced this [1][2]" becomes a
// sentence with a hole in it, and the hole is exactly where the fabricated
// evidence was — and the answer would then be published as an ANSWERED
// answer, marked as grounded, when the one thing we know about it is that its
// author invented a source. docs/32's mitigation is not "filter the model's
// output"; it is "fallback structured results", and this is where that
// decision is executed.
//
// # What this guard is not
//
// It is not a check that the summary is TRUE of the sources it cites. "Mg-
// MOF-74 has the highest uptake" cited against a real source that says the
// opposite is a defect this guard cannot see, and no guard over ids could:
// judging it needs the full texts, which the answer layer does not carry
// (provider.go, CitableEntity). What the guard enforces is the property
// docs/31 Gate E states — the answer cites versions of REAL entities, and
// never a source that does not exist — and the summary's status as a View
// (docs/14 §4) is what stands behind the rest: a reader can open every source
// it cites, which is what a citation is for.

// identityToken matches the platform's entity identity form: a uuid, the
// primary key every entity table in this repository uses (docs/21 §8). It is
// the same expression planner.go's identifier guard uses, for the same
// reason, and it is matched ANYWHERE inside a token so that a uuid embedded
// in a sentence is found.
var identityToken = regexp.MustCompile(
	`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// refKindPrefixes is the platform's citation grammar: every prefix
// search.EntityRef can produce (the projection's entity types) plus the kind
// the traversal's object versions are cited under. A token that opens with
// one of these and a colon is a REF — spelled by the platform, matched
// against the platform's answer — and a ref-shaped token that is not in the
// vocabulary is a fabricated citation whether or not it contains a uuid (a
// pid-shaped identity has no uuid in it at all: "asset:AST-7f3a…" is a real
// citation shape and a completely invented identity).
//
// It is derived from the vocabulary rather than written out, so a projection
// that gains an entity type cannot leave this guard behind.
func refKindPrefixes() []string {
	kinds := append([]string{}, search.EntityTypes()...)
	kinds = append(kinds, retrieval.RefKindObjectVersion)
	sort.Strings(kinds)
	return kinds
}

// vocabulary is the citation vocabulary of one answer: what retrieval
// returned, in the three shapes a summary can name it by.
//
//   - refs: the exact citation refs ("kind:identity@version"). This is the
//     only shape a CITATION may be written in, and it is matched exactly —
//     so "asset:AST-1@2" is grounded and "asset:AST-1@3" is not, even though
//     both name a real asset. ADR-010's whole point is that an answer cites a
//     VERSION, and a citation the platform did not return names a version
//     nobody read.
//   - identities: the entities' own identities, lowercased (a pid, a row id,
//     an object version uuid). A summary naming an entity it is citing in
//     passing — "the reproduction by <uuid>" — is naming something retrieval
//     DID return, and refusing it would fail a legitimate answer for a
//     cosmetic reason.
//   - ids: the scientific object and object-version uuids behind the
//     candidates, lowercased. They travel with a candidate
//     (retrieval.Candidate.ObjectID / ObjectVersionID) and are the ids a
//     model is most likely to have seen in a title.
type vocabulary struct {
	refs       map[string]bool
	identities map[string]bool
	ids        map[string]bool
}

// vocabularyFor builds the vocabulary of a ranking result: the refs it
// returned, and the identities behind them.
//
// The ranked candidates are the input rather than the retrieval result,
// because the ranking is what this layer is handed (generator.go) and the
// thing a caller could otherwise disagree with. Every ref that reaches a
// Source is in this set by construction, so the guard's answer and the
// answer's source list cannot drift apart.
func vocabularyFor(ranked []ranking.Ranked) vocabulary {
	v := vocabulary{
		refs:       make(map[string]bool, len(ranked)),
		identities: make(map[string]bool, len(ranked)),
		ids:        make(map[string]bool, 2*len(ranked)),
	}
	add := func(set map[string]bool, value string) {
		if value != "" {
			set[strings.ToLower(value)] = true
		}
	}
	for _, r := range ranked {
		if r.Ref != "" {
			v.refs[r.Ref] = true
		}
		c := r.Candidate
		add(v.identities, c.Identity)
		add(v.ids, c.ObjectID)
		add(v.ids, c.ObjectVersionID)
		add(v.ids, r.ObjectVersionID)
	}
	return v
}

// holds reports whether a token names something retrieval returned, in any of
// the three shapes.
func (v vocabulary) holds(token string) bool {
	if v.refs[token] {
		return true
	}
	lower := strings.ToLower(token)
	return v.identities[lower] || v.ids[lower]
}

// ungrounded returns the tokens of text that NAME an entity the vocabulary
// does not hold, in the order they appear and without duplicates.
//
// A token is a maximal run of non-space characters, trimmed of the
// punctuation that surrounds it in a sentence ("(asset:A@1)." → "asset:A@1",
// which needs both the ')' and the '.' removed, so the trim is a single pass
// over one cutset rather than one rule per character). Trimming is
// deliberately conservative: ':','@','-','/','_' and ',' are kept INSIDE a
// token, so a ref, a uuid and a URL each stay one token, and only the
// characters that can never be part of one are removed from the ends. A '.'
// at the END is punctuation and is removed; a '.' inside is left alone
// ("doi:10.1021/ja00001a001" survives intact).
//
// A token names an entity when it
//
//  1. contains '@' — a version-pinned citation, the shape
//     examples/search-answer.example.json renders ("CLM-DEMO-001@2");
//  2. opens with a platform ref kind and a colon ("knowledge:…",
//     "object_version:…") — a citation spelled in the platform's own grammar;
//  3. contains a uuid anywhere — an identity, whatever it is wrapped in;
//  4. contains "://" — a link to something outside the corpus. The answer's
//     evidence is the retrieved sources (docs/14 §1: the platform network and
//     the external references formally taken into a project, and nothing
//     else), so a URL in a summary is an uncitable source written in prose.
//
// and is not itself in the vocabulary. Anything else is a word: chemical
// formulas, temperatures ("70%"), ratios ("1:2"), DOIs without a scheme and
// dates are all ordinary prose, and a guard that refused them would refuse
// the answers this pipeline exists to produce. The boundary is stated rather
// than implied: a bare invented token like "CLM-999" (no '@', no colon with a
// platform kind, no uuid) is NOT refused by this guard. It is not a citation
// the platform can render — ADR-010's citations are version-pinned — and it
// cannot reach Answer.Citations, which is matched exactly against the
// vocabulary; what it would be is a name in a sentence, and the sentence is a
// View.
func (v vocabulary) ungrounded(text string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, raw := range strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			return true
		}
		return false
	}) {
		token := strings.Trim(raw, `()[]{}<>"“”'‘’«»,;!?*|.`)
		if token == "" || v.holds(token) {
			continue
		}
		if !namesEntity(token) {
			continue
		}
		if !seen[token] {
			seen[token] = true
			out = append(out, token)
		}
	}
	return out
}

// namesEntity reports whether a token is one of the four shapes above.
func namesEntity(token string) bool {
	if strings.ContainsRune(token, '@') {
		return true
	}
	if strings.Contains(token, "://") {
		return true
	}
	if identityToken.MatchString(token) {
		return true
	}
	prefix, _, found := strings.Cut(token, ":")
	if !found {
		return false
	}
	for _, kind := range refKindPrefixes() {
		if prefix == kind {
			return true
		}
	}
	return false
}

// violation names one reason a document was refused, so the fallback's log
// line and the caller's reason say which check failed rather than "the model
// was wrong".
type violation struct {
	// Kind is "citation" (a citations[] entry outside the vocabulary) or
	// "token" (an entity named in the summary).
	Kind string
	// Value is the offending token, verbatim.
	Value string
}

const (
	violationCitation = "citation"
	violationToken    = "token"
)

// String renders the violation for a log line: what was written and where.
func (v violation) String() string {
	return fmt.Sprintf("%s %q", v.Kind, v.Value)
}

// checkDocument applies the guard to a schema-valid document and returns the
// violations, in a deterministic order: citations first (the explicit
// claims), then the tokens of the summary in the order written.
//
// An empty result is the grounded case. There is no partial credit and no
// repair: generator.go falls back on the first violation, and the list is
// there so the log line can name all of them at once.
func checkDocument(doc document, vocab vocabulary) []violation {
	var out []violation
	seen := make(map[string]bool)
	record := func(kind, value string) {
		key := kind + "\x00" + value
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, violation{Kind: kind, Value: value})
	}
	for _, ref := range doc.Citations {
		if !vocab.refs[ref] {
			record(violationCitation, ref)
		}
	}
	for _, token := range vocab.ungrounded(doc.Summary) {
		record(violationToken, token)
	}
	return out
}

// violationsError renders a violation list for the log, bounded: a run of
// model output is not a log line, and the first few names are enough to
// recognise the failure (the same reasoning planner.truncateForLog gives).
func violationsError(violations []violation) string {
	if len(violations) == 0 {
		return ""
	}
	const max = 5
	parts := make([]string, 0, max)
	for i, v := range violations {
		if i == max {
			parts = append(parts, fmt.Sprintf("and %d more", len(violations)-max))
			break
		}
		parts = append(parts, v.String())
	}
	return strings.Join(parts, "; ")
}
