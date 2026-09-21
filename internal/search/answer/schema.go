package answer

import (
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The answer schema lives here, as a Go literal, for the reasons
// planner/plan_schema.go gives for its own: it is an INTERNAL contract
// between this package and an adapter (never served, never a specs/ entity
// schema), and a literal makes it impossible for the compiled schema and the
// deployed binary to disagree.
//
// # What the schema can and cannot do
//
// It bounds the document SHAPE: three fields, no others, with the sizes a
// provider may write. It cannot say the one thing that matters most — that
// every ref in `citations` is an entity retrieval returned — because that is
// a property of this answer's candidate set and not of the document. The
// schema is therefore necessary and not sufficient, and grounding.go is the
// other half: a document that passes here can still be refused there, and
// the fallback says which check refused it.
//
// # Why there is no limitations/conflicts field
//
// docs/14 §4 wants an answer to show conflicts and limits, and this package
// derives both from the platform's own facts (derive.go) rather than asking
// a model to write them. Two reasons, and the second is the load-bearing one:
//
//   - "Nothing contradicts this version" is a statement about every row the
//     platform read, and a model that never saw them cannot make it. A
//     conflict list written by a model would be a list of the conflicts it
//     happened to think of, printed under a heading that promises more.
//   - docs/14 §4's "不创造新 Claim" (create no new claims) rules out the
//     form itself. "A contradicts B" IS a claim — a relation the platform
//     has not recorded — and it is the exact shape of hallucination this
//     pipeline exists to prevent. A model may phrase what the sources show;
//     it may not add an edge to the graph by writing a sentence.
//
// So the provider document is a summary and the citations it leans on. That
// is also why the schema is small enough to review in one screen, which a
// schema for model-authored scientific prose would not be.
const answerSchemaJSON = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://post.local/schemas/search-answer/1.json",
  "title": "POST evidence-backed answer document (ADR-010, docs/14 §4)",
  "type": "object",
  "additionalProperties": false,
  "required": ["answer_version", "summary", "citations"],
  "properties": {
    "answer_version": {
      "const": "1",
      "description": "The answer vocabulary version, pinned so a stored answer stays interpretable after the vocabulary moves (docs/21 §8)."
    },
    "summary": {
      "type": "string",
      "minLength": 1,
      "maxLength": 4000,
      "description": "The direct answer, in the provider's own words. It is rendered as a View (Answer.AnswerView) and must state nothing the cited sources do not show: any entity it names must be a ref from Citable, and the guard refuses the document otherwise."
    },
    "citations": {
      "type": "array",
      "minItems": 1,
      "maxItems": 40,
      "uniqueItems": true,
      "items": {
        "type": "string",
        "minLength": 1,
        "maxLength": 200
      },
      "description": "The refs the summary relies on, each one verbatim from Request.Citable. minItems is 1 because a summary that leans on nothing is ungrounded prose: 'the evidence shows' with no evidence named is exactly what docs/32's risk register calls a scientific-looking hallucination, and the honest answer for a search with nothing to cite is the structured fallback (ReasonNoSources), which this package reaches without asking a provider at all."
    }
  }
}`

// answerSchemaID is the compiled schema's URL; it is also the $id the literal
// declares (the compiler refuses the pair if they disagree).
const answerSchemaID = "https://post.local/schemas/search-answer/1.json"

// AnswerSchema returns a copy of the schema document bytes, so an adapter can
// ask a provider for schema-constrained output without importing this
// package's types.
func AnswerSchema() []byte {
	out := make([]byte, len(answerSchemaJSON))
	copy(out, answerSchemaJSON)
	return out
}

// compileAnswerSchema compiles the packaged schema. It uses the same
// validator and configuration as the planner's and the entity schema
// registry's (github.com/santhosh-tekuri/jsonschema/v6, Draft 2020-12,
// format assertions on, local $refs only — this document has none).
//
// A failure is a broken literal and so a defect in this repository rather
// than a runtime condition: the Generator refuses to be constructed, so a bad
// schema can never degrade into "every answer falls back forever".
func compileAnswerSchema() (*jsonschema.Schema, error) {
	var doc any
	if err := json.Unmarshal([]byte(answerSchemaJSON), &doc); err != nil {
		return nil, fmt.Errorf("answer: answer schema is not valid JSON: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource(answerSchemaID, doc); err != nil {
		return nil, fmt.Errorf("answer: add answer schema: %w", err)
	}
	compiled, err := c.Compile(answerSchemaID)
	if err != nil {
		return nil, fmt.Errorf("answer: compile answer schema: %w", err)
	}
	return compiled, nil
}

// document is the decoded provider document. It mirrors the schema exactly;
// the decode is checked against the schema first, so a mismatch between the
// two is a bug in this file rather than an input error.
type document struct {
	AnswerVersion string   `json:"answer_version"`
	Summary       string   `json:"summary"`
	Citations     []string `json:"citations"`
}
