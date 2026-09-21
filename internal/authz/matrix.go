package authz

import "slices"

// matrixTable is the canonical V1 permission matrix, mirroring
// specs/policies/permissions-matrix.csv cell for cell. The CSV is the
// policy source of truth; TestPermissionMatrixMatchesCSV keeps this table
// identical to it (any drift fails the gate), and
// TestRoleColumnsMonotonic additionally locks in the docs/04 §2 role
// hierarchy: an action allowed for a lower role is allowed for every
// higher role.
var matrixTable = map[Action]map[ActorClass]Verdict{
	ActionReadPublicProject: {
		ActorPublicAnonymous:        VerdictAllow,
		ActorAuthenticatedNonMember: VerdictAllow,
		ActorViewer:                 VerdictAllow,
		ActorContributor:            VerdictAllow,
		ActorMaintainer:             VerdictAllow,
		ActorOwner:                  VerdictAllow,
		ActorAgent:                  VerdictAllow,
	},
	ActionReadPrivateProject: {
		ActorPublicAnonymous:        VerdictDeny,
		ActorAuthenticatedNonMember: VerdictDeny,
		ActorViewer:                 VerdictAllow,
		ActorContributor:            VerdictAllow,
		ActorMaintainer:             VerdictAllow,
		ActorOwner:                  VerdictAllow,
		ActorAgent:                  VerdictScoped,
	},
	ActionCreateProject: {
		ActorPublicAnonymous:        VerdictDeny,
		ActorAuthenticatedNonMember: VerdictAllow,
		ActorViewer:                 VerdictAllow,
		ActorContributor:            VerdictAllow,
		ActorMaintainer:             VerdictAllow,
		ActorOwner:                  VerdictAllow,
		ActorAgent:                  VerdictScoped,
	},
	ActionCreateBranch: {
		ActorPublicAnonymous:        VerdictDeny,
		ActorAuthenticatedNonMember: VerdictExternalForkOnly,
		ActorViewer:                 VerdictDeny,
		ActorContributor:            VerdictAllow,
		ActorMaintainer:             VerdictAllow,
		ActorOwner:                  VerdictAllow,
		ActorAgent:                  VerdictAllow,
	},
	ActionWriteScientificState: {
		ActorPublicAnonymous:        VerdictDeny,
		ActorAuthenticatedNonMember: VerdictOwnForkOnly,
		ActorViewer:                 VerdictDeny,
		ActorContributor:            VerdictAllow,
		ActorMaintainer:             VerdictAllow,
		ActorOwner:                  VerdictAllow,
		ActorAgent:                  VerdictAllow,
	},
	ActionOpenPR: {
		ActorPublicAnonymous:        VerdictDeny,
		ActorAuthenticatedNonMember: VerdictAllowFromFork,
		ActorViewer:                 VerdictDeny,
		ActorContributor:            VerdictAllow,
		ActorMaintainer:             VerdictAllow,
		ActorOwner:                  VerdictAllow,
		ActorAgent:                  VerdictAllow,
	},
	ActionSubmitScientificReview: {
		ActorPublicAnonymous:        VerdictDeny,
		ActorAuthenticatedNonMember: VerdictConditional,
		ActorViewer:                 VerdictConditional,
		ActorContributor:            VerdictConditional,
		ActorMaintainer:             VerdictAllow,
		ActorOwner:                  VerdictAllow,
		ActorAgent:                  VerdictProposalOrScoped,
	},
	ActionMergeMain: {
		ActorPublicAnonymous:        VerdictDeny,
		ActorAuthenticatedNonMember: VerdictDeny,
		ActorViewer:                 VerdictDeny,
		ActorContributor:            VerdictDeny,
		ActorMaintainer:             VerdictAllow,
		ActorOwner:                  VerdictAllow,
		ActorAgent:                  VerdictDeny,
	},
	ActionFreezeMain: {
		ActorPublicAnonymous:        VerdictDeny,
		ActorAuthenticatedNonMember: VerdictDeny,
		ActorViewer:                 VerdictDeny,
		ActorContributor:            VerdictDeny,
		ActorMaintainer:             VerdictAllow,
		ActorOwner:                  VerdictAllow,
		ActorAgent:                  VerdictDeny,
	},
	ActionCreateRelease: {
		ActorPublicAnonymous:        VerdictDeny,
		ActorAuthenticatedNonMember: VerdictDeny,
		ActorViewer:                 VerdictDeny,
		ActorContributor:            VerdictDeny,
		ActorMaintainer:             VerdictAllow,
		ActorOwner:                  VerdictAllow,
		ActorAgent:                  VerdictProposalOnly,
	},
	ActionPublishPrivateToPublic: {
		ActorPublicAnonymous:        VerdictDeny,
		ActorAuthenticatedNonMember: VerdictDeny,
		ActorViewer:                 VerdictDeny,
		ActorContributor:            VerdictDeny,
		ActorMaintainer:             VerdictConditional,
		ActorOwner:                  VerdictAllow,
		ActorAgent:                  VerdictDeny,
	},
	ActionChangeRightsHolder: {
		ActorPublicAnonymous:        VerdictDeny,
		ActorAuthenticatedNonMember: VerdictDeny,
		ActorViewer:                 VerdictDeny,
		ActorContributor:            VerdictDeny,
		ActorMaintainer:             VerdictDeny,
		ActorOwner:                  VerdictAllow,
		ActorAgent:                  VerdictDeny,
	},
	ActionAbortMainObject: {
		ActorPublicAnonymous:        VerdictDeny,
		ActorAuthenticatedNonMember: VerdictDeny,
		ActorViewer:                 VerdictDeny,
		ActorContributor:            VerdictDeny,
		ActorMaintainer:             VerdictViaPR,
		ActorOwner:                  VerdictViaPR,
		ActorAgent:                  VerdictProposalOnly,
	},
	// ActionReopenMainObject is cell-for-cell the abort row above, and that
	// identity is the policy: abort and reopen are one pair of opposite
	// operations, so undoing an abort is never easier than making one
	// (T0610's ruling; specs/policies/permissions-matrix.csv:15).
	ActionReopenMainObject: {
		ActorPublicAnonymous:        VerdictDeny,
		ActorAuthenticatedNonMember: VerdictDeny,
		ActorViewer:                 VerdictDeny,
		ActorContributor:            VerdictDeny,
		ActorMaintainer:             VerdictViaPR,
		ActorOwner:                  VerdictViaPR,
		ActorAgent:                  VerdictProposalOnly,
	},
	ActionReadFiles: {
		ActorPublicAnonymous:        VerdictPublicPolicy,
		ActorAuthenticatedNonMember: VerdictAuthorized,
		ActorViewer:                 VerdictAuthorized,
		ActorContributor:            VerdictAuthorized,
		ActorMaintainer:             VerdictAuthorized,
		ActorOwner:                  VerdictAuthorized,
		ActorAgent:                  VerdictAllowIfAuthorized,
	},
	ActionMutateFilesWeb: {
		ActorPublicAnonymous:        VerdictDeny,
		ActorAuthenticatedNonMember: VerdictDeny,
		ActorViewer:                 VerdictDeny,
		ActorContributor:            VerdictDeny,
		ActorMaintainer:             VerdictDeny,
		ActorOwner:                  VerdictDeny,
		ActorAgent:                  VerdictDeny,
	},
}

