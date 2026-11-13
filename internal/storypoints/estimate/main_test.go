package estimate

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the package's tests under the hermetic harness (gt-lwi): the
// package's Jev dependency pulls in the beads SDK, so the town and Dolt
// environment is sandboxed and a test that reaches live state fails instead of
// reading it. The harness also runs the unit tier's checks.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
