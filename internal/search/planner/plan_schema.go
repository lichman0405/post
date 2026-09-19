package planner

import (
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The plan schema lives here, as a Go literal, and not under specs/.
//
// Why not specs/: specs/** is the product's published surface (and, per
// CLAUDE.md §8.1, Supervisor-owned — the single generated exception is the
// schema snapshot). A search plan is an INTERNAL contract between the planner
// and the retrieval layer inside this service: it is never served, never
// referenced by a client, and specs/schemas/ holds entity schemas, not
// request-interior documents.
//
// Why a literal and not a file: the schema is the validation authority for
// what a language model may say, so it must be impossible for the compiled
// schema and the deployed binary to disagree. Embedding keeps them one
// artifact — there is no path in which a rollout ships new code against a
// stale schema file.
//
// The validator itself is NOT new: github.com/santhosh-tekuri/jsonschema/v6
// is already a direct dependency (go.mod) used by internal/rsg/schemareg,
// and this compiles with the same configuration that registry uses
// (Draft 2020-12, format assertions on, every $ref local). internal/rsg/** is
// read-only for this task, so the compile is done here rather than by calling
// into the registry — the registry's job is versioned entity schemas with
// content hashes and a persistence load path, none of which a plan needs.
//
// additionalProperties:false is on EVERY object in this schema, at every
// depth, and that is the point rather than a style: an unknown key is how a
// provider would attach something the vocabulary does not have — a list of
// candidate `entity_ids` above all (docs/54 #7). A document carrying one is
// refused and the search falls back to structured results, rather than the
// service quietly ignoring a field it does not understand.
const planSchemaJSON = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://post.local/schemas/search-plan/1.json",
  "title": "POST search plan (docs/14 §2)",
  "type": "object",
  "additionalProperties": false,
  "required": ["plan_version", "intent", "target_object", "network_scope", "visibility"],
  "properties": {
    "plan_version": {
      "const": "1",
      "description": "The plan vocabulary version. Pinned so a plan stored by the search API stays interpretable after the vocabulary moves (docs/21 §8)."
    },
    "intent": {
      "type": "string",
      "enum": ["answer", "compare", "conditions", "origin_assessment"],
      "description": "What the asker wants, from the facets docs/14 §4 requires the answer page to carry."
    },
    "target_object": {
      "type": "object",
      "additionalProperties": false,
      "required": ["entity_types"],
      "properties": {
        "entity_types": {
          "type": "array",
          "minItems": 1,
          "maxItems": 4,
          "uniqueItems": true,
          "items": {
            "type": "string",
            "enum": ["asset", "knowledge", "release", "state"]
          },
          "description": "The search projection's entity vocabulary: the kinds search_documents actually holds. A plan cannot name a kind the index does not have."
        }
      }
    },
    "property": {
      "type": "object",
      "additionalProperties": false,
      "required": ["names"],
      "properties": {
        "names": {
          "type": "array",
          "minItems": 1,
          "maxItems": 8,
          "uniqueItems": true,
          "items": {"type": "string", "minLength": 1, "maxLength": 80}
        }
      }
    },
    "condition_scope": {
      "type": "object",
      "additionalProperties": false,
      "anyOf": [{"required": ["text"]}, {"required": ["comparators"]}],
      "properties": {
        "text": {
          "type": "string",
          "minLength": 1,
          "maxLength": 240,
          "description": "The condition as the question phrased it, kept when the comparator vocabulary cannot express it."
        },
        "comparators": {
          "type": "array",
          "minItems": 1,
          "maxItems": 8,
          "items": {
            "type": "object",
            "additionalProperties": false,
            "required": ["property", "op", "value"],
            "properties": {
              "property": {"type": "string", "minLength": 1, "maxLength": 80},
              "op": {"type": "string", "enum": ["lt", "lte", "eq", "gte", "gt"]},
              "value": {"type": "string", "minLength": 1, "maxLength": 80},
              "unit": {"type": "string", "minLength": 1, "maxLength": 24}
            }
          }
        }
      }
    },
    "evidence_preference": {
      "type": "object",
      "additionalProperties": false,
      "anyOf": [{"required": ["types"]}, {"required": ["prefer"]}],
      "properties": {
        "types": {
          "type": "array",
          "minItems": 1,
          "maxItems": 6,
          "uniqueItems": true,
          "items": {
            "type": "string",
            "enum": ["experimental", "computational", "dataset", "literature", "external_attestation", "other"]
          },
          "description": "domain.CanonicalEvidenceTypes (docs/10 §3): the evidence kinds the platform already names. A plan cannot invent an evidence kind."
        },
        "prefer": {
          "type": "array",
          "minItems": 1,
          "maxItems": 3,
          "uniqueItems": true,
          "items": {
            "type": "string",
            "enum": ["reviewed", "independently_reproduced", "contradictory"]
          }
        }
      }
    },
    "network_scope": {
      "type": "string",
      "enum": ["platform", "platform_and_external"],
      "description": "docs/14 §1: the platform network, plus external references formally taken into a project. There is no whole-internet scope to ask for."
    },
    "visibility": {
      "type": "string",
      "enum": ["public", "accessible"],
      "description": "A narrowing hint, never a grant: 'accessible' is whatever the searcher's scope already allows, 'public' narrows that to rows anyone may read."
    },
    "ranking_constraints": {
      "type": "object",
      "additionalProperties": false,
      "required": ["order_by"],
      "properties": {
        "order_by": {
          "type": "array",
          "minItems": 1,
          "maxItems": 6,
          "uniqueItems": true,
          "items": {
            "type": "string",
            "enum": ["query_scope_match", "evidence_profile", "review_state", "independent_reproduction", "contradictory_evidence", "version_freshness"]
          },
          "description": "docs/14 §3's ranking criteria, verbatim. Its prohibition is encoded by omission: popularity, stars and organization prestige are not in this list, and no weight or score can be asked for (CLAUDE.md §9.13)."
        }
      }
    }
  }
}`

// planSchemaID is the compiled schema's URL; it is also the $id the literal
// declares (the compiler refuses the pair if they disagree).
const planSchemaID = "https://post.local/schemas/search-plan/1.json"

// PlanSchema returns a copy of the schema document bytes. It exists so a
// reviewer, a test or a future plan store can see exactly what the planner
// validates against without reconstructing it from this file's source.
func PlanSchema() []byte {
	out := make([]byte, len(planSchemaJSON))
	copy(out, planSchemaJSON)
	return out
}

// compilePlanSchema compiles the packaged schema with the registry's own
// configuration (internal/rsg/schemareg: Draft 2020-12, AssertFormat, local
// $refs only — this document has none, and nothing here resolves anything
// over a network).
//
// A failure is a broken literal, which is a defect in this repository rather
// than a runtime condition: the Planner refuses to be constructed, so a bad
// schema can never degrade into "every plan falls back forever".
func compilePlanSchema() (*jsonschema.Schema, error) {
	var doc any
	if err := json.Unmarshal([]byte(planSchemaJSON), &doc); err != nil {
		return nil, fmt.Errorf("planner: plan schema is not valid JSON: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	if err := c.AddResource(planSchemaID, doc); err != nil {
		return nil, fmt.Errorf("planner: add plan schema: %w", err)
	}
	compiled, err := c.Compile(planSchemaID)
	if err != nil {
		return nil, fmt.Errorf("planner: compile plan schema: %w", err)
	}
	return compiled, nil
}
