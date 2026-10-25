package seed

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier through unittier.Main, so a test that starts a
// refused tool or leaks a goroutine fails the package's run.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
