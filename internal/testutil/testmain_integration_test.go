//go:build integration

package testutil

import (
	"os"
	"testing"
)

// TestMain runs the integration tier under the harness with real git on
// PATH; the unit tier's TestMain (hermetic_main_test.go) refuses git.
func TestMain(m *testing.M) {
	os.Exit(HermeticMain(m))
}
