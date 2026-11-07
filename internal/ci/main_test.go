package ci

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain holds the unit tier's run checks, as TestUnitTierMain requires of
// every package whose tests do not run under the hermetic harness: this package
// reads only the workflow text the repository carries and writes nothing.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
