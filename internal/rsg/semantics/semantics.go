// Package semantics holds the pure, per-object-type domain semantic checks
// the RSG write services run before a version is stored (docs/08 field
// semantics distilled to rules that are crisp enough to enforce). It never
// reads or writes storage: a check receives the payload (plus, where needed,
// the object's own identity) and answers hard failures or advisory hints.
//
// The boundary between hard and advisory is deliberate and narrow:
//
//   - a HARD failure is a rule whose violation can be decided mechanically
//     from the payload alone (a research question that names itself as its
//     parent, a present-but-empty question reference on either a
//     hypothesis or a research question);
//   - a HINT is a judgement a machine cannot make — most importantly Claim
//     atomicity (docs/08: "一个可独立判断的命题...如果一句话可被 reviewer
//     部分同意部分反对，应拆 Claim"). Atomicity is only ever hinted at,
//     never refused: deciding whether a statement is one claim or several is
//     a scientific judgement, and no NLP heuristic may hard-block a write on
//     it (task acceptance criterion). Hints are returned to the caller as
//     advisory text; they never fail a command.
//
// Every payload-level check operates on the fields the V1 schemas actually
// define (specs/schemas/, the registry's truth): the docs/08 field lists are
// the long-term vocabulary, and where a field has not made it into the V1
// schema (e.g. experiment start/end), there is no semantic check on it —
// the schema stays the single authority on what a payload may carry.
// T0502 adds the structured claim checks (CheckClaimStructure): they run
// on the structured claim model (internal/domain.Claim) whose basis field
// the schema does not carry yet, and the payload path delegates the fields
// it has (see checkClaim).
package semantics

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/lichman0405/post/internal/domain"
)

// Hint is one advisory semantic observation about a payload. It is rendered
// to the caller as text; it never fails a command.
type Hint struct {
	// Code is the stable machine-readable hint id.
	Code string
	// Message is the human explanation (client-safe, no storage detail).
	Message string
}

// Hint codes (the stable vocabulary callers can switch on).
const (
	// HintClaimCompound: the claim statement reads like several independently
	// judgeable propositions — a reviewer might agree with one part and
	// disagree with another, which is exactly the docs/08 splitting signal.
	HintClaimCompound = "CLAIM_MAY_BE_COMPOUND"
	// HintHypothesisQuestionRef: the hypothesis does not name the research
	// question it addresses (docs/08: Hypothesis has question_ref); the
	// draft gate tolerates the absence, the hint says what completing it
	// would buy.
	HintHypothesisQuestionRef = "HYPOTHESIS_MISSING_QUESTION_REF"
	// HintExternalReferenceCanonicalURL: the external reference does not
	// carry a canonical_url yet (docs/19 §2: the live identity's URL).
	// The draft gate tolerates the absence; the hint says what naming it
	// would buy (a refresh can verify what the URL resolves to, and
	// readers can reach the source).
	HintExternalReferenceCanonicalURL = "EXTERNAL_REFERENCE_MISSING_CANONICAL_URL"
)

// Check runs the domain semantic checks for one object type over a payload.
// It returns the hard failures (never nil on failure) and the advisory
// hints (nil when there is nothing to say). ownObjectID names the object
// being written ("" when unknown); the self-reference checks use it.
//
// Hard failures use fixed, client-safe messages (docs/22 §5) — the callers
// wrap them with their own ErrValidation.
func Check(objectType, ownObjectID string, payload map[string]any) (errs []error, hints []Hint) {
	switch objectType {
	case "claim":
		return checkClaim(payload)
	case "research_question":
		return checkResearchQuestion(ownObjectID, payload)
	case "hypothesis":
		return checkHypothesis(payload)
	case "external_reference":
		return checkExternalReference(payload)
	}
	return nil, nil
}

