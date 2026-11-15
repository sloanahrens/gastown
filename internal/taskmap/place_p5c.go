package taskmap

// Placements for batch P5, part 3 (gt-ydx5es): gt polecat identity remove
// through gt polecat nuke. Identity remove, rename and show act on a polecat's
// permanent identity, list shows the polecats in a rig, and nuke destroys one
// finished polecat. All five sit in seats beside the identity and session
// commands from parts 1 and 2.
//
// Group judgement: gt polecat nuke destroys the session, worktree, branch and
// agent bead of a polecat. It is seats, not roll, because it cleans up after
// one finished polecat rather than undoing a landing, and it is seats, not
// host, because the branch and worktree belong to the polecat's seat.
func init() {
	Register(
		Entry{Path: "polecat identity remove", Group: "seats", Use: "Use this when a polecat's identity is no longer needed and its identity bead should be deleted, once its session is gone and nothing is left on its hook.", Check: "the run prints Removed identity bead: <id>"},
		Entry{Path: "polecat identity rename", Group: "seats", Use: "Use this when a polecat identity should carry a new name but keep the work history it built under the old one.", Check: "the run prints Renamed identity: with the old and new bead ids"},
		Entry{Path: "polecat identity show", Group: "seats", Use: "Use this to see one polecat identity's session count, completion record and recent work before you rename or remove it.", Check: "the run prints Identity: <rig>/<name> followed by its completion statistics"},
		Entry{Path: "polecat list", Group: "seats", Use: "Use this to see which polecats in a rig are working, done or stuck right now.", Check: "each polecat is listed as <rig>/<name> with its state"},
		Entry{Path: "polecat nuke", Group: "seats", Use: "Use this when a polecat's work is merged and its session, worktree, branch and agent bead should all be destroyed, after a --dry-run shows nothing blocks it.", Check: "the run ends with Nuked <n> polecat(s)"},
	)
}
