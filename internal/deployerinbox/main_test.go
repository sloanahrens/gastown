package deployerinbox_test

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain holds the unit tier's run checks: these tests use an in-memory
// filesystem and start no tool.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
