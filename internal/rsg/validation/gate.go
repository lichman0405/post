package validation

import "fmt"

// Gate names one validation checkpoint of the progressive ladder. The five
// values are the canonical set of docs/22 §7 plus the draft gate: draft is
// the baseline a branch's everyday commits are held to (docs/08: a Draft
// may lack domain fields), pr/main/release/asset are the gates the
// validate endpoint exposes (specs/api/openapi.yaml: gate enum
// pr|main|release|asset). Later gates are strictly stricter: every check a
// gate blocks on, the next gate blocks on too, and the next gate adds or
// promotes at least one more (GateSpecs).
type Gate string

const (
	// GateDraft is the loosest gate: structural validity only, with
	// domain-field gaps and incomplete provenance reported as warnings.
	GateDraft Gate = "draft"
	// GatePR holds a branch that is about to be proposed for review: the
	// type schema's required domain fields and the state/commit linkage
	// become blocking.
	GatePR Gate = "pr"
	// GateMain holds a branch that is about to merge into frozen main
	// (docs/09 §3): provenance completeness becomes blocking — payload
	// integrity, actor consistency, an unbroken state chain.
	GateMain Gate = "main"
	// GateRelease holds an accepted state that is about to become an
	// immutable release snapshot (docs/11 §1): the review/approval record,
	// the rights snapshot and the main-branch origin become blocking.
	GateRelease Gate = "release"
	// GateAsset holds a research asset version about to be published
	// (docs/11 §2/§4): the full asset checklist — pinned source and
	// version, contributors, rights/license, visibility, dependency pins,
	// hash, metadata, schema — becomes blocking.
	GateAsset Gate = "asset"
)

// gateOrder is the strictness ladder, loosest first. It is the single
// authority for "which gate is stricter": GateSpecs must be monotone
// along it (a spec-level unit test enforces that — the ladder can never
// silently regress).
var gateOrder = [...]Gate{GateDraft, GatePR, GateMain, GateRelease, GateAsset}

// ValidGate reports whether g is one of the five canonical gates.
func ValidGate(g Gate) bool {
	switch g {
	case GateDraft, GatePR, GateMain, GateRelease, GateAsset:
		return true
	}
	return false
}

// ParseGate maps a wire value to a gate; unknown values fail explicitly —
// a gate name is never guessed (docs/45: unknown inputs are rejected, not
// defaulted).
func ParseGate(s string) (Gate, error) {
	g := Gate(s)
	if !ValidGate(g) {
		return "", fmt.Errorf("validation: unknown gate %q (canonical gates: draft, pr, main, release, asset)", s)
	}
	return g, nil
}

// CommitGates returns the gates a state commit command can be held to:
// draft for everyday branch commits, pr for review-targeted transitions,
// main for the merge that lands on frozen main. release and asset are not
// commits — they snapshot an accepted state (docs/11) — so a commit named
// with them is a caller error, not a stricter commit.
func CommitGates() []Gate { return []Gate{GateDraft, GatePR, GateMain} }

// ValidCommitGate reports whether g can guard a state commit.
func ValidCommitGate(g Gate) bool {
	for _, cg := range CommitGates() {
		if g == cg {
			return true
		}
	}
	return false
}

// Stage returns the 1-based position of g on the strictness ladder and
// whether g is canonical. GateDraft is stage 1, GateAsset stage 5.
func Stage(g Gate) (int, bool) {
	for i, cg := range gateOrder {
		if g == cg {
			return i + 1, true
		}
	}
	return 0, false
}

// StricterThan reports whether a is later on the ladder than b.
func StricterThan(a, b Gate) bool {
	sa, okA := Stage(a)
	sb, okB := Stage(b)
	return okA && okB && sa > sb
}
