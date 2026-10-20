package scorecard

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the scorecard under the hermetic harness: the attention,
// finding and landing adapters read beads, so this package's tests can reach bd
// and must not touch live town state.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
