//go:build integration

package doltsquash

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier under the hermetic harness: the lab starts
// the host's dolt binary, so the run happens with GT_*/BD_* scrubbed, HOME
// redirected into a sandbox, and the Dolt ports poisoned — the lab's server is
// reached only by the scratch port the test picked. The unit tier's TestMain
// (main_test.go) runs under the unit-tier checks.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
