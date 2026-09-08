//go:build !integration

package cmd

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's non-integration tests under the hermetic
// harness (gt-lwi): GT_*/BD_* env scrubbed, HOME and town root redirected to
// a sandbox, Dolt ports poisoned so nothing reaches the production server,
// and a tripwire that fails the run if any state leaks into a live town.
// (The integration build has its own TestMain in integration_testmain_test.go.)
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
