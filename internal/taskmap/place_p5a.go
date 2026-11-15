package taskmap

// Placements for batch P5, part 1 (gt-j669b4.9): gt polecat through
// gt polecat gc. A polecat's identity outlives its session, so all five sit in
// seats beside the identity and session commands that manage one.
//
// Group judgement: gt polecat is a runnable parent whose children are almost
// all identity or session management, so it takes seats rather than internal.
// gt polecat gc sweeps polecat branches out of the rig's git store, and the
// branches belong to polecat seats, so it is seats and not host, which means
// the machine the town runs on.
func init() {
	Register(
		Entry{Path: "polecat", Group: "seats", Use: "Use this to find the commands that manage a polecat's permanent identity and its ephemeral session."},
		Entry{Path: "polecat add", Group: "seats", Use: "Use this when an old script still adds a polecat by hand: it is deprecated and gt polecat identity add replaces it."},
		Entry{Path: "polecat check-recovery", Group: "seats", Use: "Use this when you must decide whether a polecat can be removed or its work still needs recovery.", Check: "gt polecat check-recovery <rig>/<name> prints a verdict such as SAFE_TO_NUKE or NEEDS_RECOVERY"},
		Entry{Path: "polecat check-recovery-batch", Group: "seats", Use: "Use this to ask the same safe-to-remove question for every polecat in a rig in one pass.", Check: "each polecat gets a SAFE_TO_NUKE or NEEDS_RECOVERY verdict"},
		Entry{Path: "polecat gc", Group: "seats", Use: "Use this when stale timestamped polecat branches have piled up and only each polecat's current branch must stay.", Check: "gt polecat gc <rig> --dry-run lists the branches it would delete"},
	)
}
