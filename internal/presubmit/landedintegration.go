package presubmit

import (
	"errors"
	"fmt"
	"go/build"
	"maps"
	"path"
	"slices"
	"strings"
)

// ChangedIntegrationTests names, per package directory, the top-level Test
// functions base...HEAD adds or whose body it changes in integration-tagged
// test files, with each declaration's line at HEAD, so post-land can run the
// tests a landing changed (gt-10zcdg). notRun names, sorted, the changed tagged
// files the check cannot run: a nested module's file, read through read, and a
// file the integration build excludes, such as one named for another GOOS.
// Neither adds a package; an added file counts every test, and a file with no
// changed test adds none. Packages sort by directory, tests by name. A failed
// file list, merge-base read, merge-base file read, head read, build check or
// parse is an error naming the file or step.
func ChangedIntegrationTests(run Runner, read FileReader, base string) (packages []RedBasePackage, notRun []string, err error) {
	files, notJudged, err := RedBaseFiles(run, base)
	if err != nil {
		return nil, nil, err
	}
	var mb string
	if len(files) > 0 {
		if mb, err = mergeBase(run, base); err != nil {
			return nil, nil, err
		}
	}
	// A nested module's tests are outside the root build, so a tagged one is
	// named as not run rather than dropped.
	for _, p := range notJudged {
		head, err := read(p)
		if err != nil {
			return nil, nil, fmt.Errorf("reading %s: %w", p, err)
		}
		if IntegrationTagged(head) {
			notRun = append(notRun, p)
		}
	}
	// The tier builds with the integration tag; without it every tagged file
	// would look excluded and nothing would run.
	ctx := build.Default
	ctx.BuildTags = append(slices.Clone(ctx.BuildTags), "integration")
	byDir := map[string]*RedBasePackage{}
	for _, f := range files {
		head, err := read(f.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("reading %s: %w", f.Path, err)
		}
		if IntegrationTagged(head) {
			builds, err := RedBaseBuilds(&ctx, f.Path, head)
			if err != nil {
				return nil, nil, err
			}
			if !builds {
				notRun = append(notRun, f.Path)
				continue
			}
		}
		var before []byte // an added file is absent at the merge base
		if !f.Added {
			if before, err = run([]string{"show", mb + ":" + f.Path}); err != nil {
				return nil, nil, fmt.Errorf("reading %s at the merge base: %w", f.Path, err)
			}
		}
		// Both calls parse head, so a second error return would be
		// unreachable; one joined error keeps both paths tested.
		names, nameErr := ChangedTaggedTests(f.Path, before, head)
		tests, testErr := RedBaseTests(f.Path, head, names)
		if nameErr != nil || testErr != nil {
			return nil, nil, fmt.Errorf("%s: %w", f.Path, errors.Join(nameErr, testErr))
		}
		if len(names) == 0 {
			continue // untagged, or no test changed
		}
		dir := path.Dir(f.Path)
		pkg := byDir[dir]
		if pkg == nil {
			pkg = &RedBasePackage{Dir: dir}
			byDir[dir] = pkg
		}
		pkg.Files = append(pkg.Files, f.Path)
		pkg.Tests = append(pkg.Tests, tests...)
	}
	slices.Sort(notRun)
	for _, dir := range slices.Sorted(maps.Keys(byDir)) {
		pkg := byDir[dir]
		slices.SortFunc(pkg.Tests, func(a, b RedBaseTest) int { return strings.Compare(a.Name, b.Name) })
		packages = append(packages, *pkg)
	}
	return packages, notRun, nil
}
