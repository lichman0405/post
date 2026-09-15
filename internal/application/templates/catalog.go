package templates

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// The official template catalog (T0214): six materials-R&D workflow
// templates, each an immutable (id, version) entry. The V1 domain
// coverage follows docs/02 §2 — MOF, gas separation, material screening,
// synthesis, characterization, DFT, MD, GCMC/RASPA, experimental
// validation. The catalog is platform code: an upgrade is a NEW version
// entry (declared alongside the old one; List shows only the latest of
// each id, Resolve pins any version), and existing projects are never
// touched by it.

// OfficialTemplateVersion is the version label of the current official
// template set. Every entry in Official() carries it; an upgrade ships
// new entries under the next label and keeps this one for pinning.
const OfficialTemplateVersion = "v1"

// baseRef builds a canonical base schema pin (schemareg.CanonicalNamespace
// + "<name>.schema.json" at the canonical V1 registry version).
func baseRef(name string) domain.SchemaRef {
	return domain.SchemaRef{
		ID:      schemareg.CanonicalNamespace + name + ".schema.json",
		Version: schemareg.CanonicalV1,
	}
}

// str is a minimal JSON Schema fragment for a free-text custom field.
func str(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

// enumStr is a JSON Schema fragment for a string field restricted to the
// given values (an enum field, never free text).
func enumStr(description string, values ...string) map[string]any {
	vs := make([]any, 0, len(values))
	for _, v := range values {
		vs = append(vs, v)
	}
	return map[string]any{"type": "string", "enum": vs, "description": description}
}

// rules builds a review-defaults document from bool/int/string-list rule
// values (docs/12 §5 rule kinds — the policy service validates the exact
// kind per key, so a template with a mistyped rule fails at application
// time and shows up in the report, never silently).
func rules(pairs ...any) map[string]json.RawMessage {
	if len(pairs)%2 != 0 {
		panic("templates: rules() takes value pairs")
	}
	out := make(map[string]json.RawMessage, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			panic("templates: rules() keys must be strings")
		}
		raw, err := json.Marshal(pairs[i+1])
		if err != nil {
			panic("templates: rules() value is not JSON: " + err.Error())
		}
		out[key] = raw
	}
	return out
}

// question builds one research-map seed node.
func question(statement, purpose string, children ...domain.TemplateQuestion) domain.TemplateQuestion {
	return domain.TemplateQuestion{Statement: statement, Purpose: purpose, Children: children}
}

