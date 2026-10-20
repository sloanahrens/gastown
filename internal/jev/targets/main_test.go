package targets

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the package's tests under the hermetic harness (gt-lwi): the
// town and Dolt environment is sandboxed, so a test that reaches the live town
// fails rather than reading it.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
