package deployercred_test

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain holds the unit tier's run checks, as TestUnitTierMain requires of
// every package whose tests do not run under the hermetic harness: these tests
// take their key bytes from a reader they pass in and write no file.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
