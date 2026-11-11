package session

import (
	"fmt"
	"strings"
)

// branchRelation is how a local branch head stands against the remote head of
// the same branch.
type branchRelation int

const (
	// relationEqual: both heads are the same commit, so there is nothing to push.
	relationEqual branchRelation = iota
	// relationAhead: the local head holds every remote commit and more.
	relationAhead
	// relationBehind: the remote head holds commits the local head lacks, and the
	// local head is an ancestor of it.
	relationBehind
	// relationDiverged: each head holds commits the other lacks.
	relationDiverged
)

// classifyBranchRelation says how local stands against remote. isAncestor(a, d)
// reports whether a is an ancestor of d, or the same commit. Its error is
// returned as it is, never read as a relation.
func classifyBranchRelation(local, remote string, isAncestor func(ancestor, descendant string) (bool, error)) (branchRelation, error) {
	if local == remote {
		return relationEqual, nil
	}
	localInRemote, err := isAncestor(local, remote)
	if err != nil {
		return 0, err
	}
	if localInRemote {
		return relationBehind, nil
	}
	remoteInLocal, err := isAncestor(remote, local)
	if err != nil {
		return 0, err
	}
	if remoteInLocal {
		return relationAhead, nil
	}
	return relationDiverged, nil
}

// divergence is what a diverged branch's refusal names: both heads, where they
// last agreed, and how many commits each side holds that the other lacks.
type divergence struct {
	Branch        string
	Local, Remote string
	MergeBase     string
	LocalOnly     int
	RemoteOnly    int
}

// divergedRefusal is the error for a push of a diverged branch. It names the
// branch, both heads, the merge-base and the counts, and carries git's refusal
// on one line. Nothing is forced, so origin keeps the head it holds.
func divergedRefusal(d divergence, refusal error) error {
	return fmt.Errorf("branch %s diverged from origin: local %s, origin %s, merge-base %s; local holds %d commits origin lacks, origin holds %d local lacks; nothing is forced: %s",
		d.Branch, d.Local, d.Remote, d.MergeBase, d.LocalOnly, d.RemoteOnly, refusalLine(refusal.Error()))
}

// refusalLine joins the non-empty lines of text with " | ", so a refusal git
// prints over several lines reads in one log line.
func refusalLine(text string) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, " | ")
}
