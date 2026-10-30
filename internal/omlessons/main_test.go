package omlessons

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness, which also
// fails the run on a goroutine that outlives its test.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
