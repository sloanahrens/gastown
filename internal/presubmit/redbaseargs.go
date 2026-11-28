package presubmit

import "strings"

// RedBaseGoArgs returns the go test arguments that run the named tests in the package directory dir (gt-ujs5bz). The -run expression is the anchored alternation of the names, so it matches those tests and no others, and the package argument is "./" followed by dir, or "." for the root directory. No names gives nil, since there is nothing to run.
func RedBaseGoArgs(dir string, names []string) []string {
	if len(names) == 0 {
		return nil
	}
	pkg := "./" + dir
	if dir == "." {
		pkg = "."
	}
	return []string{"test", "-json", "-run", "^(" + strings.Join(names, "|") + ")$", "-count=1", pkg}
}
