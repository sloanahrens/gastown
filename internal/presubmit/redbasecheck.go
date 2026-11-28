package presubmit

import (
	"errors"
	"fmt"
	"io"
)

// RedBaseCheck runs the red-on-base check end to end: it reads the plan, makes the merge-base tree holding the plan's files, judges the planned tests in that tree, writes the report and removes the tree (gt-mwq44k). A plan with no test makes no tree and reports zero. Once the tree exists it is removed on every path, and a failed removal is joined into any judge or report error rather than read as a clean run. It reports ok when the report found no finding, or when the judge counted at least one planned test red in the tree; a package that does not build on the base counts its planned tests red, even when every test of another package passes. When red lets findings pass it writes after the report one line, "redbase: passes: N new or changed test(s) red without the change", and a failed write of that line is returned, not ok. A plan, tree, judge, report or removal error is returned, never swallowed.
func RedBaseCheck(plan func() (RedBasePlanResult, error), tree func(files []string) (string, func() error, error), judge func(plan RedBasePlanResult, tree string) ([]RedBaseFinding, int, error), out io.Writer) (bool, error) {
	p, err := plan()
	if err != nil {
		return false, err
	}
	files, hasTests := redBasePlanInputs(p)
	if !hasTests {
		return RedBaseReport(out, p, nil)
	}
	dir, remove, err := tree(files)
	if err != nil {
		return false, err
	}
	findings, red, err := judge(p, dir)
	var ok bool
	if err == nil {
		ok, err = RedBaseReport(out, p, findings)
		if err == nil && !ok && red > 0 {
			_, err = fmt.Fprintf(out, "redbase: passes: %d new or changed test(s) red without the change\n", red)
			ok = err == nil
		}
	}
	if rmErr := remove(); rmErr != nil {
		err = errors.Join(err, rmErr)
	}
	if err != nil {
		return false, err
	}
	return ok, nil
}

// redBasePlanInputs returns, in package order, every test file the plan names, and whether it has a test to judge, so a plan with none makes no tree.
func redBasePlanInputs(plan RedBasePlanResult) (files []string, hasTests bool) {
	for _, pkg := range plan.Packages {
		files = append(files, pkg.Files...)
		hasTests = hasTests || len(pkg.Tests) > 0
	}
	return files, hasTests
}
