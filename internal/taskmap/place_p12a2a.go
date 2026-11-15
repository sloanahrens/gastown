package taskmap

// Placements for batch P12, half A2, part 1 (gt-j669b4.27): the five gt hooks
// paths this piece owns, install, list, override, reconcile and registry. All
// five sit in internal, the group for plumbing an agent does not run by hand:
// they read and edit the Claude Code settings files the town writes for its
// agents, which a person or the daemon runs, not a step an agent takes during
// a task. The sibling piece (gt-chip2n) places the other hooks paths the same
// way.
//
// Group judgement: gt hooks reconcile is a one-time cleanup of a host-hygiene
// hook, which could read as host; it stays in internal because it edits the
// same settings files as the other hooks commands and is not a host setting an
// agent changes.
func init() {
	Register(
		Entry{Path: "hooks install", Group: "internal", Use: "Use this when a hook from the registry should run in a worktree, or in every worktree of one role, and it is not there yet."},
		Entry{Path: "hooks list", Group: "internal", Use: "Use this when you need to know which settings.json files the town manages and whether each one matches its base and overrides."},
		Entry{Path: "hooks override", Group: "internal", Use: "Use this when one role or rig needs a hook that differs from the shared base, so the change goes in that role's or rig's override file."},
		Entry{Path: "hooks reconcile", Group: "internal", Use: "Use this when an old host-hygiene hook is still in an agent's settings file and a sync will not remove it.", Check: "gt hooks reconcile --dry-run prints no line ending (would remove interim hook), and its summary reads Would remove the interim hook from 0 of"},
		Entry{Path: "hooks registry", Group: "internal", Use: "Use this when you need to see which hooks can be installed for each role before you install one."},
	)
}
