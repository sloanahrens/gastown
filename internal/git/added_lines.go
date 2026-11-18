package git

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// AddedLinesSince returns the number of lines the working tree adds over base:
// the added column of `git diff --numstat --no-renames base` (a binary file's
// "-" counts 0) plus the line count of every untracked file, or the error of
// the git command that failed, never a partial count.
func (g *Git) AddedLinesSince(base string) (int, error) {
	// --end-of-options makes git refuse a base that begins with "-" instead of
	// reading it as an option: --output= would write a file, --ext-diff would
	// run a program. The call fails, so no count comes back.
	out, err := g.run("diff", "--numstat", "--no-renames", "--end-of-options", base)
	if err != nil {
		return 0, fmt.Errorf("git diff --numstat --no-renames --end-of-options %s: %w", base, err)
	}
	tracked, err := sumNumstatAdded(out)
	if err != nil {
		return 0, err
	}
	untracked, err := g.addedLinesInUntrackedFiles()
	if err != nil {
		return 0, err
	}
	return tracked + untracked, nil
}

// sumNumstatAdded adds the added column of numstat output. A line whose added
// field is "-" is a binary file, which git reports no line count for.
func sumNumstatAdded(out string) (int, error) {
	added, _, err := NumstatSize(out)
	return added, err
}

// NumstatSize reads `git diff --numstat --no-renames` output: added is the
// added column summed, files is one count per line. A binary file's "-" adds
// no lines and still counts as a file, and a path git quotes (a name with a
// space or a non-ASCII character) stays one tab-separated field, so it counts
// once. A line that is not three tab-separated fields, or whose added count is
// neither "-" nor an integer, is an error and no size, never a partial sum.
func NumstatSize(out string) (added, files int, err error) {
	for i, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return 0, 0, fmt.Errorf("parsing git diff --numstat line %d: %d tab-separated fields, want 3", i+1, len(fields))
		}
		if fields[0] != "-" {
			n, err := strconv.Atoi(fields[0])
			if err != nil {
				return 0, 0, fmt.Errorf("parsing added count of git diff --numstat line %d: %w", i+1, err)
			}
			added += n
		}
		files++
	}
	return added, files, nil
}

// addedLinesInUntrackedFiles counts the lines of the files git reports as
// untracked. A clone with no untracked file adds nothing, and git grep is not
// run: with no path it would search the worktree.
func (g *Git) addedLinesInUntrackedFiles() (int, error) {
	// Raw output, not run's trimmed: a path may begin or end with a space, and
	// TrimSpace would eat it. -z keeps a name that holds a space, a quote or a
	// newline from being quoted or split.
	out, err := g.runOutput("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return 0, fmt.Errorf("git ls-files --others --exclude-standard: %w", err)
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return 0, nil
	}

	total := 0
	for _, batch := range batchPaths(paths) {
		n, err := g.linesInUntrackedBatch(batch)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// grepBatchPaths and grepBatchBytes bound one git grep argv. A worktree can
// hold thousands of untracked files, and all their names on one argv can
// exceed the platform's limit (macOS caps a process at 256 KiB), which fails
// the call in place of counting the files.
const (
	grepBatchPaths = 256
	grepBatchBytes = 32 << 10
)

// batchPaths splits paths into runs of at most grepBatchPaths names and
// grepBatchBytes bytes. A name over the byte bound travels in a batch alone.
func batchPaths(paths []string) [][]string {
	var batches [][]string
	start, size := 0, 0
	for i, p := range paths {
		if i > start && (i-start == grepBatchPaths || size+len(p)+1 > grepBatchBytes) {
			batches = append(batches, paths[start:i])
			start, size = i, 0
		}
		size += len(p) + 1 // the separator the argv puts after it
	}
	if start < len(paths) {
		batches = append(batches, paths[start:])
	}
	return batches
}

// linesInUntrackedBatch counts the lines of one batch of untracked paths.
func (g *Git) linesInUntrackedBatch(paths []string) (int, error) {
	// -z separates a path from its count with NUL and prints the path verbatim,
	// so a name holding a space, a colon or a newline still parses; the empty
	// pattern matches every line. --literal-pathspecs makes git read each path
	// as itself: as pathspecs, a name holding * ? or [ would instead glob and
	// count other files.
	args := append([]string{"--literal-pathspecs", "grep", "--no-index", "-c", "-z", "", "--"}, paths...)
	out, err := g.runOutput(args...)
	if err != nil {
		if isGrepNoMatch(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("git grep --no-index -c for untracked files: %w", err)
	}
	return sumGrepCounts(out)
}

// isGrepNoMatch reports whether err is git grep's "no line matched" status:
// exit 1 with nothing on either stream, which is what a batch of empty files
// produces. Exit 1 carrying a message, or an exit 1 that is not a git exit, is
// a failure of the command and must not be read as a zero count.
func isGrepNoMatch(err error) bool {
	if exitCode(err) != 1 {
		return false
	}
	var gitErr *GitError
	if !errors.As(err, &gitErr) {
		return false
	}
	return gitErr.Stdout == "" && gitErr.Stderr == ""
}

// sumGrepCounts adds the counts in `git grep -c -z` output: one
// "path<NUL>count" record per matching file, each ending in a newline. A path
// is cut at the NUL, since a name cannot hold one, but it may hold a newline,
// so the count is read from after the NUL to the newline that ends the record.
// A file git grep leaves out (no lines) adds nothing.
func sumGrepCounts(out string) (int, error) {
	total := 0
	record := 0
	for out != "" {
		record++
		path, rest, found := strings.Cut(out, "\x00")
		if !found || path == "" {
			return 0, fmt.Errorf("parsing git grep --count record %d: no path and count separated by NUL", record)
		}
		count, rest, _ := strings.Cut(rest, "\n")
		out = rest
		n, err := strconv.Atoi(count)
		if err != nil {
			return 0, fmt.Errorf("parsing count of git grep --count record %d: %w", record, err)
		}
		total += n
	}
	return total, nil
}
