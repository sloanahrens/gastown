package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier, except that a symlink to this binary named
// bd-standin acts as bd: the Executor passes bd's argv and no test flag, so the
// name selects it; syscall.Exit keeps a coverage build's exit hook off stderr.
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) == "bd-standin" {
		args := append(os.Args[1:], "", "") // pads: a missing argument refuses, not panics
		if status, err := strconv.Atoi(args[1]); err == nil {
			line := args[0]
			if len(os.Args) > 3 {
				line = os.Args[3] // an optional third argument is the stderr line
			}
			fmt.Fprintln(os.Stderr, line)
			syscall.Exit(status)
		}
		syscall.Exit(exitUsage)
	}
	os.Exit(unittier.Main(m))
}
