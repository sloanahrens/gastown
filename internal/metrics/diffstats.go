package metrics

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var braceRename = regexp.MustCompile(`\{([^{}]*) => ([^{}]*)\}`)

// resolvePath is the path a numstat line names after a rename: git writes
// "src/{old => new}/f.go" or "old.txt => new.txt", and the new side is the file.
func resolvePath(p string) string {
	if strings.Contains(p, "{") {
		p = braceRename.ReplaceAllString(p, "$2")
		p = strings.ReplaceAll(p, "//", "/")
	} else if strings.Contains(p, " => ") {
		_, p, _ = strings.Cut(p, " => ")
	}
	return p
}

// NumstatEntry is one file's line of `git show --numstat`. A binary file, which
// git reports as "-", counts zero lines.
type NumstatEntry struct {
	Added   int
	Deleted int
	Path    string
}

// ParseNumstat parses `git show --numstat` output. A line that is not three
// tab-separated fields is skipped; a count that is neither "-" nor an integer
// is an error that stops the parse, not a skipped line.
func ParseNumstat(text string) ([]NumstatEntry, error) {
	var out []NumstatEntry
	for _, line := range pySplitLines(text) {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		added, err := numstatCount(parts[0])
		if err != nil {
			return nil, err
		}
		deleted, err := numstatCount(parts[1])
		if err != nil {
			return nil, err
		}
		out = append(out, NumstatEntry{Added: added, Deleted: deleted, Path: resolvePath(parts[2])})
	}
	return out, nil
}

func numstatCount(s string) (int, error) {
	if s == "-" {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("numstat count %q: not an integer", s)
	}
	return n, nil
}

// pySplitLines splits on the same line boundaries as str.splitlines.
// It drops empty lines, which ParseNumstat skips anyway.
func pySplitLines(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			return true
		}
		return false
	})
}

// pyDirname is os.path.dirname: everything before the last slash, with the
// slashes before it trimmed unless the head is only slashes.
func pyDirname(p string) string {
	head := p[:strings.LastIndex(p, "/")+1]
	if head != "" && strings.TrimRight(head, "/") != "" {
		head = strings.TrimRight(head, "/")
	}
	return head
}

// DiffStat is the size and spread of one landing's diff. The entropies are in
// bits, over lines changed per file and per directory.
type DiffStat struct {
	LinesAdded     int
	LinesDeleted   int
	FilesTouched   int
	FileEntropy    float64
	PackageEntropy float64
}

// DiffStats summarizes numstat entries. A file's weight is its lines changed,
// at least one, so a pure rename still counts; a package is the file's
// directory, "." at the root.
func DiffStats(entries []NumstatEntry) DiffStat {
	var s DiffStat
	files, pkgs := newWeights(), newWeights()
	for _, e := range entries {
		w := max(e.Added+e.Deleted, 1)
		files.add(e.Path, w)
		pkg := pyDirname(e.Path)
		if pkg == "" {
			pkg = "."
		}
		pkgs.add(pkg, w)
		s.LinesAdded += e.Added
		s.LinesDeleted += e.Deleted
	}
	s.FilesTouched = len(files.sum)
	s.FileEntropy = Entropy(files.values())
	s.PackageEntropy = Entropy(pkgs.values())
	return s
}

// weights sums a weight per key and lists them in first-seen order, so the
// entropy adds its terms in the same order on every run.
type weights struct {
	order []string
	sum   map[string]int
}

func newWeights() *weights { return &weights{sum: map[string]int{}} }

func (w *weights) add(key string, n int) {
	if _, ok := w.sum[key]; !ok {
		w.order = append(w.order, key)
	}
	w.sum[key] += n
}

func (w *weights) values() []float64 {
	out := make([]float64, len(w.order))
	for i, k := range w.order {
		out[i] = float64(w.sum[k])
	}
	return out
}

// commitHash is what a ledger commit value must look like before it reaches
// git. Its "$" also admits one trailing newline, so a value ending in one still
// matches.
var commitHash = regexp.MustCompile(`^[0-9a-f]{7,64}\n?$`)

// DiffKey names one landing in a DiffCache.
type DiffKey struct {
	Rig    string
	Commit string
}

// DiffCache holds the stats of every commit already read. It is shared by the
// caller, so a second source over the same cache reads each commit once; it is
// not safe for concurrent use.
type DiffCache map[DiffKey]DiffStat

// GitDiffSource reads a landing's diff stats from the rig's bare repo, through
// Run alone.
type GitDiffSource struct {
	// BareRepoFor returns the path of the rig's bare repo.
	BareRepoFor func(rig string) string
	// Run runs git and returns its stdout; an error means git failed.
	Run Runner
	// Cache is filled by every successful read; nil reads without caching.
	Cache DiffCache
}

// ErrDiffUnavailable is the error Stats wraps when it has no stats for a
// commit. The commit value is not a hash, or git could not read it.
var ErrDiffUnavailable = errors.New("diff stats unavailable")

// Stats returns the diff stats of commit in rig. A commit that is not a hash
// (it would otherwise be an option or a revision expression) or that git fails
// on is an error wrapping ErrDiffUnavailable, and nothing is cached; output
// ParseNumstat rejects is an error that does not.
func (g *GitDiffSource) Stats(ctx context.Context, rig, commit string) (DiffStat, error) {
	key := DiffKey{Rig: rig, Commit: commit}
	if s, ok := g.Cache[key]; ok {
		return s, nil
	}
	if !commitHash.MatchString(commit) {
		return DiffStat{}, fmt.Errorf("%w: %q is not a commit hash", ErrDiffUnavailable, commit)
	}
	out, err := g.Run(ctx, []string{"git", "--git-dir", g.BareRepoFor(rig), "show", "--numstat", "-M", "--format=", commit, "--"})
	if err != nil {
		return DiffStat{}, fmt.Errorf("%w: %w", ErrDiffUnavailable, err)
	}
	entries, err := ParseNumstat(string(out))
	if err != nil {
		return DiffStat{}, err
	}
	s := DiffStats(entries)
	if g.Cache != nil {
		g.Cache[key] = s
	}
	return s, nil
}
