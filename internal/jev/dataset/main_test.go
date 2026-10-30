package dataset

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the export under the hermetic harness: it reads a battery
// through the scorecard, whose adapters read beads, so this package's tests
// must not touch live town state.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
