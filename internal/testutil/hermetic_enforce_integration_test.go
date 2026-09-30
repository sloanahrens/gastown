//go:build integration

package testutil

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// TestIntegrationHermeticGuardAgreesWithGoList pins the in-process
// dependency closure TestHermeticHarnessEnforced uses to the go command's:
// every test binary `go list -test` reports is found, and the two agree on
// which ones link a risky package.
func TestIntegrationHermeticGuardAgreesWithGoList(t *testing.T) {
	tree := sharedRepoTree(t)
	cmd := exec.Command("go", "list", "-test", "-f", "{{.ImportPath}}|{{join .Deps \",\"}}", "./...")
	cmd.Dir = tree.root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -test: %v", err)
	}
	want := map[string]bool{} // test binary -> links a risky package
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		path, deps, ok := strings.Cut(line, "|")
		if !ok || !strings.HasSuffix(path, ".test") {
			continue
		}
		risky := false
		for _, dep := range strings.Split(deps, ",") {
			if riskyDepPattern.MatchString(dep) {
				risky = true
				break
			}
		}
		want[strings.TrimSuffix(path, ".test")] = risky
	}

	got := map[string]bool{}
	for pkg, deps := range tree.testBinaryDeps() {
		for dep := range deps {
			if riskyDepPattern.MatchString(dep) {
				got[pkg] = true
				break
			}
		}
		if !got[pkg] {
			got[pkg] = false
		}
	}
	var diffs []string
	for pkg, risky := range want {
		if g, ok := got[pkg]; !ok {
			diffs = append(diffs, pkg+": go list has a test binary the parse missed")
		} else if g != risky {
			diffs = append(diffs, pkg+": risky disagrees with go list")
		}
	}
	for pkg := range got {
		if _, ok := want[pkg]; !ok {
			diffs = append(diffs, pkg+": the parse found a test binary go list does not")
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		t.Errorf("in-process test binary deps disagree with go list -test:\n  %s", strings.Join(diffs, "\n  "))
	}
}
