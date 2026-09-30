package testutil

import (
	"go/build"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// hermeticExempt lists packages that are allowed to skip the hermetic
// TestMain harness despite having a risky test dependency closure. Every
// entry needs a justification; prefer adopting the harness over adding one.
var hermeticExempt = map[string]string{}

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
//
// Each test binary's dependency closure comes from the shared parse of the
// module (sharedRepoTree), not from `go list -test`: the closure follows the
// module's own packages through their build-selected files, and a dependency
// outside the module is judged by its import path.
// TestIntegrationHermeticGuardAgreesWithGoList checks the result against go
// list.
func TestHermeticHarnessEnforced(t *testing.T) {
	t.Parallel()
	tree := sharedRepoTree(t)

	var flagged []string
	for pkg, deps := range tree.testBinaryDeps() {
		risky := false
		for dep := range deps {
			if riskyDepPattern.MatchString(dep) {
				risky = true
				break
			}
		}
		if !risky {
			risky = packageTestSourceIsRisky(t, tree.root, pkg)
		}
		if !risky {
			continue
		}
		if _, exempt := hermeticExempt[pkg]; exempt {
			continue
		}
		if !packageUsesHarness(t, tree.root, pkg) {
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

// buildFiles returns dir's files the default build context selects (GOOS,
// GOARCH and build constraints, no tags), with or without its tests.
func (tr *repoTree) buildFiles(dir string, tests bool) []*repoFile {
	var out []*repoFile
	for _, f := range tr.files[dir] {
		if f.ast == nil || (!tests && strings.HasSuffix(f.name, "_test.go")) {
			continue
		}
		if ok, err := build.Default.MatchFile(dir, f.name); err != nil || !ok {
			continue
		}
		out = append(out, f)
	}
	return out
}

func importsOf(files []*repoFile) []string {
	var out []string
	for _, f := range files {
		for _, imp := range f.ast.Imports {
			if p, err := strconv.Unquote(imp.Path.Value); err == nil {
				out = append(out, p)
			}
		}
	}
	return out
}

// testBinaryDeps returns, for every package with test files, what `go list
// -test` reports as its test binary's Deps: every package it links, found by
// following the module's packages through their non-test files. Packages
// under directories go ./... skips (a leading "." or "_") are left out.
func (tr *repoTree) testBinaryDeps() map[string]map[string]bool {
	closure := map[string]map[string]bool{} // import path -> its deps, module packages only
	var depsOf func(path string) map[string]bool
	depsOf = func(path string) map[string]bool {
		if d, ok := closure[path]; ok {
			return d
		}
		deps := map[string]bool{}
		closure[path] = deps // cycles are compile errors; this just stops the walk
		dir := filepath.Join(tr.root, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(path, tr.module), "/")))
		for _, imp := range importsOf(tr.buildFiles(dir, false)) {
			deps[imp] = true
			if imp == tr.module || strings.HasPrefix(imp, tr.module+"/") {
				for d := range depsOf(imp) {
					deps[d] = true
				}
			}
		}
		return deps
	}

	out := map[string]map[string]bool{}
	for _, dir := range tr.dirs {
		rel, _ := filepath.Rel(tr.root, dir)
		if skippedByGoList(rel) {
			continue
		}
		files := tr.buildFiles(dir, true)
		hasTest := false
		for _, f := range files {
			if strings.HasSuffix(f.name, "_test.go") {
				hasTest = true
				break
			}
		}
		if !hasTest {
			continue
		}
		pkg := tr.importPath(dir)
		deps := map[string]bool{}
		for _, imp := range importsOf(files) {
			deps[imp] = true
			if imp == tr.module || strings.HasPrefix(imp, tr.module+"/") {
				for d := range depsOf(imp) {
					deps[d] = true
				}
			}
		}
		for d := range depsOf(pkg) {
			deps[d] = true
		}
		delete(deps, pkg)
		out[pkg] = deps
	}
	return out
}

// skippedByGoList reports whether ./... leaves out the directory at rel: one
// with any path element starting with "." or "_".
func skippedByGoList(rel string) bool {
	if rel == "." {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "_") {
			return true
		}
	}
	return false
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
