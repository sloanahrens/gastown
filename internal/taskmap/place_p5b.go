package taskmap

// Placements for batch P5, part 2 (gt-fbm5x5): gt polecat git-state through
// gt polecat identity reconcile. A polecat's identity is the permanent half of
// its seat, so four of the five sit in seats beside part 1's polecat commands.
//
// Group judgement: gt polecat identity reconcile takes dolt, not seats. It
// merges one legacy agent bead row into its rig row, archives and deletes the
// town copy, so the thing it repairs is the beads store, not a seat.
func init() {
	Register(
		Entry{Path: "polecat git-state", Group: "seats", Use: "Use this when you are about to end or remove a polecat's seat, to see whether its worktree still holds work that would be lost.", Check: "the Verdict line reads CLEAN (safe to kill)"},
		Entry{Path: "polecat identity", Group: "seats", Use: "Use this to find the commands that create, list, rename or remove a polecat's permanent identity."},
		Entry{Path: "polecat identity add", Group: "seats", Use: "Use this when a rig needs a new polecat name that will outlive every session that works under it.", Check: "the run prints Created identity bead: <id>"},
		Entry{Path: "polecat identity list", Group: "seats", Use: "Use this to see which polecat identities a rig holds and what state each one is in.", Check: "each identity is listed with its name and state"},
		Entry{Path: "polecat identity reconcile", Group: "dolt", Use: "Use this when one agent bead exists as both a legacy town row and a rig row and the town copy must be merged away.", Check: "the run ends reconciled <id>: rig row updated, town row archived to <path> and deleted"},
	)
}
