package git

import (
	"fmt"
	"strings"
)

// RewriteCommitMessages rewrites the message of every commit in base..HEAD
// through fn and moves HEAD to the rewritten tip. It returns how many commits
// got a new message. Trees, authors, committer identities and dates are kept,
// so the rewrite changes commit ids and nothing else: the working tree and
// index are exactly as they were.
//
// fn receives each commit id and its message with surrounding whitespace
// trimmed, and returns the replacement; returning the message unchanged leaves
// that commit alone. An error from fn aborts the rewrite before HEAD moves. A
// commit whose parents were rewritten is recreated even when its own message is
// unchanged, so the rewritten range stays connected. Merge commits are
// rewritten with their parents remapped.
//
// The branch is not pushed. A caller that pushes afterwards must do so under a
// lease, as the old tip was already public if the branch was pushed before.
func (g *Git) RewriteCommitMessages(base string, fn func(commit, message string) (string, error)) (int, error) {
	oldHead, err := g.Rev("HEAD")
	if err != nil {
		return 0, fmt.Errorf("resolve HEAD: %w", err)
	}
	out, err := g.run("rev-list", "--reverse", "--topo-order", "--parents", base+"..HEAD")
	if err != nil {
		return 0, fmt.Errorf("list %s..HEAD: %w", base, err)
	}
	if out == "" {
		return 0, nil
	}

	// Commits outside base..HEAD keep their ids, so a parent missing from
	// this map maps to itself.
	mapped := map[string]string{}
	rewritten := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		commit, parents := fields[0], fields[1:]

		newParents := make([]string, len(parents))
		parentsChanged := false
		for i, p := range parents {
			newParents[i] = p
			if m, ok := mapped[p]; ok && m != p {
				newParents[i] = m
				parentsChanged = true
			}
		}

		message, err := g.run("log", "-1", "--format=%B", commit)
		if err != nil {
			return 0, fmt.Errorf("read message of %s: %w", commit, err)
		}
		newMessage, err := fn(commit, message)
		if err != nil {
			return 0, err
		}
		if newMessage == message && !parentsChanged {
			mapped[commit] = commit
			continue
		}

		identity, err := g.runOutput("log", "-1", "--format=%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI", commit)
		if err != nil {
			return 0, fmt.Errorf("read identity of %s: %w", commit, err)
		}
		id := strings.Split(strings.TrimRight(identity, "\n"), "\x00")
		if len(id) != 6 {
			return 0, fmt.Errorf("read identity of %s: unexpected format %q", commit, identity)
		}
		args := []string{"commit-tree", commit + "^{tree}"}
		for _, p := range newParents {
			args = append(args, "-p", p)
		}
		args = append(args, "-m", newMessage)
		newCommit, err := g.runWithEnv(args, []string{
			"GIT_AUTHOR_NAME=" + id[0], "GIT_AUTHOR_EMAIL=" + id[1], "GIT_AUTHOR_DATE=" + id[2],
			"GIT_COMMITTER_NAME=" + id[3], "GIT_COMMITTER_EMAIL=" + id[4], "GIT_COMMITTER_DATE=" + id[5],
		})
		if err != nil {
			return 0, fmt.Errorf("recreate %s: %w", commit, err)
		}
		mapped[commit] = newCommit
		if newMessage != message {
			rewritten++
		}
	}

	newHead := mapped[oldHead]
	if newHead == "" || newHead == oldHead {
		return rewritten, nil
	}
	// The trees match, so moving HEAD leaves the index and working tree valid.
	// The old-value argument makes the move fail if HEAD moved underneath us.
	if _, err := g.run("update-ref", "-m", "rewrite commit messages", "HEAD", newHead, oldHead); err != nil {
		return 0, fmt.Errorf("move HEAD to rewritten tip: %w", err)
	}
	return rewritten, nil
}
