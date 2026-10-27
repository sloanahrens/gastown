package presubmit

import (
	"fmt"
	"path"
	"strings"
)

// rationaleMarker is what a commit message says to explain a go.mod or go.sum
// change: a line beginning "Rationale:" with text after the colon.
const rationaleMarker = "rationale:"

// logFormat is the per-commit record ReadCommits asks git log for: sha,
// parents and raw message, NUL-separated, which git log -z also terminates the
// record with. A commit message cannot hold a NUL, so the three fields never
// run together.
const logFormat = "%H%x00%P%x00%B"

// Runner runs one git command in the repository and returns its standard
// output; args are git's arguments without the program name. Tests pass a
// runner that answers from canned output, so the unit tier runs no process.
type Runner func(args []string) ([]byte, error)

// Commit is one commit the rationale check judges.
type Commit struct {
	SHA     string
	Parents []string
	Message string
	Changed []string // repo-relative paths the commit changes, filled by ReadCommits
}

// RationaleFinding is a commit that changes a go.mod or go.sum file and gives
// no reason for it.
type RationaleFinding struct {
	SHA     string
	Subject string
	Files   []string // the module files the commit changes
}

// RationaleResult is what the check judged on one branch.
type RationaleResult struct {
	// Judged is how many commits change a go.mod or go.sum file.
	Judged int
	// Missing are the judged commits whose message carries no rationale line.
	Missing []RationaleFinding
}

// MissingRationales reports the commits that change a go.mod or go.sum file
// without a rationale line after the subject of their message. A commit that
// changes no module file is not judged, and a module file in a nested module
// counts like the root one.
func MissingRationales(commits []Commit) RationaleResult {
	var res RationaleResult
	for _, c := range commits {
		files := moduleFiles(c.Changed)
		if len(files) == 0 {
			continue
		}
		res.Judged++
		if hasRationale(c.Message) {
			continue
		}
		res.Missing = append(res.Missing, RationaleFinding{SHA: c.SHA, Subject: subject(c.Message), Files: files})
	}
	return res
}

// ReadCommits reads the commits of base..head, each with the paths its own diff
// changes.
//
// A merge commit is judged by what it changes against every parent: one that
// only carries a parent's change introduces nothing, so the commit that made
// the change is the one asked for a rationale, and a resolution that leaves a
// module file different from every parent is the merge's own change.
func ReadCommits(run Runner, base, head string) ([]Commit, error) {
	revs := base + ".." + head
	out, err := run([]string{"log", "-z", "--format=" + logFormat, "--end-of-options", revs})
	if err != nil {
		return nil, fmt.Errorf("git log %s: %w", revs, err)
	}
	commits, err := parseLog(out)
	if err != nil {
		return nil, err
	}
	for i := range commits {
		changed, err := changedPaths(run, commits[i])
		if err != nil {
			return nil, err
		}
		commits[i].Changed = changed
	}
	return commits, nil
}

// changedPaths lists the paths one commit changes, read against each parent.
func changedPaths(run Runner, c Commit) ([]string, error) {
	if len(c.Parents) == 0 {
		out, err := run(diffTreeArgs(c.SHA))
		if err != nil {
			return nil, fmt.Errorf("git diff-tree of root commit %s: %w", c.SHA, err)
		}
		return splitPaths(out), nil
	}
	var changed []string
	for i, parent := range c.Parents {
		out, err := run(diffTreeArgs(parent, c.SHA))
		if err != nil {
			return nil, fmt.Errorf("git diff-tree of %s against parent %s: %w", c.SHA, parent, err)
		}
		paths := splitPaths(out)
		if i == 0 {
			changed = paths
			continue
		}
		changed = commonPaths(changed, paths)
	}
	return changed, nil
}

// diffTreeArgs is the diff-tree command that lists the paths between two trees;
// with one revision it lists a root commit's own paths. -z writes each path as
// it is, never octal-quoted, so a module file under a non-ASCII directory keeps
// its base name (gt-b9j3o.6). --end-of-options keeps a ref that begins with a
// dash an operand rather than an option (gt-b9j3o.6).
func diffTreeArgs(revs ...string) []string {
	args := []string{"diff-tree", "-r", "--no-commit-id", "--name-only", "-z"}
	if len(revs) == 1 {
		args = append(args, "--root")
	}
	return append(append(args, "--end-of-options"), revs...)
}

// parseLog reads git log -z --format=%H%x00%P%x00%B output. Output that does
// not divide into sha, parents and message groups is refused rather than read
// as fewer commits, so a changed format fails the check instead of judging
// nothing.
func parseLog(out []byte) ([]Commit, error) {
	fields := dropTrailingEmpty(strings.Split(string(out), "\x00"))
	if len(fields)%3 != 0 {
		return nil, fmt.Errorf("git log output holds %d NUL-separated fields, not sha/parents/message groups", len(fields))
	}
	commits := make([]Commit, 0, len(fields)/3)
	for i := 0; i < len(fields); i += 3 {
		if fields[i] == "" {
			return nil, fmt.Errorf("git log output holds a record with no sha")
		}
		commits = append(commits, Commit{SHA: fields[i], Parents: splitParents(fields[i+1]), Message: fields[i+2]})
	}
	return commits, nil
}

// splitPaths reads a git -z path list.
func splitPaths(out []byte) []string {
	return dropTrailingEmpty(strings.Split(string(out), "\x00"))
}

// dropTrailingEmpty removes the empty last element that splitting
// NUL-terminated output leaves.
func dropTrailingEmpty(fields []string) []string {
	if n := len(fields); n > 0 && fields[n-1] == "" {
		return fields[:n-1]
	}
	return fields
}

// splitParents reads git's space-separated parent list; a root commit has none.
func splitParents(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, " ")
}

// commonPaths returns the paths of a that also appear in b, in a's order.
func commonPaths(a, b []string) []string {
	inB := make(map[string]bool, len(b))
	for _, p := range b {
		inB[p] = true
	}
	var out []string
	for _, p := range a {
		if inB[p] {
			out = append(out, p)
		}
	}
	return out
}

// moduleFiles returns the go.mod and go.sum files among paths, once each.
func moduleFiles(paths []string) []string {
	var out []string
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		base := path.Base(p)
		if (base != "go.mod" && base != "go.sum") || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// hasRationale reports whether a message carries a rationale line: a line
// after the subject beginning "Rationale:" in any case, with text after the
// colon. A title that mentions a rationale is not one, so the reason sits in
// the body where a log search finds it.
func hasRationale(message string) bool {
	for i, line := range strings.Split(message, "\n") {
		if i == 0 {
			continue
		}
		rest, ok := cutPrefixFold(strings.TrimSpace(line), rationaleMarker)
		if ok && strings.TrimSpace(rest) != "" {
			return true
		}
	}
	return false
}

// cutPrefixFold cuts prefix from s, ignoring ASCII case, and reports whether it
// cut anything.
func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}

// subject returns a message's first line, which is git's subject.
func subject(message string) string {
	line, _, _ := strings.Cut(message, "\n")
	return strings.TrimSpace(line)
}
