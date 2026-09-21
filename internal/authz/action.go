package authz

// Action identifies one permission-matrix action: a row of
// specs/policies/permissions-matrix.csv. Every public product action must
// map onto one of these before it runs (docs/50); anything outside the
// matrix is denied by default.
type Action string

const (
	// ActionReadPublicProject reads the shell of a public project.
	ActionReadPublicProject Action = "read_public_project"
	// ActionReadPrivateProject reads the shell of a private project
	// (member-only in V1; T0106 wires the visibility-aware choice).
	ActionReadPrivateProject Action = "read_private_project"
	// ActionCreateProject creates a new project.
	ActionCreateProject Action = "create_project"
	// ActionCreateBranch creates a research branch (T0201).
	ActionCreateBranch Action = "create_branch"
	// ActionWriteScientificState writes RSG state in a branch (T0202+).
	ActionWriteScientificState Action = "write_scientific_state"
	// ActionOpenPR opens a pull request with a proposed RSG diff (T0205).
	ActionOpenPR Action = "open_pr"
	// ActionSubmitScientificReview submits a scientific/integrity review
	// (T0206).
	ActionSubmitScientificReview Action = "submit_scientific_review"
	// ActionMergeMain merges an approved PR into frozen main (T0409).
	ActionMergeMain Action = "merge_main"
	// ActionFreezeMain freezes main (T0601).
	ActionFreezeMain Action = "freeze_main"
	// ActionCreateRelease creates an immutable release (T0606).
	ActionCreateRelease Action = "create_release"
	// ActionPublishPrivateToPublic widens visibility: private to public
	// (docs/12 §3 — never automatic, always audited).
	ActionPublishPrivateToPublic Action = "publish_private_to_public"
	// ActionChangeRightsHolder changes the rights holder.
	ActionChangeRightsHolder Action = "change_rights_holder"
	// ActionAbortMainObject aborts a main-branch object (T0602).
	ActionAbortMainObject Action = "abort_main_object"
	// ActionReopenMainObject reopens a main-branch object that is in
	// lifecycle 'reopened' — the reverse edge of the abort above (T0610,
	// docs/43's object lifecycle "active → aborted → reopened → active").
	// It is a row of the matrix in its own right, not a reuse of
	// write_scientific_state: the ruling recorded for T0610 rejects that
	// reuse because it would let a contributor undo a maintainer's abort
	// while being unable to abort at all — a loosening of the permission
	// model, not a simplification of it.
	ActionReopenMainObject Action = "reopen_main_object"
	// ActionReadFiles reads repository files (GitProvider-level).
	ActionReadFiles Action = "read_files"
	// ActionMutateFilesWeb mutates repository files through the web
	// surface (docs/10: the Files Web page is strictly read-only).
	ActionMutateFilesWeb Action = "mutate_files_web"
)

// ValidAction reports whether a is one of the matrix's actions.
func ValidAction(a Action) bool {
	_, ok := matrixTable[a]
	return ok
}
