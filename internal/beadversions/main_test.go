package beadversions

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// The package reads no environment, but its test binary's import closure
// reaches the Dolt server manager through internal/metrics, so
// TestHermeticHarnessEnforced requires the hermetic harness here rather than
// unittier.Main (internal/testutil/hermetic_enforce_test.go).
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
