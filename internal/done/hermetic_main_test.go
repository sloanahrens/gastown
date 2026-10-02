//go:build !integration

package done

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs done's unit tier under the hermetic harness (gt-lwi): GT_*/
// BD_* env scrubbed, HOME and town root redirected to a sandbox, and Dolt
// ports poisoned so nothing reaches the production server. The integration
// tier's TestMain is in testmain_integration_test.go.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
