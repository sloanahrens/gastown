package crewstale

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier: the walk is tested against in-memory fakes, so
// no test here reaches git, a subprocess or the live filesystem.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