// Official returns the official catalog: the six V1 templates. Entries of
// one id are declared newest first — the catalog's "latest" is the first
// declared entry, so future upgrades prepend their version and pinning
// keeps working forever.
func Official() *Catalog {
	return NewCatalogMust(
		// Materials Discovery: candidate families → prioritized
		// synthesis → falsifying characterization.
		domain.ProjectTemplate{
			ID:      "materials-discovery",
			Version: OfficialTemplateVersion,
			Name:    "Materials Discovery",
			Description: "Discover and down-select candidate materials for a target application: " +
				"family screening, prioritized synthesis, characterization and experimental validation.",
			Project: domain.TemplateProjectDefaults{
				Name: "Materials Discovery",
				Purpose: "Discover and down-select candidate materials for a target application, " +
					"from family screening through synthesis and characterization to experimental validation.",
				Visibility: domain.VisibilityPrivate,
			},
			Schemas: []domain.TemplateSchemaProfile{
				{
					Name: "material_ext",
					Base: baseRef("material"),
					Properties: map[string]any{
						"synthesis_route":    str("The synthesis route the candidate material was or will be prepared by."),
						"target_application": str("The application the candidate is screened for (e.g. C2H4/C2H6 separation)."),
						"key_property_targets": map[string]any{
							"type": "object", "additionalProperties": true,
							"description": "The target property thresholds the candidate must meet (e.g. selectivity, capacity, stability).",
						},
					},
					Required: []string{"target_application"},
				},
				{
					Name: "experiment_ext",
					Base: baseRef("experiment"),
					Properties: map[string]any{
						"screening_stage":            enumStr("The candidate's screening stage in this project.", "candidate", "promising", "validated"),
						"characterization_technique": str("The primary characterization technique of the experiment (e.g. PXRD, BET, breakthrough)."),
					},
					Required: []string{"screening_stage"},
				},
			},
			Review: domain.TemplateReviewDefaults{Rules: rules(
				domain.RuleMainProtected, true,
				domain.RuleReleaseMinReviewers, 1,
				domain.RulePublicAssetIPReview, true,
			)},
			Map: []domain.TemplateQuestion{
				question(
					"Which material families are the strongest candidates for the target application?",
					"Anchor the search space: the families worth screening first.",
					question(
						"Which candidate materials should be prioritized for synthesis?",
						"Turn the family ranking into a concrete synthesis queue."),
				),
				question(
					"Which characterization experiments can falsify the leading candidates?",
					"Choose the measurements whose failure would eliminate a candidate.",
					question(
						"Which control experiments rule out competing explanations for the observed performance?",
						"Plan controls alongside the primary measurements."),
				),
			},
		},
		// Computational Screening: DFT/force-field/GCMC ranking runs.
		domain.ProjectTemplate{
			ID:      "computational-screening",
			Version: OfficialTemplateVersion,
			Name:    "Computational Screening",
			Description: "Screen candidate materials with computational methods (DFT, force fields, " +
				"GCMC/RASPA) and rank them for experimental follow-up.",
			Project: domain.TemplateProjectDefaults{
				Name: "Computational Screening",
				Purpose: "Screen candidate materials with computational methods (DFT, force fields, GCMC/RASPA) " +
					"and rank them for experimental follow-up.",
				Visibility: domain.VisibilityPrivate,
			},
			Schemas: []domain.TemplateSchemaProfile{
				{
					Name: "calculation_ext",
					Base: baseRef("calculation"),
					Properties: map[string]any{
						"screening_campaign": str("The screening campaign this calculation belongs to."),
						"code_and_version":   str("The code (and version) the calculation ran with, e.g. 'RASPA 2.0.47'."),
						"convergence_notes":  str("Notes on convergence criteria and how they were verified."),
					},
					Required: []string{"screening_campaign", "code_and_version"},
				},
			},
			Review: domain.TemplateReviewDefaults{Rules: rules(
				domain.RuleMainProtected, true,
				domain.RuleReleaseMinReviewers, 1,
			)},
			Map: []domain.TemplateQuestion{
				question(
					"Which candidates survive the computational screen at the required property thresholds?",
					"The screen's verdict: the ranked candidate set that advances."),
				question(
					"How reproducible are the screening results across methods and parameters?",
					"Bound how much of the ranking is method artifact.",
					question(
						"Which calculations need independent re-runs before a candidate advances?",
						"Pin the re-runs that make the ranking trustworthy."),
				),
			},
		},
		// Experimental Validation: synthesis + characterization evidence.
		domain.ProjectTemplate{
			ID:      "experimental-validation",
			Version: OfficialTemplateVersion,
			Name:    "Experimental Validation",
			Description: "Plan, execute and record synthesis and characterization experiments that " +
				"validate or falsify a material hypothesis.",
			Project: domain.TemplateProjectDefaults{
				Name: "Experimental Validation",
				Purpose: "Plan, execute and record synthesis and characterization experiments that " +
					"validate or falsify a material hypothesis.",
				Visibility: domain.VisibilityPrivate,
			},
			Schemas: []domain.TemplateSchemaProfile{
				{
					Name: "experiment_ext",
					Base: baseRef("experiment"),
					Properties: map[string]any{
						"characterization_technique": str("The primary characterization technique (e.g. PXRD, BET, breakthrough, TGA)."),
						"instrument_id":              str("The instrument the experiment ran on (lab book reference)."),
						"calibration_notes":          str("Instrument calibration facts relevant to the result."),
					},
					Required: []string{"characterization_technique"},
				},
				{
					Name: "sample_ext",
					Base: baseRef("sample"),
					Properties: map[string]any{
						"synthesis_batch_id": str("The synthesis batch this sample came from."),
						"storage_conditions": str("How the sample was stored before measurement."),
						"preparation_notes":  str("Sample preparation steps that affect the measurement."),
					},
					Required: []string{"synthesis_batch_id"},
				},
			},
			Review: domain.TemplateReviewDefaults{Rules: rules(
				domain.RuleMainProtected, true,
				domain.RuleReleaseMinReviewers, 1,
				domain.RuleRawDataRetentionDays, 3650,
			)},
			Map: []domain.TemplateQuestion{
				question(
					"Does the experimental evidence support or contradict the working hypothesis?",
					"The project's central validation question."),
				question(
					"Which experiments are required to validate the candidate material?",
					"The measurement plan.",
					question(
						"Which control experiments rule out competing explanations?",
						"Controls that make the primary evidence conclusive."),
				),
			},
		},
		// Paper Reproduction: replicate a published result.
		domain.ProjectTemplate{
			ID:      "paper-reproduction",
			Version: OfficialTemplateVersion,
			Name:    "Paper Reproduction",
			Description: "Reproduce the central results of a published paper and record agreements, " +
				"gaps and deviations transparently.",
			Project: domain.TemplateProjectDefaults{
				Name: "Paper Reproduction",
				Purpose: "Reproduce the central results of a published paper and record agreements, " +
					"gaps and deviations transparently.",
				Visibility: domain.VisibilityPrivate,
			},
			Schemas: []domain.TemplateSchemaProfile{
				{
					Name: "experiment_ext",
					Base: baseRef("experiment"),
					Properties: map[string]any{
						"source_doi":          str("The DOI of the paper this experiment reproduces."),
						"reproduction_status": enumStr("Where this reproduction stands.", "planned", "attempted", "confirmed", "contradicted", "inconclusive"),
						"deviation_notes":     str("Deliberate deviations from the paper's procedure and why."),
					},
					Required: []string{"source_doi", "reproduction_status"},
				},
			},
			Review: domain.TemplateReviewDefaults{Rules: rules(
				domain.RuleMainProtected, true,
				domain.RuleReleaseMinReviewers, 2,
			)},
			Map: []domain.TemplateQuestion{
				question(
					"Can the paper's central claim be reproduced with our methods and resources?",
					"The reproduction target."),
				question(
					"Which methodological details in the paper are missing or ambiguous?",
					"Catalog what the paper does not pin down.",
					question(
						"Which missing details require contacting the authors or re-deriving parameters?",
						"Decide how each gap gets closed."),
				),
			},
		},
		// Dataset Construction: reusable, quality-controlled data.
		domain.ProjectTemplate{
			ID:      "dataset-construction",
			Version: OfficialTemplateVersion,
			Name:    "Dataset Construction",
			Description: "Build, document and quality-control a reusable research dataset from " +
				"experiments and calculations.",
			Project: domain.TemplateProjectDefaults{
				Name: "Dataset Construction",
				Purpose: "Build, document and quality-control a reusable research dataset from " +
					"experiments and calculations.",
				Visibility: domain.VisibilityPrivate,
			},
			Schemas: []domain.TemplateSchemaProfile{
				{
					Name: "dataset_ext",
					Base: baseRef("dataset"),
					Properties: map[string]any{
						"collection_method": str("How the data was collected (instrument pipeline, calculation workflow, manual entry)."),
						"curation_level":    enumStr("The curation stage each record reached.", "raw", "cleaned", "curated"),
						"subject_system":    str("The material or system family the dataset covers."),
					},
					Required: []string{"collection_method", "curation_level"},
				},
			},
			Review: domain.TemplateReviewDefaults{Rules: rules(
				domain.RuleMainProtected, true,
				domain.RuleReleaseMinReviewers, 1,
				domain.RuleRawDataRetentionDays, 3650,
				domain.RulePublicAssetIPReview, true,
			)},
			Map: []domain.TemplateQuestion{
				question(
					"What data must be captured to answer the research question?",
					"Scope the dataset against the question it serves."),
				question(
					"Which quality controls make the dataset citable and reusable?",
					"Define the quality bar.",
					question(
						"Which metadata fields are required for every record?",
						"Pin the metadata schema every record must carry."),
				),
			},
		},
		// Benchmarking: metrics, baselines, candidate comparison.
		domain.ProjectTemplate{
			ID:      "benchmarking",
			Version: OfficialTemplateVersion,
			Name:    "Benchmarking",
			Description: "Define a benchmark with metrics, baselines and evaluation splits, then " +
				"compare candidate methods against it.",
			Project: domain.TemplateProjectDefaults{
				Name: "Benchmarking",
				Purpose: "Define a benchmark with metrics, baselines and evaluation splits, then " +
					"compare candidate methods against it.",
				Visibility: domain.VisibilityPrivate,
			},
			Schemas: []domain.TemplateSchemaProfile{
				{
					Name: "dataset_ext",
					Base: baseRef("dataset"),
					Properties: map[string]any{
						"benchmark_metric": str("The metric this dataset's benchmark scores (e.g. selectivity at 1 bar)."),
						"reference_method": str("The reference method or baseline the benchmark compares against."),
						"evaluation_split": str("The evaluation split (train/test/held-out) this part of the data belongs to."),
					},
					Required: []string{"benchmark_metric", "reference_method"},
				},
			},
			Review: domain.TemplateReviewDefaults{Rules: rules(
				domain.RuleMainProtected, true,
				domain.RuleReleaseMinReviewers, 2,
			)},
			Map: []domain.TemplateQuestion{
				question(
					"Which metrics and baselines define the benchmark?",
					"Fix what 'better' means before any comparison runs."),
				question(
					"How do the candidate methods compare against the reference baselines?",
					"The benchmark's verdict."),
			},
		},
	)
}

