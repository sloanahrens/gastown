package rehearsal

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the package under the hermetic harness: seats.go imports
// internal/polecat, so the test binary depends on packages that can reach the
// town's beads and Dolt.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
