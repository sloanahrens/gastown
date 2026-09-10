package health

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi).
// health_test.go imports internal/doltserver for its DoltListener fixtures,
// which the enforcer treats as a risky dependency even though these tests
// only exercise pure classification logic and never dial a real server.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
