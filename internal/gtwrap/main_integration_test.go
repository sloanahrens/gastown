//go:build integration

package gtwrap

import (
	"os"
	"testing"
)

// The integration tier runs real files and real processes, which the unit
// tier's hermetic harness refuses.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
