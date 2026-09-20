package e2e

import (
	"testing"

	"github.com/lichman0405/post/internal/persistence/testdb"
)

// TestMain hands the whole run to testdb.Main, which drops this run's migrated
// template database when the package finishes. conflict_e2e_test.go is the one
// file here that calls testdb.Setup, and its template outlives the individual
// tests, so no t.Cleanup can reach it — without this hook the package builds a
// template on every run that nothing ever drops. T0815's first draft did
// exactly that and leaked two; testdb.Setup now fails the test outright when a
// package forgets, and this is the hook it is asking for.
func TestMain(m *testing.M) { testdb.Main(m) }
