package imagevalues_test

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain holds the unit tier's run checks, as TestUnitTierMain requires of
// every package whose tests do not run under the hermetic harness: this
// package's tests write their values files under t.TempDir() and read nothing
// else.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
