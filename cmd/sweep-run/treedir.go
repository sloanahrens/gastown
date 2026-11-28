package main

import (
	"fmt"
	"io"

	"github.com/steveyegge/gastown/internal/tiersweep"
)

// treeSweep holds the filesystem seams the swept tree's sweep command is built
// into. A test fills them with fakes, so no directory is made or removed.
type treeSweep struct {
	// MkdirTemp creates the directory the command is built into, as os.MkdirTemp.
	MkdirTemp func(dir, pattern string) (string, error)
	// RemoveAll deletes that directory, as os.RemoveAll.
	RemoveAll func(path string) error
	// Build builds the swept tree's sweep command in that directory, as
	// tiersweep.ProcessBuildExec runs one go build under a wall-clock bound.
	Build tiersweep.BuildExec
	// Run starts the built sweep command, as Deps.RunScript runs a program. It
	// returns the exit status, or a negative value (sweepNoExit) when it produced
	// none.
	Run func(program string, args []string, dir string, env []string, stdout, stderr io.Writer) int
	// Environ is the process environment the build and the run start from, with
	// the sweep's overrides added on top (environWith).
	Environ []string
}

// tempDir makes the directory the swept tree's sweep command is built into. It
// returns the directory and remove, which deletes it. A failed MkdirTemp is one
// stderr line that names the step and the error, and returns ok false with a
// remove that is a no-op, so the caller can defer remove either way. A removal
// that fails is one stderr line that names the directory.
func (t treeSweep) tempDir(stderr io.Writer) (dir string, remove func(), ok bool) {
	made, err := t.MkdirTemp("", "sweep-bin.")
	if err != nil {
		fmt.Fprintf(stderr, "%s: create the tree sweep's temporary directory: %v\n", name, err)
		return "", func() {}, false
	}
	return made, func() {
		if err := t.RemoveAll(made); err != nil {
			fmt.Fprintf(stderr, "%s: remove the tree sweep's temporary directory %s: %v\n", name, made, err)
		}
	}, true
}
