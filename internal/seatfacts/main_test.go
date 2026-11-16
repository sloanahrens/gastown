package seatfacts

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs this package's unit tests under the checks every package's
// TestMain runs (internal/testpolicy's TestUnitTierMain): no goroutine a test
// starts may outlive it, and no test may start an external tool. The package
// reads files it is given and starts nothing, so it does not need the
// hermetic harness.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
