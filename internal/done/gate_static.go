package done

import (
	"io/fs"
)

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
