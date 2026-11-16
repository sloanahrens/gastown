package hygiene

import (
	"os"
	"strings"
	"testing"
)

// lintScanStep is the command make lint runs to enforce the tree scan. The scan
// needs real git over the whole tracked tree, which the unit tier never starts,
// so the gate enforces it through make lint (gt-xl8mb8) and this test holds the
// wiring: the command's own behaviour is tested in cmd/scan with a fake git.
const lintScanStep = "go run ./internal/hygiene/cmd/scan"

// TestLintRunsTheHygieneScan fails when the lint recipe no longer runs the tree
// scan, so the enforcement cannot be dropped without a test failing.
func TestLintRunsTheHygieneScan(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("reading the Makefile: %v", err)
	}
	recipe, ok := lintRecipe(string(data))
	if !ok {
		t.Fatal("the Makefile has no lint target")
	}
	if !strings.Contains(recipe, lintScanStep) {
		t.Fatalf("the lint recipe does not run %q", lintScanStep)
	}
}

// TestLintRecipeIsReadFromItsTargetOnly checks the reader that finds the lint
// recipe: a scan step under another target does not count, and the recipe ends
// at the next target.
func TestLintRecipeIsReadFromItsTargetOnly(t *testing.T) {
	t.Parallel()
	const other = "build:\n\t" + lintScanStep + "\n\nlint: docs-lint\n\tgo run ./cmd/repo-guards\n\ndocs-lint:\n\t" + lintScanStep + "\n"
	recipe, ok := lintRecipe(other)
	if !ok {
		t.Fatal("lint target not found")
	}
	if strings.Contains(recipe, lintScanStep) {
		t.Fatalf("a scan step under another target counted for lint: %q", recipe)
	}
	if _, ok := lintRecipe("build:\n\tgo build\n"); ok {
		t.Fatal("found a lint target in a Makefile without one")
	}
}

// lintRecipe returns the tab-indented lines under the lint target, up to the
// next line that starts a target or a comment block.
func lintRecipe(makefile string) (string, bool) {
	lines := strings.Split(makefile, "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "lint:") {
			start = i
			break
		}
	}
	if start < 0 {
		return "", false
	}
	var out []string
	for _, l := range lines[start+1:] {
		if l == "" || strings.HasPrefix(l, "#") || !strings.HasPrefix(l, "\t") {
			break
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), true
}