// NewCatalogMust builds a catalog from entries, panicking on an invalid
// entry — the official catalog is code-reviewed platform data; a broken
// entry must fail at build/test time, never at request time.
func NewCatalogMust(entries ...domain.ProjectTemplate) *Catalog {
	c, err := NewCatalog(entries...)
	if err != nil {
		panic("templates: invalid official catalog: " + err.Error())
	}
	return c
}

// NewCatalog builds a catalog from entries. It refuses an empty catalog
// (fail closed: an unwired service must not answer "no templates exist"),
// a duplicate (id, version) pair, and any entry whose shape is invalid
// (see validateEntry) — the official catalog is validated here, once, and
// every resolve trusts it afterwards.
func NewCatalog(entries ...domain.ProjectTemplate) (*Catalog, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("%w: catalog is empty", ErrValidation)
	}
	seen := make(map[string]bool, len(entries))
	for _, e := range entries {
		if err := validateEntry(e); err != nil {
			return nil, err
		}
		key := e.ID + "@" + e.Version
		if seen[key] {
			return nil, fmt.Errorf("%w: duplicate template %s version %s", ErrValidation, e.ID, e.Version)
		}
		seen[key] = true
	}
	return &Catalog{entries: entries}, nil
}

// validateEntry checks one template's shape: the recorded labels, the
// display fields, the profile definitions and the question seeds. The
// review rules' per-key kind is the policy service's own check at
// application time (it owns the rule vocabulary), so a catalog entry only
// checks what the catalog itself can know.
func validateEntry(e domain.ProjectTemplate) error {
	if !domain.ValidTemplateID(e.ID) {
		return fmt.Errorf("%w: template id %q must be 1..64 characters of [a-z0-9-], starting with a letter or digit", ErrValidation, e.ID)
	}
	if !domain.ValidTemplateVersion(e.Version) {
		return fmt.Errorf("%w: template %s version %q must be 1..64 characters of [A-Za-z0-9._-]", ErrValidation, e.ID, e.Version)
	}
	if stringsTrimmedEmpty(e.Name) {
		return fmt.Errorf("%w: template %s: name is required", ErrValidation, e.ID)
	}
	if stringsTrimmedEmpty(e.Description) {
		return fmt.Errorf("%w: template %s: description is required", ErrValidation, e.ID)
	}
	if e.Project.Visibility != "" && !domain.ValidProjectVisibility(e.Project.Visibility) {
		return fmt.Errorf("%w: template %s: visibility must be public or private", ErrValidation, e.ID)
	}
	for _, p := range e.Schemas {
		if err := validateProfile(e.ID, p); err != nil {
			return err
		}
	}
	if len(e.Review.Rules) > domain.MaxPolicyRuleCount {
		return fmt.Errorf("%w: template %s: too many review rules (max %d)", ErrValidation, e.ID, domain.MaxPolicyRuleCount)
	}
	for key := range e.Review.Rules {
		if !domain.ValidPolicyRuleKey(key) {
			return fmt.Errorf("%w: template %s: review rule key %q is not a valid policy rule key", ErrValidation, e.ID, key)
		}
	}
	for i := range e.Map {
		if err := validateQuestion(e.ID, e.Map[i]); err != nil {
			return err
		}
	}
	return nil
}

