package ci

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi): the
// images workflow test links internal/cmd, whose dependency closure reaches the
// beads SDK and the Dolt server manager, so the package adopts the harness
// rather than the unit tier.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
