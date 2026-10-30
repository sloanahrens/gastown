package docslint

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Git reports when each path was last committed, which the slice is ordered by.
type Git interface {
	// LastCommit is the Unix time of the newest commit touching p. It is 0 with
	// no error when git knows no commit for p, and an error when git cannot answer.
	LastCommit(p string) (int64, error)
}

// Slice is the audit's reading order: the n docs and then the m go files whose
// last commit is oldest. A tier that cannot be read, or a git lookup that fails,
// stops it, because a shorter slice would read as the audit's whole plan. The
// error return departs from the []string signature in gt-tpg0p.110's spec.
func Slice(fsys FS, git Git, n, m int) ([]string, error) {
	docs, err := docsAndReference(fsys)
	if err != nil {
		return nil, err
	}
	goFiles, err := Go(fsys)
	if err != nil {
		return nil, err
	}
	docOrder, err := oldestFirst(docs, git)
	if err != nil {
		return nil, err
	}
	goOrder, err := oldestFirst(goFiles, git)
	if err != nil {
		return nil, err
	}
	out := firstN(docOrder, n)
	return append(out, firstN(goOrder, m)...), nil
}

// docsAndReference is the docs half of the slice: an audit may edit a file in
// either tier, so the slice offers both.
func docsAndReference(fsys FS) ([]string, error) {
	agentFacing, err := AgentFacing(fsys)
	if err != nil {
		return nil, err
	}
	reference, err := Reference(fsys)
	if err != nil {
		return nil, err
	}
	return sortedUnique(append(agentFacing, reference...)), nil
}

// oldestFirst orders paths by last commit time ascending, ties by path
// bytewise, so the same tree yields the same slice on every run and host.
func oldestFirst(paths []string, git Git) ([]string, error) {
	type dated struct {
		path string
		time int64
	}
	entries := make([]dated, 0, len(paths))
	for _, p := range paths {
		t, err := git.LastCommit(p)
		if err != nil {
			return nil, err
		}
		entries = append(entries, dated{path: p, time: t})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].time != entries[j].time {
			return entries[i].time < entries[j].time
		}
		return entries[i].path < entries[j].path
	})
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.path
	}
	return out, nil
}

// firstN is the first n paths, and none for a count of zero or less, which the
// command refuses as a usage error before the slice runs (departure 3).
func firstN(paths []string, n int) []string {
	if n <= 0 {
		return nil
	}
	if n >= len(paths) {
		return paths
	}
	return paths[:n]
}

// GitLog is Git over the checkout at Root, answering each path from
// `git log -1 --format=%ct -- <path>`.
type GitLog struct {
	// Root is the directory the repo-relative paths resolve against.
	Root string
	// run is the exec seam: it runs git with args and returns its stdout. A
	// unit test replaces it, so no unit test starts git.
	run func(dir string, args ...string) ([]byte, error)
}

// LastCommit is the Unix time of the newest commit touching p. It is 0 with no
// error for an untracked path, and an error when git cannot answer.
func (g GitLog) LastCommit(p string) (int64, error) {
	out, err := g.runner()(g.Root, "log", "-1", "--format=%ct", "--", p)
	// A failed lookup is an error, not a 0: 0 is how an uncommitted file reads,
	// so a git that cannot answer would pass as a file no commit touched.
	if err != nil {
		return 0, fmt.Errorf("docslint: git log for %s: %w", p, err)
	}
	text := strings.TrimSpace(string(out))
	// git prints nothing for a path no commit touched.
	if text == "" {
		return 0, nil
	}
	ts, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		// The output stays out of the error: it is captured command output, which
		// can hold anything.
		return 0, fmt.Errorf("docslint: git log for %s gave no commit time", p)
	}
	return ts, nil
}

func (g GitLog) runner() func(dir string, args ...string) ([]byte, error) {
	if g.run != nil {
		return g.run
	}
	return func(dir string, args ...string) ([]byte, error) {
		return exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	}
}
