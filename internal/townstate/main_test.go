package townstate

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the tests under the hermetic harness. The rig pin test imports
// internal/rig, whose import closure reaches the Dolt server manager, so
// TestHermeticHarnessEnforced requires the harness here rather than
// unittier.Main (internal/testutil/hermetic_enforce_test.go).
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
