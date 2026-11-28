package presubmit

import (
	"cmp"
	"fmt"
	"slices"
)

// RedBaseFinding is a new or changed test that passed in the tree: it does not need the change.
type RedBaseFinding struct {
	Dir  string
	Test RedBaseTest
}

// JudgeRedBase returns the findings JudgeRedBaseRuns computes, the planned tests that passed in tree, and drops its red count (gt-lbmdp6).
func JudgeRedBase(plan RedBasePlanResult, tree string, goTest func(dir string, args []string) ([]byte, error)) ([]RedBaseFinding, error) {
	findings, _, err := JudgeRedBaseRuns(plan, tree, goTest)
	return findings, err
}

// JudgeRedBaseRuns runs each planned package's named tests in tree through goTest and returns a finding for each of them that passed, sorted by package and test name, and red, how many planned tests failed in that tree (gt-kwurob, gt-gxnlab, gt-lbmdp6). A planned test is red when it has a fail event, or when it has no event under a package whose build failed or that failed without running it; a skipped test is neither red nor a finding. A package with no names runs nothing. goTest errors only when go could not be run, never for a non-zero exit; that error is returned naming the package, with no findings, so an unrun package never reads as one whose tests all failed. A planned test with no pass, fail or skip event under a package that did not fail is an error naming the package and the test, so a test go never ran never counts as one proven to fail without the change; a failed package is that proof, and its missing event is red.
func JudgeRedBaseRuns(plan RedBasePlanResult, tree string, goTest func(dir string, args []string) ([]byte, error)) (findings []RedBaseFinding, red int, err error) {
	for _, pkg := range plan.Packages {
		names := make([]string, 0, len(pkg.Tests))
		for _, t := range pkg.Tests {
			names = append(names, t.Name)
		}
		args := RedBaseGoArgs(pkg.Dir, names)
		if args == nil {
			continue
		}
		out, err := goTest(tree, args)
		if err != nil {
			return nil, 0, fmt.Errorf("running the tests of package %s: %w", pkg.Dir, err)
		}
		events := RedBaseRan(out)
		for _, t := range pkg.Tests {
			switch {
			case slices.Contains(events.Failed, t.Name):
				red++
			case slices.Contains(events.Passed, t.Name):
				findings = append(findings, RedBaseFinding{Dir: pkg.Dir, Test: t})
			case slices.Contains(events.Ran, t.Name):
				// A skipped test is neither a finding nor red.
			case events.FailedBuild:
				red++
			default:
				return nil, 0, fmt.Errorf("package %s: test %s has no pass, fail or skip event in its go test output", pkg.Dir, t.Name)
			}
		}
	}
	slices.SortFunc(findings, func(a, b RedBaseFinding) int {
		return cmp.Or(cmp.Compare(a.Dir, b.Dir), cmp.Compare(a.Test.Name, b.Test.Name))
	})
	return findings, red, nil
}
