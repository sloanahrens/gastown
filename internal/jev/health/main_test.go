package health_test

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the package's tests under the hermetic harness (gt-lwi): the
// health package reads every battery package's files, which can reach the
// live town, so a test that does is sandboxed rather than reading it.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
