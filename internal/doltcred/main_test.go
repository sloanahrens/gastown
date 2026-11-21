package doltcred

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier's run-time checks: no goroutine a test started
// may outlive the tests, and no test may start an external tool
// (docs/testing.md, "The rules").
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
