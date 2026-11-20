package presubmit

import (
	"bytes"
	"fmt"
	"maps"
	"path"
	"slices"
)

// ChangedDeclsResult is what the declarations a branch changed came to.
type ChangedDeclsResult struct {
	// Names maps a directory to the sorted, de-duplicated names of the top-level
	// declarations the branch adds or modifies in it.
	Names map[string][]string
	// Declarations is how many names Names holds across every directory.
	Declarations int
}

// ChangedDecls reports the top-level declarations the Go files of base...HEAD
// add or modify, grouped by the directory each file is in (gt-k2mjzs).
//
// A line the branch adds names the declaration whose span holds it, judged on
// the file as it is at HEAD (declsTouching). A test file is not read, and a file
// the go tool skips is not read either. Declarations deleted by the branch are
// not listed here. A git-quoted changed path is an error rather than a skipped
// file, because a declaration in it would go unjudged. A file that does not
// parse, a failed git command and an empty base are errors too.
func ChangedDecls(run Runner, base string) (ChangedDeclsResult, error) {
	res := ChangedDeclsResult{Names: map[string][]string{}}
	if base == "" {
		return res, fmt.Errorf("changed declarations: empty base")
	}
	out, err := run([]string{"diff", "--unified=0", "--end-of-options", base + "...HEAD", "--", "*.go"})
	if err != nil {
		return res, fmt.Errorf("git diff %s...HEAD: %w", base, err)
	}
	added, err := addedLines(bytes.NewReader(out))
	if err != nil {
		return res, err
	}
	lines := map[string][]int{}
	var paths []string
	for _, a := range added {
		if a.path == "" {
			return res, fmt.Errorf("git quoted a changed path in the diff against %s; its declarations cannot be read", base)
		}
		if !isGoSource(a.path) {
			continue
		}
		if _, seen := lines[a.path]; !seen {
			paths = append(paths, a.path)
		}
		lines[a.path] = append(lines[a.path], a.line)
	}
	dirs := map[string]map[string]bool{}
	for _, p := range paths {
		src, err := run([]string{"show", "HEAD:" + p})
		if err != nil {
			return res, fmt.Errorf("reading %s at HEAD: %w", p, err)
		}
		names, err := declsTouching(src, lines[p])
		if err != nil {
			return res, fmt.Errorf("%s: %w", p, err)
		}
		if len(names) == 0 {
			continue
		}
		dir := path.Dir(p)
		if dirs[dir] == nil {
			dirs[dir] = map[string]bool{}
		}
		for _, name := range names {
			dirs[dir][name] = true
		}
	}
	for dir, set := range dirs {
		res.Names[dir] = slices.Sorted(maps.Keys(set))
		res.Declarations += len(res.Names[dir])
	}
	return res, nil
}