// validateProfile checks one template profile definition: a valid name
// token, a pinned canonical base, and NEW custom field names with object
// fragments (the registration's own rules — mirroring them here makes a
// broken official template fail at catalog build time).
func validateProfile(templateID string, p domain.TemplateSchemaProfile) error {
	if !domain.ValidProfileName(p.Name) {
		return fmt.Errorf("%w: template %s: profile name %q must be lowercase snake_case (a-z, 0-9, _)", ErrValidation, templateID, p.Name)
	}
	if p.Base.ID == "" || p.Base.Version == "" {
		return fmt.Errorf("%w: template %s: profile %s: base schema id and version are required", ErrValidation, templateID, p.Name)
	}
	if !strings.HasPrefix(strings.ToLower(p.Base.ID), strings.ToLower(schemareg.CanonicalNamespace)) {
		return fmt.Errorf("%w: template %s: profile %s: base %q is not an official schema", ErrValidation, templateID, p.Name, p.Base.ID)
	}
	for name, def := range p.Properties {
		if _, ok := def.(map[string]any); !ok {
			return fmt.Errorf("%w: template %s: profile %s: custom property %q must be a JSON Schema fragment (a JSON object)", ErrValidation, templateID, p.Name, name)
		}
	}
	for _, name := range p.Required {
		if _, ok := p.Properties[name]; !ok {
			return fmt.Errorf("%w: template %s: profile %s: required field %q is not among the custom properties", ErrValidation, templateID, p.Name, name)
		}
	}
	return nil
}

