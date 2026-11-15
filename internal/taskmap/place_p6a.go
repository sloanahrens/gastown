package taskmap

// Placements for batch P6 piece A (gt-j669b4.10): committing and submitting
// work, wisp compaction, and the formula namespace. Group judgement: commit
// and done sit in land because they prepare and submit a branch for landing;
// compact and compact report sit in dolt because both work on the wisp records
// in the beads database, one promoting and deleting them and the other
// reporting what it found; formula sits in dispatch because a formula defines
// the work handed to agents.
func init() {
	Register(
		Entry{Path: "commit", Group: "land", Use: "Use this when your work is ready to commit and the commit must be authored as your seat rather than as the worktree's git user.", Check: "git log -1 names your seat and its <name>@gastown.local address"},
		Entry{Path: "compact", Group: "dolt", Use: "Use this when ephemeral wisps have outlived their type's time to live and the beads database should promote the stuck ones and delete the settled ones.", Check: "the run prints the promoted and deleted counts"},
		Entry{Path: "compact report", Group: "dolt", Use: "Use this to send the overseer the day's or week's account of what compaction deleted, promoted or left active.", Check: "the printed digest lists each category's deleted, promoted and active counts"},
		Entry{Path: "done", Group: "land", Use: "Use this when your work is committed and ready to land, to submit the branch for landing and end your session.", Check: "the READY TO LAND block on the work bead names the branch and its head"},
		Entry{Path: "formula", Group: "dispatch", Use: "Use this to find the formula command for the job: reading a workflow's steps, writing a new one, or running one."},
	)
}
