package taskmap

// Placements for batch P12, half A2, part 2 (gt-tr4vyk): the gt hooks scan and
// gt hooks sync commands, which read and regenerate the Claude Code settings
// files the town writes for its agents, and the gt init, gt install and gt
// plugin commands. The two hooks commands sit in internal beside their sibling
// hooks paths (gt-chip2n, gt-j669b4.27): a person or the daemon runs them, not
// an agent during a task.
//
// Group judgement: gt init and gt install set up a rig and the HQ on this
// machine, so they sit in host. gt plugin sits in dispatch beside its
// subcommands, since a plugin is periodic work the daemon or an operator hands
// out.
func init() {
	Register(
		Entry{Path: "hooks scan", Group: "internal", Use: "Use this when you need to see every hook the town's agents run, by hook type and by the agent whose settings file holds it, before you change one."},
		Entry{Path: "hooks sync", Group: "internal", Use: "Use this when a hook change in the shared base or an override has to reach the agents' settings files, so every agent starts its next session with it.", Check: "gt hooks diff reports no changes pending after gt hooks sync"},
		Entry{Path: "init", Group: "host", Use: "Use this when a git repository should become a Gas Town rig, so its polecats and crew directories exist and git stops tracking them.", Check: "polecats/ and crew/ are listed in .git/info/exclude and git status no longer shows them"},
		Entry{Path: "install", Group: "host", Use: "Use this when the machine has no Gas Town headquarters yet and you need the workspace where the mayor, the town config and the rigs will live.", Check: "the new HQ directory contains CLAUDE.md and mayor/"},
		Entry{Path: "plugin", Group: "dispatch", Use: "Use this when you need to find the periodic plugins the town and its rigs define, and the commands that list, run or record them by hand."},
	)
}
