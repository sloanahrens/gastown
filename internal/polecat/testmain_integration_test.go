//go:build integration

package polecat

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier under the hermetic harness with real
// git on PATH. Tests that need a Dolt server start the shared container
// lazily via RequireDoltContainer; Finish terminates it.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