// validateQuestion checks one map seed: the statement must be non-trivial
// (the research_question schema requires >= 3 characters), and children
// recurse.
func validateQuestion(templateID string, q domain.TemplateQuestion) error {
	if len(strings.TrimSpace(q.Statement)) < 3 {
		return fmt.Errorf("%w: template %s: map question statement must be at least 3 characters", ErrValidation, templateID)
	}
	for _, c := range q.Children {
		if err := validateQuestion(templateID, c); err != nil {
			return err
		}
	}
	return nil
}

// Catalog is the immutable template set a service resolves against.
type Catalog struct {
	entries []domain.ProjectTemplate
}

// List returns every catalog entry, declaration order. Entries of one id
// are declared newest first, so the first occurrence of an id is its
// latest version.
func (c *Catalog) List() []domain.ProjectTemplate {
	out := make([]domain.ProjectTemplate, len(c.entries))
	copy(out, c.entries)
	return out
}

// LatestIDs returns the newest version of every id, sorted by id.
func (c *Catalog) LatestIDs() []domain.ProjectTemplate {
	first := make(map[string]int, len(c.entries))
	for i, e := range c.entries {
		if _, ok := first[e.ID]; !ok {
			first[e.ID] = i
		}
	}
	out := make([]domain.ProjectTemplate, 0, len(first))
	for _, i := range first {
		out = append(out, c.entries[i])
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// Resolve pins one entry: version "" resolves the latest (first declared)
// version of the id; an explicit version resolves exactly that pair. An
// unknown id or version answers ErrTemplateNotFound.
func (c *Catalog) Resolve(id, version string) (domain.ProjectTemplate, error) {
	if !domain.ValidTemplateID(id) {
		return domain.ProjectTemplate{}, fmt.Errorf("%w: template id must be 1..64 characters of [a-z0-9-], starting with a letter or digit", ErrValidation)
	}
	if version != "" && !domain.ValidTemplateVersion(version) {
		return domain.ProjectTemplate{}, fmt.Errorf("%w: template version must be 1..64 characters of [A-Za-z0-9._-]", ErrValidation)
	}
	for _, e := range c.entries {
		if e.ID != id {
			continue
		}
		if version == "" || e.Version == version {
			return e, nil
		}
	}
	return domain.ProjectTemplate{}, ErrTemplateNotFound
}

// stringsTrimmedEmpty reports whether s is empty after trimming.
func stringsTrimmedEmpty(s string) bool { return strings.TrimSpace(s) == "" }
