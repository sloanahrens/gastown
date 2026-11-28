package presubmit

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// RedBaseFile is one test file base...HEAD adds or modifies, with Added true when the branch creates it.
type RedBaseFile struct {
	Path  string
	Added bool
}

// RedBaseFiles names, sorted, the test files base...HEAD adds or modifies, with files under a nested module returned in notJudged instead, so the red-on-base check knows which tests to run against the merge base (gt-v6ay97). An empty base, a failed diff or unreadable line, a quoted path and a failed ls-files are errors, so a branch the check could not read never reads as a branch with no test file.
func RedBaseFiles(run Runner, base string) (files []RedBaseFile, notJudged []string, err error) {
	if base == "" {
		return nil, nil, fmt.Errorf("red-base files: empty base")
	}
	revs := base + "...HEAD"
	out, err := run([]string{"-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff", "--name-status", "--no-renames", "--end-of-options", revs, "--", "*.go"})
	if err != nil {
		return nil, nil, fmt.Errorf("git diff --name-status %s: %w", revs, err)
	}
	var changed []RedBaseFile
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		status, p, ok := strings.Cut(line, "\t")
		if !ok || len(status) != 1 {
			return nil, nil, fmt.Errorf("unreadable name-status line")
		}
		if strings.HasPrefix(p, `"`) {
			return nil, nil, fmt.Errorf("git quoted a changed path; its changed tests cannot be named")
		}
		if (status == "A" || status == "M") && isTestFile(p) {
			changed = append(changed, RedBaseFile{Path: p, Added: status == "A"})
		}
	}
	if len(changed) == 0 {
		return nil, nil, nil
	}
	modules, err := nestedModuleDirs(run)
	if err != nil {
		return nil, nil, err
	}
	for _, f := range changed {
		if inNestedModule(f.Path, modules) {
			notJudged = append(notJudged, f.Path)
			continue
		}
		files = append(files, f)
	}
	slices.SortFunc(files, func(a, b RedBaseFile) int { return strings.Compare(a.Path, b.Path) })
	slices.Sort(notJudged)
	return files, notJudged, nil
}

// isTestFile reports whether p is a test file the go tool builds: not under testdata or a directory starting _ or ., and not itself named starting _ or .
func isTestFile(p string) bool {
	if !strings.HasSuffix(p, "_test.go") || strings.HasPrefix(path.Base(p), "_") || strings.HasPrefix(path.Base(p), ".") {
		return false
	}
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if seg == "." {
			continue
		}
		if seg == "testdata" || strings.HasPrefix(seg, "_") || strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

// nestedModuleDirs lists the directories below the root that hold a go.mod, where a test file belongs to a module other than the root's.
func nestedModuleDirs(run Runner) ([]string, error) {
	out, err := run([]string{"ls-files", "-z", "--full-name", "--", "*/go.mod"})
	if err != nil {
		return nil, fmt.Errorf("git ls-files go.mod: %w", err)
	}
	var dirs []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			dirs = append(dirs, path.Dir(p))
		}
	}
	return dirs, nil
}

// inNestedModule reports whether p lies in a directory of its own module; a sibling whose name only shares a prefix is not inside it.
func inNestedModule(p string, modules []string) bool {
	dir := path.Dir(p)
	for _, m := range modules {
		if dir == m || strings.HasPrefix(dir, m+"/") {
			return true
		}
	}
	return false
}
