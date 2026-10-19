package revertwaiver

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the package under the hermetic harness: the waiver reads beads
// issues, so a test must never resolve the live town (gt-lwi).
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