// checkClaim implements the claim checks. Atomicity is advisory by design:
// the check looks for the cheap textual signals of a compound claim —
// sentence terminators separating multiple complete statements, or
// conjunction words joining two independent clauses — and hints. It makes
// no NLP judgement and NEVER hard-fails: whether the statement is one claim
// or several is a reviewer's scientific call (task acceptance criterion:
// "Claim atomicity只做提示不硬 NLP 判断").
//
// On top of atomicity it runs the claim structure checks (T0502,
// CheckClaimStructure) over the payload's own fields. The schema payload
// has no basis field, so the basis arrives empty here: a causal claim
// written through the payload-only path is exactly the missing-basis case
// docs/10 §5 warns about, until a later task gives the write path a basis
// channel (the structured check is already basis-aware).
func checkClaim(payload map[string]any) ([]error, []Hint) {
	var errs []error
	var hints []Hint

	statement, _ := payload["statement"].(string)
	statement = strings.TrimSpace(statement)
	if statement != "" {
		clauses := splitSentences(statement)
		if joinsIndependentClauses(clauses) {
			hints = append(hints, Hint{
				Code: HintClaimCompound,
				Message: "this claim statement reads like several independently judgeable propositions " +
					"(a reviewer could agree with one part and disagree with another). " +
					"docs/08: consider splitting it into one claim per proposition, so each claim stays independently supported or contested.",
			})
		}
	}

	claim := claimFromPayload(payload)
	structErrs, structHints := CheckClaimStructure(claim)
	errs = append(errs, structErrs...)
	hints = append(hints, structHints...)
	return errs, hints
}

// claimFromPayload lifts the claim fields the payload carries into a
// structured claim. Fields the payload cannot carry (the causal basis)
// stay empty — CheckClaimStructure treats that as "not declared", which
// is the missing-basis warning case.
func claimFromPayload(payload map[string]any) domain.Claim {
	var c domain.Claim
	if s, ok := payload["claim_type"].(string); ok {
		c.Type = domain.ClaimType(s)
	}
	c.Scope = payloadRaw(payload, "scope")
	return c
}

// payloadRaw re-encodes one payload value as exact JSON bytes (the
// payload arrived decoded, so a typed round-trip is the way back to the
// stored form).
func payloadRaw(payload map[string]any, key string) json.RawMessage {
	v, ok := payload[key]
	if !ok || v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return raw
}

// splitSentences splits a statement into its sentence-like clauses on
// terminators (. ! ? ;) that end a clause. Cheap textual preprocessing
// keeps the common false-split sources from fragmenting: decimals are
// masked ("3.5" stays one token) and the abbreviation "e.g." is glued. It
// is only the input to the compound-claim hint, never a decision.
func splitSentences(s string) []string {
	// Normalize whitespace into single spaces.
	s = strings.Join(strings.Fields(s), " ")
	// Mask decimals ("3.5 g/cm3") so the period does not split a number,
	// and glue "e.g."/"i.e." so the dot does not split an abbreviation.
	s = decimalRe.ReplaceAllString(s, "N")
	s = egRe.ReplaceAllString(s, "eg")
	parts := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case '.', '!', '?', ';':
			return true
		}
		return false
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// decimalRe matches a decimal number (digits dot digits) for masking.
var decimalRe = regexp.MustCompile(`\d+\.\d+`)

// egRe matches the "e.g."/"i.e." abbreviations (the dot would otherwise
// read as a sentence terminator).
var egRe = regexp.MustCompile(`\b(e\.g|i\.e)\.`)

// joinsIndependentClauses reports whether the statement reads like several
// independently judgeable propositions: at least two substantive
// sentence-like clauses, or one clause that joins two propositions with a
// coordinating conjunction (and/or/but/yet/while/whereas). Fragments (a
// masked decimal's neighbour, an abbreviation) are not counted as clauses.
// The heuristic is deliberately cheap — the output is a hint, and a false
// positive costs nothing (no hard judgement rides on it).
func joinsIndependentClauses(clauses []string) bool {
	var substantive int
	coordinated := false
	for _, c := range clauses {
		words := strings.Fields(c)
		if len(words) < 2 {
			continue // a fragment is not a proposition
		}
		substantive++
		if len(clauses) > 1 && isCoordinator(words[0]) {
			coordinated = true
		}
	}
	if substantive >= 2 {
		return true
	}
	// One sentence with a coordinator joining two independent halves.
	for _, c := range clauses {
		for _, w := range strings.Fields(c) {
			if isCoordinator(w) {
				return true
			}
		}
	}
	return coordinated
}

