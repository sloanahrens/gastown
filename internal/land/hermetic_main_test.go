package land

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs land's tests under the hermetic harness (gt-lwi), like every
// package whose tests could reach bd: they use beadsfake and git only, and the
// tripwire proves it.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
