package signing

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain holds the unit tier's run checks, as TestUnitTierMain requires of
// every package whose tests do not run under the hermetic harness: the tests
// drive the signing steps through fakes and touch no real command or keychain.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
