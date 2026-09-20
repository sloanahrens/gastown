package git

import (
	"fmt"
	"strconv"
	"strings"
)

// This file holds the low-level ("plumbing") readers used to compare what a
// commit ACTUALLY does to the tree against what a branch's own history claims
// it does. A branch's commit graph and its content can disagree: a `git reset
// --soft <remote-tracking-ref>` over a stale checkout moves HEAD to the remote
// tip while keeping the old index and working tree, so the next commit records
// (old tree) - (new tip) — a revert of everything merged since the checkout was
// cut — under a message describing unrelated work (gt-63sz). Nothing in
// git merge-base/ancestry can see that, because ancestry says the branch
// contains every one of those commits. Only the per-file blob content says
// otherwise, so these helpers expose exactly that: blob shas per tree, blob
// shas on both sides of each commit in a range, and the line-level diff of two
// blobs.

// nullBlob is git's all-zero object name, used in --raw output for the absent
// side of a creation (old) or deletion (new).
const nullBlob = "0000000000000000000000000000000000000000"

// CommitFileChange is one file changed by one commit, as reported by
// git log --raw. OldBlob is the blob the path had in the commit's first parent,
// NewBlob the blob it has in the commit; either is "" when that side does not
// have the path at all (creation and deletion respectively), so callers never
// have to know git's null-object spelling.
type CommitFileChange struct {
	Commit  string
	Path    string
	OldBlob string
	NewBlob string
}

// CommitFileChanges returns the per-file blob changes of up to limit commits
// reachable from rev, newest first, including rev itself.
//
// Merge commits contribute nothing: git log --raw prints no diff for a merge
// without -m/-c, and the changes a merge brings in are already reported by the
// commits it merges, which the same walk visits. That is the behavior this
// caller wants — a merge's own patch is not a unit anyone reverts.
//
// Paths are read with -z so files with spaces, quotes or newlines in their
// names come through verbatim instead of being C-quoted.
func (g *Git) CommitFileChanges(rev string, limit int) ([]CommitFileChange, error) {
	out, err := g.run("log", "--raw", "--no-abbrev", "--no-renames", "-z",
		"--format=C %H", rev, "-n", strconv.Itoa(limit))
	if err != nil {
		return nil, err
	}
	return parseRawLogChanges(out), nil
}

// parseRawLogChanges walks the NUL-separated stream produced by
// CommitFileChanges' git log invocation: a "C <sha>" header token, then
// alternating "<raw meta>" / "<path>" token pairs. A token is only ever read as
// a path from the position immediately after a meta token, so a path that
// happens to look like a header is not misread.
func parseRawLogChanges(out string) []CommitFileChange {
	var changes []CommitFileChange
	var commit string
	tokens := strings.Split(out, "\x00")
	for i := 0; i < len(tokens); i++ {
		// git log separates the commit header from its --raw block with a
		// blank line, and under -z that newline survives as a leading "\n" on
		// the first meta token of every commit — including the first one, since
		// the walk always has a header above it. Trim it before classifying the
		// token; the path is still read verbatim from its own token below.
		tok := strings.TrimLeft(tokens[i], "\n")
		switch {
		case strings.HasPrefix(tok, "C "):
			commit = strings.TrimSpace(tok[2:])
		case strings.HasPrefix(tok, ":"):
			if i+1 >= len(tokens) {
				break
			}
			path := tokens[i+1]
			i++
			fields := strings.Fields(tok)
			if len(fields) < 4 || commit == "" {
				continue
			}
			oldBlob, newBlob := fields[2], fields[3]
			if oldBlob == nullBlob {
				oldBlob = ""
			}
			if newBlob == nullBlob {
				newBlob = ""
			}
			if oldBlob == newBlob {
				continue // mode-only change: the content is untouched
			}
			changes = append(changes, CommitFileChange{
				Commit:  commit,
				Path:    path,
				OldBlob: oldBlob,
				NewBlob: newBlob,
			})
		}
	}
	return changes
}

