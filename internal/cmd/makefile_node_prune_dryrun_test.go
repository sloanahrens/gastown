package cmd

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dryRunMakePath is the child's whole PATH. `make -n` runs no recipe line, but
// make's own $(shell) calls do, and a fixed PATH keeps the dry run the same
// whoever runs it.
const dryRunMakePath = "/usr/bin:/bin"

// TestDryRunPolecatImagePrunesTheNodesOnlyWhenAsked pins the opt-in's effect in
// the gate, where the shell cases in scripts/makefile-gate_test.sh do not run:
// a default polecat-image leaves the gt-sbx nodes alone, and POLE_NODE_PRUNE=1
// prunes them, after the docker build (gt-06ni7, gt-06ni7.2).
//
// The seam is make's dry run, not the recipe's text: the flag is read by a
// parse-time `ifeq`, so make itself is the reader that answers which lines run,
// and a guard written another way (ifneq, filter) still answers here. -n prints
// the lines and runs none of them. Each case passes its own environment
// instead of inheriting the harness's: an exported POLE_NODE_PRUNE decides the
// default case, and an exported MAKEFLAGS reaches the dry run too (gt-pkl4q
// found the shell cases reading the caller's shell).
func TestDryRunPolecatImagePrunesTheNodesOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		env       []string
		wantPrune bool
	}{
		{
			name: "a default build leaves the nodes alone",
			env:  []string{"PATH=" + dryRunMakePath},
		},
		{
			name:      "POLE_NODE_PRUNE=1 prunes after the docker build",
			env:       []string{"PATH=" + dryRunMakePath, "POLE_NODE_PRUNE=1"},
			wantPrune: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lines := strings.Split(dryRunPolecatImage(t, tc.env), "\n")
			build, prune := lineMentioning(lines, "docker build"), lineMentioning(lines, "polecat-node-prune")
			if build < 0 {
				t.Fatalf("make -n polecat-image printed no docker build, so this case pinned nothing — if the recipe moved, move this test with it:\n\t%s", strings.Join(lines, "\n\t"))
			}
			if !tc.wantPrune {
				if prune >= 0 {
					t.Errorf("a build that did not ask prunes the gt-sbx nodes, so every image build changes the cluster too (gt-06ni7):\n\t%s", lines[prune])
				}
				return
			}
			if prune < 0 {
				t.Fatalf("POLE_NODE_PRUNE=1 runs no polecat-node-prune, so the opt-in prunes nothing (gt-06ni7):\n\t%s", strings.Join(lines, "\n\t"))
			}
			if prune < build {
				t.Errorf("polecat-node-prune runs at line %d, before the docker build at line %d, so it prunes the nodes for an image that is not built yet:\n\t%s", prune+1, build+1, lines[prune])
			}
		})
	}
}

// lineMentioning returns the index of the first line that contains want, or -1.
func lineMentioning(lines []string, want string) int {
	for i, line := range lines {
		if strings.Contains(line, want) {
			return i
		}
	}
	return -1
}

// dryRunPolecatImage returns what `make -n polecat-image` prints in the
// repository root, with env as the child's whole environment. make is the
// gate's own tool — every gate here is a make target — so finding it on PATH is
// a requirement the gate already has, not host state this test borrows.
func dryRunPolecatImage(t *testing.T, env []string) string {
	t.Helper()
	//testpolicy:allow no-subprocess — which recipe lines run is make's own answer: the flag is read by a parse-time ifeq, so the dry run is how make's reading is read, and -n runs none of the recipe
	cmd := exec.Command("make", "--no-print-directory", "-n", "polecat-image")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n polecat-image (working directory %s): %v\n%s", mustGetwd(t), err, out)
	}
	return string(out)
}
