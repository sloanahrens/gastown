package main

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier through unittier.Main, which fails the run on a
// goroutine that outlives its tests (docs/testing.md, "The rules").
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}
