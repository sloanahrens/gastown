package seattoken

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain holds the unit tier's run checks, as TestUnitTierMain requires of
// every package whose tests do not run under the hermetic harness: the package
// touches only the temp towns a test hands it.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
