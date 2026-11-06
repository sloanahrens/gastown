//go:build !integration

package makegate

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier's hermetic checks. The integration tier has its
// own TestMain in cpuclass_integration_test.go, so this file is excluded there.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
