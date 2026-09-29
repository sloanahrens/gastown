//go:build integration

package notifyfake

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain puts the package's tests under the hermetic harness, as every
// package that links the beads client must (TestHermeticHarnessEnforced).
// The unit tests here only touch the in-memory Recorder; the contract's real
// run lives in internal/notify's integration tier.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
