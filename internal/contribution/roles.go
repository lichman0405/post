package contribution

import (
	"fmt"
	"sort"
	"strings"
)

// ContributionRole is one value of the Contribution Role vocabulary —
// docs/04_USERS_ROLES.md §4 "Contribution Role（实际上做了什么）", the
// thirteen things a contribution event may be tagged with (:30-42, in the
// document's order). A contribution event MAY carry several of them
// ("Contribution Event 可标注多个角色"): the roles say what the person
// actually did in that event, nothing more.
//
// The document pins the vocabulary's nature in the sentence that follows
// the list (:44): "这些不是身份，不赋权，不计固定分值" — these are not
// identities, they grant nothing, and they carry no fixed score value.
// Two consequences the code must keep, both of them structural rather
// than stylistic:
//
//   - role_codes is RECORDED, never enforced: no permission, routing or
//     authorization decision may read it. What a person may do is the
//     Access Role / responsibility matrix (docs/04 §2-§3, authz), which is
//     a different axis entirely — "责任用于 Review routing，不自动赋予更高
//     访问权限" (:37).
//   - the codes carry no weight. There is no scoring function over this
//     vocabulary anywhere, and there must not be one: docs/13 §4 forbids a
//     single reputation score, and CLAUDE.md §9 invariant 13 forbids a
//     Truth Score / Research Score. A Reputation Profile shows several
//     dimensions (docs/13 §4), and the ledger's job is to record the facts
//     those dimensions are read from.
//
// The code spellings are the snake_case form of the document's own labels
// (Label is the label verbatim), so the vocabulary has exactly one source
// and ContributionRoles is checkable against it — the drift test in
// tests/integration pins the two together rather than trusting this file.
//
// The vocabulary lives HERE and not in the database: role_codes carries no
// CHECK, by the convention four migrations state verbatim for their own
// vocabularies (00064, 00066, 00067, 00045 — "duplicating that shape as a
// CHECK here would mean two definitions ... drifting apart").
type ContributionRole string

const (
	// RoleResearchQuestionProposal is docs/04's "Research Question
	// Proposal": the contribution states a research question.
	RoleResearchQuestionProposal ContributionRole = "research_question_proposal"
	// RoleHypothesisProposal is "Hypothesis Proposal".
	RoleHypothesisProposal ContributionRole = "hypothesis_proposal"
	// RoleResearchDesign is "Research Design".
	RoleResearchDesign ContributionRole = "research_design"
	// RoleMethodDevelopment is "Method Development" — designing a method
	// or protocol.
	RoleMethodDevelopment ContributionRole = "method_development"
	// RoleExperimentalInvestigation is "Experimental Investigation" —
	// running bench/lab work.
	RoleExperimentalInvestigation ContributionRole = "experimental_investigation"
	// RoleComputationalInvestigation is "Computational Investigation" —
	// running/recording a calculation.
	RoleComputationalInvestigation ContributionRole = "computational_investigation"
	// RoleDataCuration is "Data Curation" — producing or curating a
	// dataset.
	RoleDataCuration ContributionRole = "data_curation"
	// RoleAnalysis is "Analysis" — deriving claims/findings from data.
	RoleAnalysis ContributionRole = "analysis"
	// RoleValidation is "Validation" — testing a claim against evidence.
	RoleValidation ContributionRole = "validation"
	// RoleReproduction is "Reproduction" — independently reproducing a
	// result.
	RoleReproduction ContributionRole = "reproduction"
	// RoleScientificReview is "Scientific Review" — reviewing a proposal.
	RoleScientificReview ContributionRole = "scientific_review"
	// RoleAssetStewardship is "Asset Stewardship" — publishing and
	// maintaining released assets.
	RoleAssetStewardship ContributionRole = "asset_stewardship"
	// RoleProjectMaintenance is "Project Maintenance" — the upkeep work
	// that keeps a project running.
	//
	// It is part of the docs/04 §4 vocabulary (the document's ninth
	// contribution role, and docs/13 §1 lists the maintenance category
	// among the acts the ledger records), and it is NOT dead: the label
	// table below carries it, ContributionRoles enumerates that table, and
	// Label/Valid/ParseContributionRole all answer out of it.
	//
	// What is missing today is an EVENT that produces it: no event in
	// specs/events/event-types.yaml records maintenance work, and no row of
	// the projection's mapping table (internal/contribution/ledger.go)
	// resolves to this role. So no ledger row carries it, and none can
	// until such an event exists — at which point the event either gets a
	// mapping (and this role is what it records) or does not, and is then
	// COUNTED as an unmapped type in every projection report rather than
	// being projected under a role invented to cover the gap. Inventing an
	// event name for maintenance is a spec change and belongs to whoever
	// owns specs/events/**, not here.
	RoleProjectMaintenance ContributionRole = "project_maintenance"
)

