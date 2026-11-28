package presubmit

import (
	"fmt"
	"io"
	"strings"
)

// RunLandedPackages runs the changed integration tests of each package through
// goTest, returning 0 when all passed and 1 when any failed (gt-42bk5a). Each
// notRun file prints "not run: <file>" on stdout before any package runs and
// changes nothing else. Each package runs with
// "test -tags integration -count=1 -run ^(names)$ ./<dir>" ("." for the root),
// even after a failure, and each failure is named on stderr; with no package,
// stdout gets one "no integration test changed" line and the result is 0. A
// failed stdout write is 1, so an unreportable run never reads as clean.
func RunLandedPackages(packages []RedBasePackage, notRun []string, stdout, stderr io.Writer, goTest func(args []string) error) int {
	ok := true
	for _, f := range notRun {
		if _, err := fmt.Fprintf(stdout, "not run: %s\n", f); err != nil {
			ok = false
			fmt.Fprintf(stderr, "not run: %s: %v\n", f, err)
		}
	}
	if len(packages) == 0 {
		if _, err := fmt.Fprintln(stdout, "no integration test changed"); err != nil {
			ok = false
			fmt.Fprintf(stderr, "no integration test changed: %v\n", err)
		}
	} else {
		for _, pkg := range packages {
			if err := goTest(landedArgs(pkg)); err != nil {
				ok = false
				fmt.Fprintf(stderr, "integration tests of %s failed: %v\n", pkg.Dir, err)
			}
		}
	}
	if !ok {
		return 1
	}
	return 0
}

// landedArgs returns the go test arguments that run the package's changed
// integration tests: the tag that selects the tier, the anchored alternation of
// their names, and the package directory, "." for the root.
func landedArgs(pkg RedBasePackage) []string {
	names := make([]string, 0, len(pkg.Tests))
	for _, t := range pkg.Tests {
		names = append(names, t.Name)
	}
	dir := "./" + pkg.Dir
	if pkg.Dir == "." {
		dir = "."
	}
	return []string{"test", "-tags", "integration", "-count=1", "-run", "^(" + strings.Join(names, "|") + ")$", dir}
}
