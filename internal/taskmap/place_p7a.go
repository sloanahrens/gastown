package taskmap

// Placements for batch P7, part 1 (gt-j669b4.11): the gt land family.
// land om-stats and land om-lessons report om's rejections and findings and
// never move work onto main, so they sit in review beside land verdict and
// land rework-stats rather than in land.
func init() {
	Register(
		Entry{Path: "land", Group: "land", Use: "Use this when a landing is stuck in the rig's queue, whether refused, escalated or still waiting, to find the command that acts on it."},
		Entry{Path: "land om-lessons", Group: "review", Use: "Use this when the same kind of om finding keeps rejecting work, to see which rejection class it falls in and whether that class is growing."},
		Entry{Path: "land om-stats", Group: "review", Use: "Use this when you need to judge whether the landing gate or the work is at fault, to see how often om rejects landings, by day, rig and agent."},
		Entry{Path: "land requeue", Group: "land", Use: "Use this when the landing worker refused a bead for a reason outside the diff and the branch head is unchanged, to put the same head back in the queue.", Check: "the bead carries gt:ready-to-land and no rework label"},
		Entry{Path: "land takeover", Group: "land", Use: "Use this when a landing worker left an escalated bead for a person and the overseer has reviewed the branch head, to land that head.", Check: "the printed head matches the head the overseer reviewed"},
	)
}
