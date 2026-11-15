package taskmap

// Placements for batch P7, part 2 (gt-975w2u): gt land wait and the gt reaper
// family. land wait prints the outcome of a landing of the caller's own crew
// branch, so it sits in land beside the commands that queue one.
//
// Group judgement: the four reaper commands read or edit the beads databases
// themselves, and no task group describes bead cleanup, so they take dolt, the
// group for inspecting or repairing the beads database — auto-close too, which
// closes bead issues rather than Dolt storage.
func init() {
	Register(
		Entry{Path: "land wait", Group: "land", Use: "Use this when a crew branch of yours has been pushed and you need the landing worker's verdict on it before you build on it.", Check: "the printed line names the commit the landing landed as, or the gate or review that refused it"},
		Entry{Path: "reaper", Group: "dolt", Use: "Use this when the beads store has wisps or issues to clean up, to find the step that scans, closes or purges them."},
		Entry{Path: "reaper auto-close", Group: "dolt", Use: "Use this when issues have stayed open with no updates past the stale age and should be closed, once a --dry-run preview has shown the set.", Check: "the printed closed count matches the set the dry run previewed"},
		Entry{Path: "reaper databases", Group: "dolt", Use: "Use this to list the databases the reaper can reach, before you name one with --db.", Check: "the printed list names each database"},
		Entry{Path: "reaper purge", Group: "dolt", Use: "Use this when closed wisps or closed mail past the purge age are taking space in a database and can be deleted for good.", Check: "the purge line reports the same counts the --dry-run preview showed"},
	)
}
