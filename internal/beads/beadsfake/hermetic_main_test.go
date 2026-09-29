package beadsfake_test

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the fake's tests under the hermetic harness (gt-lwi), like
// every package whose tests could reach bd: nothing here should, and the
// tripwire proves it.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
