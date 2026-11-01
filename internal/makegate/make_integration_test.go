//go:build integration

package makegate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// makeTestTimeout bounds one dry run. make -n prints recipes and runs nothing,
// so a call that takes this long is hung rather than slow.
const makeTestTimeout = 60 * time.Second

// Make runs the checkout's make with args and returns its combined output.
type Make func(args ...string) (string, error)

// RealMake returns the Make that runs the checkout at root as a dry run,
// `make -C root --no-print-directory -n args...`, whose output is the recipe
// without running it. The dry run is the shape the shell test read; a recipe
// line holding $(MAKE) still runs under -n, which the gate-shape cases pin.
func RealMake(root string) Make {
	return func(args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), makeTestTimeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, "make", append([]string{"-C", root, "--no-print-directory", "-n"}, args...)...).CombinedOutput()
		return string(out), err
	}
}

// lineOf is the shell test's line_of: the 1-based number of the first line of
// out that holds needle as a fixed string, or 0 when no line does.
func lineOf(out, needle string) int {
	for i, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			return i + 1
		}
	}
	return 0
}

// repoRoot is the checkout under test, taken from this file's path so a test
// reads the tree it was compiled in rather than the working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(here)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("checkout root %s has no go.mod, so the test would read no Makefile: %v", root, err)
	}
	return root
}

// makefileText is the checkout's Makefile. A case reads it directly for what
// make -n cannot show: a recipe line holding $(MAKE) runs even under -n.
func makefileText(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	return string(data)
}

// hasTargetLine reports whether text declares target, as the shell's
// grep -E '^target:' does.
func hasTargetLine(text, target string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, target+":") {
			return true
		}
	}
	return false
}

// recipesRecursingThroughMake returns the recipe lines of the gate-shape
// targets that call $(MAKE); make runs such a line even under -n, so a dry run
// of one is not a dry run of what it recurses into.
func recipesRecursingThroughMake(text string) []string {
	var found []string
	inRecipe := false
	for _, line := range strings.Split(text, "\n") {
		if isGateShapeTarget(line) {
			inRecipe = true
			continue
		}
		if !strings.HasPrefix(line, "\t") {
			inRecipe = false
		}
		if inRecipe && strings.Contains(line, "$(MAKE)") {
			found = append(found, line)
		}
	}
	return found
}

// isGateShapeTarget reports whether line declares one of the targets whose
// recipe shape the gate-shape cases pin.
func isGateShapeTarget(line string) bool {
	for _, target := range []string{"gate", "test-slow", "test-integration", "test"} {
		if strings.HasPrefix(line, target+":") {
			return true
		}
	}
	return false
}

// dryGate is make -n gate, the output the gate-shape cases read. make failing
// fails the case: no assertion may run against a partial dry run.
func dryGate(t *testing.T, m Make) string {
	t.Helper()
	out, err := m("gate")
	if err != nil {
		t.Fatalf("make -n gate: %v\n%s", err, out)
	}
	return out
}