// Matrix returns a copy of the canonical permission matrix. It exists so
// tests and tooling can read the table without being able to mutate the
// engine's data.
func Matrix() map[Action]map[ActorClass]Verdict {
	out := make(map[Action]map[ActorClass]Verdict, len(matrixTable))
	for action, row := range matrixTable {
		rowCopy := make(map[ActorClass]Verdict, len(row))
		for class, verdict := range row {
			rowCopy[class] = verdict
		}
		out[action] = rowCopy
	}
	return out
}

// Actions returns the matrix's actions (the CSV's rows), sorted.
func Actions() []Action {
	out := make([]Action, 0, len(matrixTable))
	for a := range matrixTable {
		out = append(out, a)
	}
	slices.Sort(out)
	return out
}

// ActorClasses returns the matrix's actor classes (the CSV's columns),
// sorted.
func ActorClasses() []ActorClass {
	out := make([]ActorClass, 0, len(actorClasses))
	for c := range actorClasses {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}

// actorClasses is the fixed set of matrix columns (the CSV header).
var actorClasses = map[ActorClass]bool{
	ActorPublicAnonymous:        true,
	ActorAuthenticatedNonMember: true,
	ActorViewer:                 true,
	ActorContributor:            true,
	ActorMaintainer:             true,
	ActorOwner:                  true,
	ActorAgent:                  true,
}
