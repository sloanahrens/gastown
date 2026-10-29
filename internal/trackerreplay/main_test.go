//go:build !integration

package trackerreplay

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the package's tests under the hermetic harness, so a goroutine
// that outlives a test fails the run rather than leaking into the next one.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
