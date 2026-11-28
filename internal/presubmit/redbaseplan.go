package presubmit

import (
	"bytes"
	"fmt"
	"go/build"
	"io"
	"maps"
	"path"
	"slices"
	"strings"
)

// RedBasePackage is one package directory whose test files the branch changes.
type RedBasePackage struct {
	Dir   string
	Files []string      // the test files the branch adds or modifies, sorted
	Tests []RedBaseTest // the tests the branch adds or whose body it changes, sorted by name
}

// RedBasePlanResult is the plan of what the red-on-base check runs.
type RedBasePlanResult struct {
	Packages  []RedBasePackage // sorted by Dir
	NotJudged []string         // changed test files under a nested module, sorted
}

// RedBasePlan names, per package directory, the test files base...HEAD adds or modifies and the tests the branch adds or whose body it changes, with the line of each declaration at HEAD, so the gate can run those tests against the merge base and see them fail (gt-13dre6, gt-v6ay97). The file list and the nested-module not-judged files come from RedBaseFiles, the names from ChangedTests, the build exclusion from RedBaseBuilds (a file the gate's build excludes is not judged, gt-6xw9zr) and the lines from RedBaseTests. A failed file list, merge-base read, file read or show, and a file that does not parse at HEAD or at the merge base are errors, so a branch the check could not read never reads as a branch with no test to run.
func RedBasePlan(run Runner, read FileReader, base string) (RedBasePlanResult, error) {
	var res RedBasePlanResult
	files, notJudged, err := RedBaseFiles(run, base)
	if err != nil {
		return res, err
	}
	res.NotJudged = notJudged
	if len(files) == 0 {
		return res, nil
	}
	mb, err := mergeBase(run, base)
	if err != nil {
		return res, err
	}
	byDir := map[string]*RedBasePackage{}
	for _, f := range files {
		head, err := read(f.Path)
		if err != nil {
			return res, fmt.Errorf("reading %s: %w", f.Path, err)
		}
		var before []byte // an added file is absent at the merge base
		if !f.Added {
			if before, err = run([]string{"show", mb + ":" + f.Path}); err != nil {
				return res, fmt.Errorf("reading %s at the merge base: %w", f.Path, err)
			}
		}
		names, err := ChangedTests(f.Path, before, head)
		if err != nil {
			return res, fmt.Errorf("%s: %w", f.Path, err)
		}
		tests, err := RedBaseTests(f.Path, head, names)
		if err != nil {
			return res, fmt.Errorf("%s: %w", f.Path, err)
		}
		// The file is parsed before the build check, so a file that does not parse is an error whatever its constraints. The gate's build compiles no test in an excluded file, so no run can judge it (gt-6xw9zr).
		builds, err := RedBaseBuilds(&build.Default, f.Path, head)
		if err != nil {
			return res, err
		}
		if !builds {
			res.NotJudged = append(res.NotJudged, f.Path)
			continue
		}
		dir := path.Dir(f.Path)
		pkg := byDir[dir]
		if pkg == nil {
			pkg = &RedBasePackage{Dir: dir}
			byDir[dir] = pkg
		}
		pkg.Files = append(pkg.Files, f.Path) // files arrive sorted from RedBaseFiles
		pkg.Tests = append(pkg.Tests, tests...)
	}
	slices.Sort(res.NotJudged)
	for _, dir := range slices.Sorted(maps.Keys(byDir)) {
		pkg := byDir[dir]
		slices.SortFunc(pkg.Tests, func(a, b RedBaseTest) int { return strings.Compare(a.Name, b.Name) })
		res.Packages = append(res.Packages, *pkg)
	}
	return res, nil
}

// RedBaseBuilds reports whether the go tool compiles the test file p, with source head, under ctx: its name (GOOS and GOARCH suffixes) and its build constraints. The source is served from head, so the file is not read from disk. A constraint that does not parse is an error naming p, never read as excluded or included.
func RedBaseBuilds(ctx *build.Context, p string, head []byte) (bool, error) {
	c := *ctx
	c.OpenFile = func(string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(head)), nil
	}
	ok, err := c.MatchFile(path.Dir(p), path.Base(p))
	if err != nil {
		return false, fmt.Errorf("%s: build constraints: %w", p, err)
	}
	return ok, nil
}
