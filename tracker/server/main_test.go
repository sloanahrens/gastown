//go:build !integration

package server

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/tracker/internal/leakcheck"
)

// TestMain runs the unit tier under the leak check. The integration tier's
// main_integration_test.go replaces it: one test binary takes one TestMain.
func TestMain(m *testing.M) { os.Exit(leakcheck.Main(m)) }
