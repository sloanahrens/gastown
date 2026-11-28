package land

// gateAheadBuild builds the candidate a train's next car is gated ahead on
// (train 3b-3): the read-only checks (gateAheadCheck), the merge onto base in
// slot 1 (gateAheadMerge), then the stored approval of that merge
// (gateAheadApproval). It returns the merged commit and the cleanup that
// removes the worktree the merge made.
//
// A failure at any step returns that step's reason with no candidate: the
// check runs before any worktree exists, the merge removes the one it made,
// and a refused approval removes it through cleanup before returning. dir,
// merged and cleanup are empty on failure. It writes nothing to the bead, so a
// refusal is left for the landing to record, or not.
func (l *Lander) gateAheadBuild(g Repo, w Work, base string) (dir, merged string, cleanup func(), err error) {
	if err := l.gateAheadCheck(g, w, base); err != nil {
		return "", "", nil, err
	}
	dir, merged, cleanup, err = l.gateAheadMerge(g, w, base)
	if err != nil {
		return "", "", nil, err
	}
	if err := l.gateAheadApproval(g, w, base, dir, merged); err != nil {
		cleanup()
		return "", "", nil, err
	}
	return dir, merged, cleanup, nil
}
