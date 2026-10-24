package metrics

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness. The host
// source imports doltserver for its port, which puts the beads and Dolt
// packages in this test closure, so TestHermeticHarnessEnforced requires the
// harness here. HermeticMain also runs the unit tier's checks.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
