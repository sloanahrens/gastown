package main

import (
	"fmt"
	"io"
	"path/filepath"
	"time"
)

// treeBinName is the swept tree's sweep command, as build names it.
const treeBinName = "tier-sweep"

// treeBuildTimeout bounds the build of the swept tree's sweep command: a warm
// build links in seconds, and the sweep has every package still ahead of it
// (gt-tuhcg9).
const treeBuildTimeout = 10 * time.Minute

// build builds work's cmd/tier-sweep into a temporary directory and returns the
// binary and remove, which deletes that directory (gt-tuhcg9). A failed tempDir
// or a build that does not exit 0 is ok false: the build's exit status is one
// stderr line naming the step, and the directory is removed before returning.
// remove is never nil, so the caller defers it on every path; after a failed
// build it is a no-op, because the directory is already gone.
func (t treeSweep) build(work string, env []string, stderr io.Writer) (bin string, remove func(), ok bool) {
	dir, remove, ok := t.tempDir(stderr)
	if !ok {
		return "", remove, false
	}
	bin = filepath.Join(dir, treeBinName)
	argv := []string{"go", "build", "-o", bin, "./" + goSweepDir}
	if code := t.Build(work, env, argv, stderr, treeBuildTimeout); code != 0 {
		fmt.Fprintf(stderr, "%s: build the tree sweep: go build ./%s exited %d\n", name, goSweepDir, code)
		remove()
		return "", func() {}, false
	}
	return bin, remove, true
}
