package integration

import (
	"testing"

	"github.com/lichman0405/post/internal/persistence/testdb"
)

// TestMain hands the whole run to testdb.Main: it drops this run's migrated
// template databases (they outlive the individual tests, so no t.Cleanup can
// reach them) and reports where the run's database provisioning time went.
// See the doc on testdb.Main for what that number is for.
func TestMain(m *testing.M) { testdb.Main(m) }
