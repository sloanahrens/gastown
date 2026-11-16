//go:build !integration

package doltsquash

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier under the unit-tier checks: the package's tests
// answer the Dolt seam with a fake and start no process. The integration tier's
// TestMain is in testmain_integration_test.go.
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