// TreeFileBlobs returns path -> blob sha for every regular file in rev's tree,
// recursively. Submodule entries are skipped: they are gitlinks, not blobs, and
// carry no file content to compare.
func (g *Git) TreeFileBlobs(rev string) (map[string]string, error) {
	out, err := g.run("ls-tree", "-r", "-z", "--full-tree", rev)
	if err != nil {
		return nil, err
	}
	blobs := make(map[string]string)
	for _, entry := range strings.Split(out, "\x00") {
		if entry == "" {
			continue
		}
		meta, path, found := strings.Cut(entry, "\t")
		if !found {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 3 || fields[1] != "blob" {
			continue
		}
		blobs[path] = fields[2]
	}
	return blobs, nil
}

// BlobDiffLines compares two blobs of the same file and returns the multiset of
// lines the second side ADDS and the multiset of lines it REMOVES, keyed by
// line text. Because it diffs two bare blobs there is no path involved and no
// rename detection to confuse the result.
//
// Either blob may be "" ("that side does not have this file"). Such a
// comparison is reported as no lines at all rather than as "every line added"
// or "every line removed": callers use this to ask whether one change is
// contained in another, and a whole-file comparison cannot answer that
// question honestly.
func (g *Git) BlobDiffLines(oldBlob, newBlob string) (added, removed map[string]int, err error) {
	empty := map[string]int{}
	if oldBlob == "" || newBlob == "" {
		return empty, empty, nil
	}
	out, err := g.run("diff", "--unified=0", "--no-color", oldBlob, newBlob)
	if err != nil {
		return nil, nil, err
	}
	added, removed = map[string]int{}, map[string]int{}
	// Everything up to the first hunk header is diff preamble (diff --git,
	// index, the two blob-name lines). Skipping whole preamble blocks — rather
	// than testing each line against "---"/"+++" — keeps a removed line whose
	// own text starts with "--" from being mistaken for the file header.
	inBody := false
	for _, line := range strings.Split(out, "\n") {
		if !inBody {
			if strings.HasPrefix(line, "@@") {
				inBody = true
			}
			continue
		}
		if line == "" {
			continue
		}
		switch line[0] {
		case '+':
			added[line[1:]]++
		case '-':
			removed[line[1:]]++
		}
	}
	return added, removed, nil
}

// CommitSubject returns the subject line of a single commit.
func (g *Git) CommitSubject(rev string) (string, error) {
	return g.run("log", "-1", "--format=%s", rev)
}

// DiffStatThreeDot returns git diff --stat for base...head, the diff restricted
// to what head introduces on top of the merge base (as opposed to the pair-wise
// base..head range, which also reports base's own progress as a deletion).
func (g *Git) DiffStatThreeDot(base, head string) (string, error) {
	return g.run("diff", "--stat", base+"..."+head)
}

// TreesIdentical reports whether a and b name commits with byte-identical
// trees — the condition under which merging either into the other leaves the
// receiving branch's tree untouched.
func (g *Git) TreesIdentical(a, b string) (bool, error) {
	aTree, err := g.run("rev-parse", a+"^{tree}")
	if err != nil {
		return false, fmt.Errorf("resolve tree of %s: %w", a, err)
	}
	bTree, err := g.run("rev-parse", b+"^{tree}")
	if err != nil {
		return false, fmt.Errorf("resolve tree of %s: %w", b, err)
	}
	return strings.TrimSpace(aTree) == strings.TrimSpace(bTree), nil
}

// CommitLineStats is one commit's total added and removed line counts.
type CommitLineStats struct {
	Commit  string
	Subject string
	Added   int
	Removed int
}

// CommitLineStatsInRange returns per-commit line totals for up to limit commits
// in revRange (e.g. "main..branch"), newest first.
//
// Merge commits contribute nothing: git log --numstat prints no stats for a
// merge without -m/-c, and the changes a merge brings in are already reported
// by the commits it merges, which the same walk visits. Binary files also
// contribute nothing — git reports their counts as "-".
func (g *Git) CommitLineStatsInRange(revRange string, limit int) ([]CommitLineStats, error) {
	out, err := g.run("log", "--no-merges", "--numstat", "--format=%H%n%s",
		"-n", strconv.Itoa(limit), revRange)
	if err != nil {
		return nil, err
	}
	return parseNumstatLog(out), nil
}

// parseNumstatLog walks the line-oriented stream of CommitLineStatsInRange's
// git log invocation: a commit hash line, its subject line, then a blank line
// and the per-file "<added>\t<removed>\t<path>" lines. A commit with no changes
// emits no stats block at all, so there the next hash line follows the subject
// immediately.
func parseNumstatLog(out string) []CommitLineStats {
	var stats []CommitLineStats
	lines := strings.Split(out, "\n")
	for i := 0; i < len(lines); {
		commit := strings.TrimSpace(lines[i])
		if !isFullSHA(commit) {
			i++
			continue
		}
		entry := CommitLineStats{Commit: commit}
		i++
		if i < len(lines) {
			entry.Subject = strings.TrimSpace(lines[i])
			i++
		}
		for i < len(lines) {
			if strings.TrimSpace(lines[i]) == "" {
				i++
				continue
			}
			added, removed, ok := parseNumstatLine(lines[i])
			if !ok {
				break // the next commit's hash line ends this stats block
			}
			entry.Added += added
			entry.Removed += removed
			i++
		}
		stats = append(stats, entry)
	}
	return stats
}

// parseNumstatLine reads one "<added>\t<removed>\t<path>" numstat line. ok is
// false for a line that is not one, which is how the walk detects the end of a
// commit's stats block.
func parseNumstatLine(line string) (added, removed int, ok bool) {
	fields := strings.SplitN(line, "\t", 3)
	if len(fields) < 3 {
		return 0, 0, false
	}
	added, err := numstatCount(fields[0])
	if err != nil {
		return 0, 0, false
	}
	removed, err = numstatCount(fields[1])
	if err != nil {
		return 0, 0, false
	}
	return added, removed, true
}

// numstatCount parses one side of a numstat line. A binary file's counts are
// "-", which is zero lines rather than a malformed field.
func numstatCount(field string) (int, error) {
	if field == "-" {
		return 0, nil
	}
	return strconv.Atoi(field)
}

// isFullSHA reports whether s is a lowercase-or-uppercase 40-character hex
// object name.
func isFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
