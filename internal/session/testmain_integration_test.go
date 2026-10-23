//go:build integration

package session

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier under the hermetic harness with real git
// on PATH: the clone script's replay runs git for real. The unit tier's
// TestMain is in main_test.go.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
