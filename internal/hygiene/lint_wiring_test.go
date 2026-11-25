package hygiene

import (
	"os"
	"strings"
	"testing"
)

// lintScanStep is the command make lint runs to enforce the tree scan. The scan
// needs real git over the whole tracked tree, which the unit tier never starts,
// so the gate enforces it through make lint (gt-xl8mb8) and this test holds the
// wiring: the command's own behaviour is tested in cmd/scan with a fake git. The
// step carries the scan's flag variable, empty for make lint (gt-5suext).
const lintScanStep = "go run ./internal/hygiene/cmd/scan $(HYGIENE_SCAN_FLAGS)"

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
	if !recipeRunsScan(recipe) {
		t.Fatalf("the lint recipe has no line that is exactly %q: a commented, chained or exit-swallowing step does not enforce the scan", "\t"+lintScanStep)
	}
}

// recipeRunsScan reports whether the recipe holds the scan as a whole line of
// its own, so its exit status is the line's: a make comment, a trailing
// "|| true" or a "; true" leaves the line different and does not count.
func recipeRunsScan(recipe string) bool {
	for _, l := range strings.Split(recipe, "\n") {
		if strings.TrimRight(l, " ") == "\t"+lintScanStep {
			return true
		}
	}
	return false
}

// TestWiringCheckRefusesADisabledScan checks the exact-line rule on the ways a
// lint line can keep the command text and still not fail the gate.
func TestWiringCheckRefusesADisabledScan(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		recipe string
		want   bool
	}{
		{"the plain step", "\tgo run ./cmd/repo-guards\n\t" + lintScanStep, true},
		{"trailing spaces", "\t" + lintScanStep + "  ", true},
		{"commented out", "\t# " + lintScanStep, false},
		{"exit swallowed", "\t" + lintScanStep + " || true", false},
		{"chained to a success", "\t" + lintScanStep + "; true", false},
		{"dash prefix ignores the exit", "\t-" + lintScanStep, false},
		{"only echoed", "\t@echo " + lintScanStep, false},
		{"absent", "\tgo run ./cmd/repo-guards", false},
	} {
		if got := recipeRunsScan(c.recipe); got != c.want {
			t.Errorf("%s: recipeRunsScan = %v, want %v", c.name, got, c.want)
		}
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
	if recipe, ok := lintRecipe("lint:fast:\n\tgo vet\n\nlint: docs-lint\n\t" + lintScanStep + "\n"); !ok || !strings.Contains(recipe, lintScanStep) {
		t.Fatalf("a target named lint:fast: was read instead of lint: (%q, %v)", recipe, ok)
	}
}

// lintRecipe returns the tab-indented lines under the lint target, up to the
// next line that starts a target or a comment block.
func lintRecipe(makefile string) (string, bool) {
	lines := strings.Split(makefile, "\n")
	start := -1
	for i, l := range lines {
		if l == "lint:" || strings.HasPrefix(l, "lint: ") {
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

// scanFlagsVar carries the scan's flags: empty for make lint, set by gate-lint
// to the changed files (gt-5suext).
const scanFlagsVar = "HYGIENE_SCAN_FLAGS"

// gateScanFlags is what gate-lint sets; the gate's checkout has origin/main.
const gateScanFlags = "-changed-since origin/main"

// TestLintWiringGateScansChangedFiles fails when the gate's lint stops scanning
// only the changed files, or make lint stops scanning the whole tree (gt-5suext).
func TestLintWiringGateScansChangedFiles(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatalf("reading the Makefile: %v", err)
	}
	if !gateLintScansChangedFiles(string(data)) {
		t.Fatalf("gate-lint does not pass %q to the lint scan, or make lint does not keep the full scan", gateScanFlags)
	}
}

// TestGateLintScanWiringRefusesAnUnwiredScan mutates one line of a wired
// Makefile per row. A row whose text is not in the base once fails, so a
// mutation cannot pass by changing nothing.
func TestGateLintScanWiringRefusesAnUnwiredScan(t *testing.T) {
	t.Parallel()
	const gateLine = "gate-lint: " + scanFlagsVar + " := " + gateScanFlags + "\n"
	base := scanFlagsVar + " :=\n\nlint: docs-lint\n\t" + lintScanStep + "\n\n" + gateLine + "gate-lint: exec-tax-preflight lint\n"
	if !gateLintScansChangedFiles(base) {
		t.Fatal("the wired base Makefile is refused")
	}
	for _, c := range []struct{ name, old, new string }{
		{"gate-lint drops the variable", gateLine, ""},
		{"gate-lint line commented out", "gate-lint: " + scanFlagsVar, "# gate-lint: " + scanFlagsVar},
		{"gate-lint names another base", gateScanFlags, "-changed-since HEAD~1"},
		{"gate-lint swallows the exit", gateScanFlags, gateScanFlags + " || true"},
		{"variable set under another target", "gate-lint: " + scanFlagsVar, "docs-lint: " + scanFlagsVar},
		{"default lint carries the flag", scanFlagsVar + " :=\n", scanFlagsVar + " := " + gateScanFlags + "\n"},
		{"lint scan line drops the variable", " $(" + scanFlagsVar + ")", ""},
	} {
		if strings.Count(base, c.old) != 1 {
			t.Fatalf("%s: %q is not in the base once", c.name, c.old)
		}
		if gateLintScansChangedFiles(strings.Replace(base, c.old, c.new, 1)) {
			t.Errorf("%s: the wiring check passed the mutation", c.name)
		}
	}
}

// gateLintScansChangedFiles reports whether the Makefile text sets the scan's
// default to empty, sets gate-lint's to gateScanFlags, and runs the scan line
// with the variable on it.
func gateLintScansChangedFiles(makefile string) bool {
	var defaults []string
	gateSet := false
	for _, l := range strings.Split(makefile, "\n") {
		if strings.HasPrefix(l, scanFlagsVar+" ") {
			defaults = append(defaults, strings.TrimRight(l, " "))
		}
		if v, ok := strings.CutPrefix(l, "gate-lint: "+scanFlagsVar+" := "); ok {
			gateSet = gateSet || strings.TrimRight(v, " ") == gateScanFlags
		}
	}
	recipe, ok := lintRecipe(makefile)
	return ok && len(defaults) == 1 && defaults[0] == scanFlagsVar+" :=" && gateSet && recipeRunsScan(recipe)
}
