package authz_test

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"testing"

	"github.com/lichman0405/post/internal/authz"
)

// TestPermissionMatrixMatchesCSV is the T0105 acceptance gate
// "权限矩阵核心动作与 CSV 一致": the engine's table must be cell-for-cell
// identical with the canonical policy CSV
// (specs/policies/permissions-matrix.csv). Any drift — an edited row, a
// new action, a renamed column, a cell value outside the verdict
// vocabulary — fails this test, so the matrix cannot silently diverge
// from the policy document it implements.
func TestPermissionMatrixMatchesCSV(t *testing.T) {
	csvPath := filepath.Join("..", "..", "specs", "policies", "permissions-matrix.csv")
	f, err := os.Open(csvPath)
	if err != nil {
		t.Fatalf("open canonical permission matrix CSV: %v", err)
	}
	defer f.Close()

	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("parse permission matrix CSV: %v", err)
	}
	if len(records) < 2 {
		t.Fatalf("permission matrix CSV has %d records, want header + rows", len(records))
	}

	// --- header: the CSV's columns must be exactly the engine's classes ---
	header := records[0]
	if len(header) < 2 || header[0] != "action" {
		t.Fatalf("CSV header = %q, want action,<actor classes...>", header)
	}
	csvClasses := map[authz.ActorClass]bool{}
	for _, col := range header[1:] {
		if col == "" {
			t.Fatalf("CSV header contains an empty column name: %q", header)
		}
		csvClasses[authz.ActorClass(col)] = true
	}
	engineClasses := map[authz.ActorClass]bool{}
	for _, c := range authz.ActorClasses() {
		engineClasses[c] = true
	}
	for c := range csvClasses {
		if !engineClasses[c] {
			t.Errorf("CSV column %q is missing from the engine's actor classes", c)
		}
	}
	for c := range engineClasses {
		if !csvClasses[c] {
			t.Errorf("engine actor class %q is missing from the CSV header", c)
		}
	}

	// --- rows: every CSV cell must match the engine's matrix exactly ---
	matrix := authz.Matrix()
	csvActions := map[authz.Action]bool{}
	for _, record := range records[1:] {
		if len(record) == 0 {
			continue // trailing blank line
		}
		if allEmpty(record) {
			continue
		}
		if len(record) != len(header) {
			t.Fatalf("CSV row %q has %d cells, want %d (header width)", record, len(record), len(header))
		}
		action := authz.Action(record[0])
		csvActions[action] = true
		row, ok := matrix[action]
		if !ok {
			t.Errorf("CSV action %q is missing from the engine matrix", action)
			continue
		}
		for i, col := range header[1:] {
			cell := record[i+1]
			verdict, ok := parseVerdict(cell)
			if !ok {
				t.Errorf("CSV cell %s/%s = %q is outside the verdict vocabulary", action, col, cell)
				continue
			}
			if got := row[authz.ActorClass(col)]; got != verdict {
				t.Errorf("matrix[%s][%s] = %q, CSV says %q", action, col, got, cell)
			}
		}
	}
	for a := range matrix {
		if !csvActions[a] {
			t.Errorf("engine action %q is missing from the CSV", a)
		}
	}
}

// allEmpty reports whether a CSV record is entirely blank cells (a
// trailing newline produces such a record and is not a matrix row).
func allEmpty(record []string) bool {
	for _, cell := range record {
		if cell != "" {
			return false
		}
	}
	return true
}

// parseVerdict maps a CSV cell onto the Verdict vocabulary. The mapping
// is explicit: an unrecognized cell value is a policy typo and must fail
// the gate rather than silently parse as "deny".
func parseVerdict(s string) (authz.Verdict, bool) {
	switch s {
	case "allow":
		return authz.VerdictAllow, true
	case "deny":
		return authz.VerdictDeny, true
	case "scoped":
		return authz.VerdictScoped, true
	case "conditional":
		return authz.VerdictConditional, true
	case "external_fork_only":
		return authz.VerdictExternalForkOnly, true
	case "own_fork_only":
		return authz.VerdictOwnForkOnly, true
	case "allow_from_fork":
		return authz.VerdictAllowFromFork, true
	case "proposal_or_scoped":
		return authz.VerdictProposalOrScoped, true
	case "proposal_only":
		return authz.VerdictProposalOnly, true
	case "via_pr":
		return authz.VerdictViaPR, true
	case "public_policy":
		return authz.VerdictPublicPolicy, true
	case "authorized":
		return authz.VerdictAuthorized, true
	case "allow_if_authorized":
		return authz.VerdictAllowIfAuthorized, true
	}
	return "", false
}

// TestRoleColumnsMonotonic locks in the docs/04 §2 role hierarchy against
// the matrix itself: viewer < contributor < maintainer < owner, so an
// action allowed for a lower role must be allowed for every higher role
// too. If a policy edit breaks that ordering, this test says so — the
// CSV and the domain's role ranking must stay consistent with each
// other.
func TestRoleColumnsMonotonic(t *testing.T) {
	chain := []authz.ActorClass{
		authz.ActorViewer,
		authz.ActorContributor,
		authz.ActorMaintainer,
		authz.ActorOwner,
	}
	matrix := authz.Matrix()
	for _, action := range authz.Actions() {
		for i := 0; i < len(chain)-1; i++ {
			lower, higher := chain[i], chain[i+1]
			lowV, highV := matrix[action][lower], matrix[action][higher]
			if lowV == authz.VerdictAllow && highV != authz.VerdictAllow {
				t.Errorf("role hierarchy violated: %s allowed for %s but %s for %s",
					action, lower, highV, higher)
			}
			if highV == authz.VerdictDeny && lowV != authz.VerdictDeny {
				t.Errorf("role hierarchy violated: %s denied for %s but %s for %s",
					action, higher, lowV, lower)
			}
		}
	}
}
