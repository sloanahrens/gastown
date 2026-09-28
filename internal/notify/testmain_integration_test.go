//go:build integration

package notify_test

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier under the hermetic harness: the contract
// test runs a real gt, which must never see the caller's town identity or
// reach a live Dolt server.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
