package done

import (
	"fmt"
	"io/fs"
	"regexp"
)

// gateTestLister lists the test names one Gate line's -run pattern selects, from the
// packages the line names under its tags, in the directory dir.
type gateTestLister func(dir string, gr GateRun) ([]string, error)

// newStaticGateLister is the gateTestLister gt done wires. It refuses a -run pattern that does
// not compile before go list runs, then keeps the names packageTestNames returns that the
// pattern selects, each name once at its first position in that order (gt-002qkd). An error
// from a step ends the call and is returned as it is.
func newStaticGateLister(run gateListCommand, environ func() []string, openDir func(dir string) fs.FS) gateTestLister {
	return func(dir string, gr GateRun) ([]string, error) {
		// The first element compiles once, so a bad pattern is refused even when no name reaches it.
		// An empty element compiles to a regexp matching every name, as runPatternSelects does.
		re, err := regexp.Compile(firstRunPatternElement(gr.Pattern))
		if err != nil {
			return nil, fmt.Errorf("-run pattern %q: first element does not compile: %w", gr.Pattern, err)
		}
		names, err := packageTestNames(run, environ, openDir, dir, gr.Tags, gr.Packages)
		if err != nil {
			return nil, err
		}
		var selected []string
		seen := make(map[string]bool, len(names))
		for _, name := range names {
			if seen[name] {
				continue
			}
			seen[name] = true
			if re.MatchString(name) {
				selected = append(selected, name)
			}
		}
		return selected, nil
	}
}

// packageTestNames returns the names of every test the packages declare under tags, in
// package then file order. go list gives each package's Dir and test files, and the parser
// reads the names: no test binary starts, so no TestMain runs (gt-icmj4w). environ is read
// when this runs, so wiring it reads no host state.
func packageTestNames(run gateListCommand, environ func() []string, openDir func(dir string) fs.FS, dir, tags string, packages []string) ([]string, error) {
	listed, err := goListTestFiles(run, environ(), dir, tags, packages)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, pkg := range listed {
		got, err := testFuncNames(openDir(pkg.Dir), pkg.Files)
		if err != nil {
			return nil, err
		}
		names = append(names, got...)
	}
	return names, nil
}
