package authz

// Verdict is one cell of the permission matrix: the answer for an
// (action, actor class) pair. The string values are exactly the CSV's
// cell vocabulary (specs/policies/permissions-matrix.csv); the
// CSV-consistency test rejects a cell value that is not one of these.
type Verdict string

const (
	// VerdictAllow permits the action unconditionally.
	VerdictAllow Verdict = "allow"
	// VerdictDeny refuses the action.
	VerdictDeny Verdict = "deny"
	// VerdictScoped permits a scoped form of the action (e.g. agents
	// read private state only inside their granted scope).
	VerdictScoped Verdict = "scoped"
	// VerdictConditional permits the action only when a per-resource
	// condition holds; the enforcement site must resolve the condition.
	VerdictConditional       Verdict = "conditional"
	VerdictExternalForkOnly  Verdict = "external_fork_only"
	VerdictOwnForkOnly       Verdict = "own_fork_only"
	VerdictAllowFromFork     Verdict = "allow_from_fork"
	VerdictProposalOrScoped  Verdict = "proposal_or_scoped"
	VerdictProposalOnly      Verdict = "proposal_only"
	VerdictViaPR             Verdict = "via_pr"
	VerdictPublicPolicy      Verdict = "public_policy"
	VerdictAuthorized        Verdict = "authorized"
	VerdictAllowIfAuthorized Verdict = "allow_if_authorized"
)

// Permits reports whether the verdict allows the action unconditionally.
// Only VerdictAllow permits: every conditional form still requires the
// enforcement site to resolve its condition, and the safe default for an
// unresolved condition is refusal (default deny).
func (v Verdict) Permits() bool { return v == VerdictAllow }

// Denies reports whether the verdict refuses the action outright.
func (v Verdict) Denies() bool { return v == VerdictDeny }

// Decision is the engine's answer to one authorization request.
type Decision struct {
	// Verdict is the matrix cell for the request. An unknown action or
	// actor class answers VerdictDeny.
	Verdict Verdict
	// Reason explains the decision ("not in the permission matrix",
	// "unknown actor class", "no policy engine configured", …). It is
	// for logs and operators, not for the wire: a client only ever sees
	// the mapped HTTP code.
	Reason string
}

// Permits reports whether the decision allows the action unconditionally.
func (d Decision) Permits() bool { return d.Verdict.Permits() }

// Denies reports whether the decision refuses the action outright.
func (d Decision) Denies() bool { return d.Verdict.Denies() }
