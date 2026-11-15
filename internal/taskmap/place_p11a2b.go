package taskmap

// Placements for batch P11, half A2 part 2 (gt-ucbpea): the crew command that
// makes a working tree for a topic, and the four Dolt commands that archive,
// clean and compare the beads database.
//
// Group judgement: gt crew worktree mints a working tree and a branch, which
// could argue land, but nothing it does reaches main and its caller is a crew
// seat, so it sits in seats beside the other crew rows. The Dolt commands sit
// in dolt, which is the group the agent asking what happened to the beads
// database starts in; cleanup is the closest call, since it deletes
// directories, but what it deletes are orphaned databases under .dolt-data/.
func init() {
	Register(
		Entry{Path: "crew worktree", Group: "seats", Use: "Use this when you need to work a topic without disturbing your crew clone, and want a worktree of your own on a branch cut from origin/main.", Check: "the path it prints exists and git -C <path> branch --show-current names crew/<agent>/<topic>"},
		Entry{Path: "dolt", Group: "dolt", Use: "Use this when you are not sure which dolt verb fits the job of inspecting or repairing the server the beads databases live on."},
		Entry{Path: "dolt archive-now", Group: "dolt", Use: "Use this when the beads history must reach the archive before a flatten, and you want the daemon's archive pass run now and each database's commits behind it reported.", Check: "each database prints 0 behind the archive after the pass and the command exits 0"},
		Entry{Path: "dolt cleanup", Group: "dolt", Use: "Use this when .dolt-data/ holds databases that no rig's metadata names, to remove them, after a --dry-run has shown which ones go.", Check: "the removal reports the same databases the dry run listed"},
		Entry{Path: "dolt compare", Group: "dolt", Use: "Use this when a restore of the beads servers must be proved, and every database on the restored copy has to match its source in row count and content hash.", Check: "every database prints as equal and the command exits 0"},
	)
}
