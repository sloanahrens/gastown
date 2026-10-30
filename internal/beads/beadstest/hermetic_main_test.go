package beadstest

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi): the
// wrappers reach no bd, and the poisoned Dolt ports would refuse it.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
