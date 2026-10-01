package steward

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the unit tier under the hermetic harness: the package's
// production imports reach internal/beads, and a bare unittier.Main is not
// the harness that guard accepts (gt-lwi).
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
