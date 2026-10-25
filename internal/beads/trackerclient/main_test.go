//go:build !integration

package trackerclient_test

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the package's tests under the hermetic harness, like every
// package whose tests could reach bd: nothing here may, and the poisoned Dolt
// ports would refuse it.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