// isCoordinator reports whether the word is a coordinating conjunction.
func isCoordinator(word string) bool {
	switch strings.ToLower(strings.TrimSuffix(word, ",")) {
	case "and", "or", "but", "yet", "while", "whereas", "however":
		return true
	}
	return false
}

// checkResearchQuestion enforces the self-parent rule: a research question
// cannot be its own parent (docs/08: ResearchQuestion has parent_question;
// a cycle of length one is mechanically wrong wherever the reference
// appears). ownObjectID is the server-generated object id, so the payload
// can only name it by coincidence — the check guards that coincidence and
// every future surface that could let it through.
func checkResearchQuestion(ownObjectID string, payload map[string]any) ([]error, []Hint) {
	if ownObjectID == "" {
		return nil, nil
	}
	parent, ok := payload["parent_question_id"]
	if !ok {
		return nil, nil
	}
	if s, isString := parent.(string); isString {
		// A present-but-empty parent is as meaningless as the empty
		// hypothesis question_id (same rule, same reason): "no parent"
		// is expressed by omitting the field (a root question), never
		// by an empty string. JSON null means absent and stays legal.
		if strings.TrimSpace(s) == "" {
			return []error{fmt.Errorf("semantics: research question parent_question_id must not be empty when present")}, nil
		}
		if s == ownObjectID {
			return []error{fmt.Errorf("semantics: research question cannot be its own parent (parent_question_id names the question itself)")}, nil
		}
	}
	return nil, nil
}

// checkHypothesis implements the hypothesis checks. The question reference
// is advisory only: docs/08 has Hypothesis carry question_ref, but a draft
// may not know the question yet — the hint says what naming it would buy,
// the draft gate remains the authority on whether that matters.
func checkHypothesis(payload map[string]any) ([]error, []Hint) {
	ref, ok := payload["question_id"]
	if !ok {
		return nil, []Hint{{
			Code: HintHypothesisQuestionRef,
			Message: "the hypothesis does not name the research question it addresses (question_id). " +
				"docs/08: naming it lets reviewers trace the hypothesis to the question it answers.",
		}}
	}
	if s, isString := ref.(string); isString && strings.TrimSpace(s) == "" {
		return []error{fmt.Errorf("semantics: hypothesis question_id must not be empty when present")}, nil
	}
	return nil, nil
}

// checkExternalReference implements the external reference checks. The
// identity pair rule (docs/19 §2: source_type + external_identifier is
// the live identity) is a hard error when half of the pair is given
// without the other — a half-identity is mechanically wrong, wherever it
// appears. Both absent is a valid draft (the pr gate demands the pair);
// both present is validated by the schema enum and normalized by the
// 00045 identity sync. The canonical_url absence is advisory: a draft
// may not know the URL yet (mirroring hypothesis's question_id hint).
func checkExternalReference(payload map[string]any) ([]error, []Hint) {
	srcType, hasSrc := nonEmptyString(payload["source_type"])
	extID, hasID := nonEmptyString(payload["external_identifier"])
	var hints []Hint
	if hasSrc != hasID {
		return []error{fmt.Errorf("semantics: external reference source_type and external_identifier must be given together")}, hints
	}
	_, hasURL := nonEmptyString(payload["canonical_url"])
	if hasSrc && !hasURL {
		hints = append(hints, Hint{
			Code: HintExternalReferenceCanonicalURL,
			Message: "the external reference does not name its canonical_url (source_type=" + srcType + ", external_identifier=" + extID + "). " +
				"docs/19 §2: the live identity carries the URL — naming it lets a refresh verify what it resolves to, and readers reach the source.",
		})
	}
	return nil, hints
}

// nonEmptyString reports the trimmed string value of v and whether it is
// present and non-empty. The trim set is domain.ASCIIWhitespace — the
// same explicit ASCII set the 00045 identity sync trims with, so a
// tab-only value is blank in both worlds. Unicode-aware TrimSpace must
// not be used here: the two emptiness judgements are pinned together by
// the integration drift test and must never diverge.
func nonEmptyString(v any) (string, bool) {
	s, ok := v.(string)
	if !ok {
		return "", false
	}
	s = strings.Trim(s, domain.ASCIIWhitespace)
	return s, s != ""
}
