package testpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RealGitFiles applies the no-git detection of GitFreeFindings to every
// package directory in dirs that gitfree.txt does not list, and returns the
// repo-relative unit-tier test files that run real git, each with its
// violations. Packages in gitfree.txt are left to CheckGitFree, which allows
// no file at all.
func RealGitFiles(root string, dirs []string, gitFree map[string]bool) (map[string][]Violation, error) {
	files := map[string][]Violation{}
	for _, dir := range dirs {
		rel := relPath(root, dir)
		if gitFree[rel] {
			continue
		}
		vs, _, err := GitFreeFindings(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		for _, v := range vs {
			if v.Rule != RuleNoGit {
				continue
			}
			f := relPath(root, v.Pos.Filename)
			files[f] = append(files[f], v)
		}
	}
	return files, nil
}

// CheckRealGit holds the unit tier outside gitfree.txt to the realgit.txt
// baseline: the test files that still ran real git when the baseline was
// taken. It returns one finding per problem:
//   - a unit-tier file that runs git but is not in the baseline (a new one);
//   - a baseline file that no longer runs git (delete its line);
//   - a baseline file that does not exist;
//   - a baseline file in a package unconverted.txt does not list: a package
//     whose unit tier runs git is not converted.
//
// The baseline only shrinks, so the unit tier never gains a real-git file.
func CheckRealGit(root string, found map[string][]Violation, baseline, unconverted map[string]bool) []string {
	var findings []string
	for _, f := range sortedKeys(baseline) {
		if pkg := filepath.ToSlash(filepath.Dir(f)); !unconverted[pkg] {
			findings = append(findings, fmt.Sprintf("realgit.txt lists %s, but unconverted.txt does not list %s: a package whose unit tier runs git is not converted", f, pkg))
		}
	}
	for _, f := range sortedKeys(found) {
		if baseline[f] {
			continue
		}
		for _, v := range found[f] {
			findings = append(findings, v.String())
		}
		findings = append(findings, fmt.Sprintf("%s runs real git in the unit tier: use gitfake or canned output, or move the test to the integration tier (realgit.txt only shrinks)", f))
	}
	for _, f := range sortedKeys(baseline) {
		if _, ok := found[f]; ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			findings = append(findings, fmt.Sprintf("realgit.txt lists %s, which does not exist: delete its line and lower maxRealGit", f))
			continue
		}
		findings = append(findings, fmt.Sprintf("realgit.txt lists %s, which no longer runs git in the unit tier: delete its line and lower maxRealGit", f))
	}
	return findings
}

// ConvertedShare counts the test lines (every *_test.go file, integration
// tier included) in converted packages against all test lines. A package is
// converted when unconverted.txt does not list it and none of its files is
// in the realgit.txt baseline.
func ConvertedShare(root string, dirs []string, unconverted, realGit map[string]bool) (converted, total int, err error) {
	gitPkgs := map[string]bool{}
	for f := range realGit {
		gitPkgs[filepath.ToSlash(filepath.Dir(f))] = true
	}
	for _, dir := range dirs {
		rel := relPath(root, dir)
		matches, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if err != nil {
			return 0, 0, err
		}
		n := 0
		for _, m := range matches {
			b, err := os.ReadFile(m)
			if err != nil {
				return 0, 0, err
			}
			n += strings.Count(string(b), "\n")
		}
		total += n
		if !unconverted[rel] && !gitPkgs[rel] {
			converted += n
		}
	}
	return converted, total, nil
}

func relPath(root, p string) string {
	return filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
