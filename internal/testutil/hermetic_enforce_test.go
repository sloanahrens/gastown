package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// hermeticExempt lists packages that are allowed to skip the hermetic
// TestMain harness despite having a risky test dependency closure. Every
// entry needs a justification; prefer adopting the harness over adding one.
var hermeticExempt = map[string]string{
	"github.com/steveyegge/gastown/internal/testutil": "the harness itself; its tests build and probe sandbox fixtures directly",
}

// riskyDepPattern matches dependency import paths that can reach live town
// state: the beads SDK (Dolt-backed issue tracker) and the Dolt server
// manager. internal/events is deliberately absent — its cwd-resolving write
// path already no-ops under tests (see internal/events.write).
var riskyDepPattern = regexp.MustCompile(`^github\.com/steveyegge/(beads(/|$)|gastown/internal/doltserver$)`)

// riskyTestSource matches test code that shells out to the gt/bd/dolt
// binaries, which inherit the process env and can reach live town state even
// when the package's import graph looks harmless.
var riskyTestSource = regexp.MustCompile(`exec\.Command\(\s*"(gt|bd|dolt)"|New(Isolated)?(GT|BD)Command\(`)

// harnessMarker matches adoption of the hermetic harness in a test file.
var harnessMarker = regexp.MustCompile(`testutil\.(HermeticMain|StartHermetic)\(|\b(HermeticMain|StartHermetic)\(`)

// TestHermeticHarnessEnforced fails when a package whose tests can touch live
// town state (Dolt, beads, gt/bd subprocesses) does not run under the
// hermetic harness (testutil.HermeticMain or StartHermetic in a TestMain).
//
// This is the CI guard for gt-lwi: running `go test ./...` from a worktree
// inside a live town must never mutate that town.
func TestHermeticHarnessEnforced(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not available")
	}
	moduleRoot := findModuleRoot(t)

	// One `go list -test` pass gives every test binary's full dependency
	// closure. Lines look like: <import-path>|<dep>,<dep>,...
	cmd := exec.Command(goBin, "list", "-test", "-f", "{{.ImportPath}}|{{join .Deps \",\"}}", "./...") //nolint:gosec // fixed args
	cmd.Dir = moduleRoot
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list -test failed: %v\n%s", err, ee.Stderr)
		}
		t.Fatalf("go list -test failed: %v", err)
	}

	var flagged []string
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, deps, ok := strings.Cut(line, "|")
		if !ok || !strings.HasSuffix(path, ".test") {
			continue
		}
		pkg := strings.TrimSuffix(path, ".test")
		if seen[pkg] {
			continue
		}
		seen[pkg] = true

		risky := false
		for _, dep := range strings.Split(deps, ",") {
			if riskyDepPattern.MatchString(dep) {
				risky = true
				break
			}
		}
		if !risky {
			risky = packageTestSourceIsRisky(t, moduleRoot, pkg)
		}
		if !risky {
			continue
		}
		if _, exempt := hermeticExempt[pkg]; exempt {
			continue
		}
		if !packageUsesHarness(t, moduleRoot, pkg) {
			flagged = append(flagged, pkg)
		}
	}

	sort.Strings(flagged)
	if len(flagged) > 0 {
		t.Errorf("packages whose tests can reach live town state (Dolt/beads/gt/bd) but do not use the hermetic harness:\n  %s\n\n"+
			"Add a TestMain calling testutil.HermeticMain (WithDolt() if the tests need a Dolt server), e.g.:\n\n"+
			"\tfunc TestMain(m *testing.M) {\n\t\tos.Exit(testutil.HermeticMain(m))\n\t}\n\n"+
			"or add an entry to hermeticExempt with a justification (internal/testutil/hermetic_enforce_test.go).",
			strings.Join(flagged, "\n  "))
	}
}

// packageDir maps an import path in this module to its directory.
func packageDir(moduleRoot, pkg string) string {
	rel := strings.TrimPrefix(pkg, "github.com/steveyegge/gastown/")
	return filepath.Join(moduleRoot, filepath.FromSlash(rel))
}

// packageTestSourceIsRisky reports whether any *_test.go in the package
// shells out to gt/bd/dolt.
func packageTestSourceIsRisky(t *testing.T, moduleRoot, pkg string) bool {
	t.Helper()
	for _, data := range readTestFiles(t, packageDir(moduleRoot, pkg)) {
		if riskyTestSource.Match(data) {
			return true
		}
	}
	return false
}

// packageUsesHarness reports whether any *_test.go in the package invokes the
// hermetic harness.
func packageUsesHarness(t *testing.T, moduleRoot, pkg string) bool {
	t.Helper()
	for _, data := range readTestFiles(t, packageDir(moduleRoot, pkg)) {
		if harnessMarker.Match(data) {
			return true
		}
	}
	return false
}

func readTestFiles(t *testing.T, dir string) [][]byte {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		t.Fatalf("globbing %s: %v", dir, err)
	}
	var files [][]byte
	for _, m := range matches {
		data, err := os.ReadFile(m) //nolint:gosec // paths come from the module tree
		if err != nil {
			t.Fatalf("reading %s: %v", m, err)
		}
		files = append(files, data)
	}
	return files
}

func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test working directory")
		}
		dir = parent
	}
}
