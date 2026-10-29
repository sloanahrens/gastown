package polecat

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// make lint runs shellcheck, and a sandbox polecat has no host tools, so the
// image must install shellcheck in its final stage. The make-build stage is
// discarded and ships only its make binary, so an install there does not count.
func TestPolecatDockerfileInstallsShellcheckInTheFinalStage(t *testing.T) {
	t.Parallel()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "deploy", "polecat", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	// Continuation lines joined, so each instruction is one line.
	lines := strings.Split(strings.ReplaceAll(string(data), "\\\n", " "), "\n")
	final := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "FROM ") {
			final = i
		}
	}
	if final < 0 {
		t.Fatal("deploy/polecat/Dockerfile has no FROM instruction")
	}
	for _, line := range lines[final:] {
		cmd, isRun := strings.CutPrefix(line, "RUN ")
		if !isRun {
			continue
		}
		for _, part := range strings.Split(cmd, "&&") {
			if strings.Contains(part, "apt-get install") && slices.Contains(strings.Fields(part), "shellcheck") {
				return
			}
		}
	}
	t.Error("the final stage of deploy/polecat/Dockerfile installs no shellcheck (add it to the apt-get install list that carries build-essential)")
}