// contributionRoleLabels is the document's label for each code
// (docs/04_USERS_ROLES.md:30-42, verbatim). It is the bridge the drift
// test and any human reader use to check the two copies against each
// other; the ledger never renders it (the UI owns presentation).
var contributionRoleLabels = map[ContributionRole]string{
	RoleResearchQuestionProposal:   "Research Question Proposal",
	RoleHypothesisProposal:         "Hypothesis Proposal",
	RoleResearchDesign:             "Research Design",
	RoleMethodDevelopment:          "Method Development",
	RoleExperimentalInvestigation:  "Experimental Investigation",
	RoleComputationalInvestigation: "Computational Investigation",
	RoleDataCuration:               "Data Curation",
	RoleAnalysis:                   "Analysis",
	RoleValidation:                 "Validation",
	RoleReproduction:               "Reproduction",
	RoleScientificReview:           "Scientific Review",
	RoleAssetStewardship:           "Asset Stewardship",
	RoleProjectMaintenance:         "Project Maintenance",
}

// ContributionRoles returns the thirteen canonical roles, sorted by code.
// The list is the vocabulary's single enumeration — the projection's
// mapping table and the tests both read it from here.
func ContributionRoles() []ContributionRole {
	out := make([]ContributionRole, 0, len(contributionRoleLabels))
	for r := range contributionRoleLabels {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Label returns the docs/04 label of a role ("" for a value outside the
// vocabulary).
func (r ContributionRole) Label() string { return contributionRoleLabels[r] }

// Valid reports whether r is one of the thirteen canonical roles.
func (r ContributionRole) Valid() bool {
	_, ok := contributionRoleLabels[r]
	return ok
}

// ValidContributionRoles reports whether every code in rs is canonical and
// no code repeats. The projection builds the slice it writes from its own
// mapping table, so this is the check that a table entry cannot smuggle in
// a value docs/04 does not define.
func ValidContributionRoles(rs []ContributionRole) bool {
	seen := make(map[ContributionRole]bool, len(rs))
	for _, r := range rs {
		if !r.Valid() || seen[r] {
			return false
		}
		seen[r] = true
	}
	return true
}

// ParseContributionRole resolves a code to its canonical role; ok is false
// for any value outside the vocabulary.
func ParseContributionRole(code string) (ContributionRole, bool) {
	r := ContributionRole(code)
	return r, r.Valid()
}

// FormatContributionRoles renders role codes as the comma-separated text
// form used in error messages and logs (sorted, so a message is stable
// whatever order the caller built the slice in).
func FormatContributionRoles(rs []ContributionRole) string {
	codes := make([]string, 0, len(rs))
	for _, r := range rs {
		codes = append(codes, string(r))
	}
	sort.Strings(codes)
	return strings.Join(codes, ",")
}

// RoleVocabularyError reports the first code outside the vocabulary, with
// the canonical set in the message — an unknown role is a programming
// error in the mapping table, not user input, and the message has to say
// enough to fix it.
func RoleVocabularyError(rs []ContributionRole) error {
	for _, r := range rs {
		if !r.Valid() {
			return fmt.Errorf("contribution: %q is not a docs/04 §4 contribution role (canonical: %s)",
				string(r), FormatContributionRoles(ContributionRoles()))
		}
	}
	return nil
}
