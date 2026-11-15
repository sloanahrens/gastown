package taskmap

// Placements for batch P10, half B, part 2 (gt-nwnfyq): the five tap guard
// commands that refuse a tool call before it runs. They go in internal, the
// same as the other tap handlers (place_p10ba.go), because a PreToolUse hook
// calls them and an agent never runs them by hand.
//
// Group judgement: none. Every tap guard is a hook handler, so all five sit
// in internal even where the policy they enforce is about a task (bd create,
// container suites) rather than plumbing.
func init() {
	Register(
		Entry{Path: "tap guard bd-create-store", Group: "internal", Use: "Use this when a bd create must be refused because a polecat would file the bead into the town's hq store, where the rig's dispatcher never sees it."},
		Entry{Path: "tap guard bd-init", Group: "internal", Use: "Use this when bd init must be refused because it runs outside the HQ root, where it would create a second beads database that conflicts with the town's."},
		Entry{Path: "tap guard container-suite", Group: "internal", Use: "Use this when a bare go test or make test on a container-backed package must be refused, so the suite runs through gt slot run and does not collide with other suites on the shared Docker VM."},
		Entry{Path: "tap guard dangerous-command", Group: "internal", Use: "Use this when a shell command must be refused before it runs because it could do irreversible damage, such as rm -rf, a force push, a hard reset or a system package install."},
		Entry{Path: "tap guard mol-patrol", Group: "internal", Use: "Use this when gt mol patrol must be refused inside an agent session, since it would terminate stale molecules that may include the caller's own or a sibling agent's work."},
	)
}
