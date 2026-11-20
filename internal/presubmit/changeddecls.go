package presubmit

import (
	"bytes"
	"fmt"
	"io"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ChangedDeclsResult is what the declarations a branch changed came to.
type ChangedDeclsResult struct {
	// Names maps a directory to the sorted, de-duplicated names of the top-level
	// declarations the branch adds, modifies or deletes in it.
	Names map[string][]string
	// Declarations is how many names Names holds across every directory.
	Declarations int
}

// ChangedDecls reports the top-level declarations the Go files of base...HEAD
// add, modify or delete, grouped by the directory each file is in (gt-k2mjzs,
// gt-ezad81).
//
// A line the branch adds names the declaration whose span holds it, judged on
// the file as it is at HEAD (declsTouching). A line the branch removes names the
// declaration whose span at the merge base holds it, judged on the merge-base
// file, because the removed lines are not at HEAD. So a hunk that replaces a
// function names both the one it removed and the one it added. A file the diff
// deletes outright names every top-level declaration it had at the merge base.
// The diff runs without rename detection, so a renamed file reads as a deletion and
// an addition. A test file is not read, and a file the go tool skips is not read
// either. A git-quoted changed path is an error rather than a skipped file,
// because a declaration in it would go unjudged. A file that does not parse, a
// failed git command and an empty base are errors too.
func ChangedDecls(run Runner, base string) (ChangedDeclsResult, error) {
	res := ChangedDeclsResult{Names: map[string][]string{}}
	if base == "" {
		return res, fmt.Errorf("changed declarations: empty base")
	}
	out, err := run([]string{"diff", "--unified=0", "--no-renames", "--end-of-options", base + "...HEAD", "--", "*.go"})
	if err != nil {
		return res, fmt.Errorf("git diff %s...HEAD: %w", base, err)
	}
	added, err := addedLines(bytes.NewReader(out))
	if err != nil {
		return res, err
	}
	removed, err := removedLines(bytes.NewReader(out))
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
	oldLines := map[string][]int{}
	var oldPaths []string
	for _, r := range removed {
		if r.path == "" {
			return res, fmt.Errorf("git quoted a changed path in the diff against %s; its declarations cannot be read", base)
		}
		if !isGoSource(r.path) {
			continue
		}
		if _, seen := oldLines[r.path]; !seen {
			oldPaths = append(oldPaths, r.path)
		}
		for n := 0; n < r.count; n++ {
			oldLines[r.path] = append(oldLines[r.path], r.start+n)
		}
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
		addNames(dirs, p, names)
	}
	if len(oldPaths) > 0 {
		mb, err := mergeBase(run, base)
		if err != nil {
			return res, err
		}
		for _, p := range oldPaths {
			src, err := run([]string{"show", mb + ":" + p})
			if err != nil {
				return res, fmt.Errorf("reading %s at the merge base: %w", p, err)
			}
			names, err := declsTouching(src, oldLines[p])
			if err != nil {
				return res, fmt.Errorf("%s at the merge base: %w", p, err)
			}
			addNames(dirs, p, names)
		}
	}
	for dir, set := range dirs {
		res.Names[dir] = slices.Sorted(maps.Keys(set))
		res.Declarations += len(res.Names[dir])
	}
	return res, nil
}

// addNames records names as changed declarations in the directory of file p.
func addNames(dirs map[string]map[string]bool, p string, names []string) {
	if len(names) == 0 {
		return
	}
	dir := path.Dir(p)
	if dirs[dir] == nil {
		dirs[dir] = map[string]bool{}
	}
	for _, name := range names {
		dirs[dir][name] = true
	}
}

// mergeBase returns the commit the branch forked from: the file a removed line
// is judged against, since the diff is taken from it to HEAD.
func mergeBase(run Runner, base string) (string, error) {
	out, err := run([]string{"merge-base", "--end-of-options", base, "HEAD"})
	if err != nil {
		return "", fmt.Errorf("git merge-base %s HEAD: %w", base, err)
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", fmt.Errorf("git merge-base %s HEAD: no common ancestor", base)
	}
	return sha, nil
}

// hunkRangeRE reads both sides of a hunk header, "@@ -a,b +c,d @@". A count git
// leaves out is 1, so the groups for b and d may be empty.
var hunkRangeRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,(\d+))? @@`)

// removal is the run of old lines one hunk removes, whether or not it adds lines
// back. A file the diff deletes outright is one removal that covers every line
// of the file.
type removal struct {
	path  string
	start int // the first old line removed
	count int // how many old lines it removes
}

// removedLines reads a unified diff for the hunks that remove lines, a header
// "@@ -a,b +c,d @@" with b greater than 0: the file each belongs to and the old
// lines it removes, whether or not the hunk adds lines back. A file the diff
// deletes is listed by its old path, since its new side is /dev/null.
func removedLines(r io.Reader) ([]removal, error) {
	var out []removal
	sc := newScanner(r)
	oldFile := ""
	inHunk := false
	for sc.Scan() {
		raw := strings.TrimSuffix(sc.Text(), "\r")
		if inHunk && raw != "" {
			switch raw[0] {
			case '+', '-', '\\', ' ':
				continue
			}
		}
		inHunk = false
		switch {
		case strings.HasPrefix(raw, "--- "):
			oldFile = diffOldPath(raw[len("--- "):])
		case strings.HasPrefix(raw, "@@"):
			m := hunkRangeRE.FindStringSubmatch(raw)
			if m == nil {
				return nil, fmt.Errorf("unreadable hunk header %q", raw)
			}
			start, err := strconv.Atoi(m[1])
			if err != nil {
				return nil, fmt.Errorf("unreadable hunk header %q: %w", raw, err)
			}
			oldCount, err := hunkCount(m[2])
			if err != nil {
				return nil, fmt.Errorf("unreadable hunk header %q: %w", raw, err)
			}
			if _, err := hunkCount(m[3]); err != nil {
				return nil, fmt.Errorf("unreadable hunk header %q: %w", raw, err)
			}
			inHunk = true
			if oldCount > 0 {
				out = append(out, removal{path: oldFile, start: start, count: oldCount})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// hunkCount reads one count of a hunk header; an empty count is 1.
func hunkCount(s string) (int, error) {
	if s == "" {
		return 1, nil
	}
	return strconv.Atoi(s)
}

// diffOldPath reads the path on a "--- " line. A file created names /dev/null,
// and a path git quoted reads as the empty string, as diffPath does.
func diffOldPath(s string) string {
	s = strings.TrimSuffix(s, "\t")
	if s == "/dev/null" || strings.HasPrefix(s, `"`) {
		return ""
	}
	return strings.TrimPrefix(s, "a/")
}
