//go:build integration

package config_test

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier under the hermetic harness with real
// tools on PATH; the unit tier's TestMain (hermetic_main_test.go) refuses git.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}
