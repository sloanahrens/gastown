package taskmap

// Placements for batch P13: the operator-question pair, tmux session cycling,
// the town lifecycle commands (down, up, shutdown, estop, thaw, kill-all),
// disk maintenance, and the notify and Claude Code hook plumbing.
func init() {
	Register(
		Entry{Path: "answer", Group: "orient", Use: "Use this when an overseer question is waiting on an answer the operator gives, so the overseer can act on it.", Check: "gt questions shows the question as answered"},
		Entry{Path: "ask", Group: "orient", Use: "Use this when a decision is the operator's to make and the work waiting on it must not start until it is answered.", Check: "gt questions lists the new question"},
		Entry{Path: "cycle", Group: "seats", Use: "Use this when you are in a tmux session and want to see the commands that switch between the sessions of a group."},
		Entry{Path: "cycle next", Group: "seats", Use: "Use this when you are in a tmux session and want to move to the next session of the same rig."},
		Entry{Path: "cycle prev", Group: "seats", Use: "Use this to switch to the previous session in the same rig, as the C-b p keybinding does."},
		Entry{Path: "down", Group: "shutdown", Use: "Use this to pause the town: it stops the infrastructure and leaves the worktrees in place, so gt up can bring it back.", Check: "gt status shows the services stopped"},
		Entry{Path: "estop", Group: "shutdown", Use: "Use this when something is going wrong and everything must hold: no new dispatch and no restarts, while running sessions finish.", Check: "gt estop status shows the stop active"},
		Entry{Path: "estop status", Group: "shutdown", Use: "Use this to check whether an emergency stop is holding the town or one rig, before you act on work that will not start."},
		Entry{Path: "kill-all", Group: "shutdown", Use: "Use this when everything must stop now, including the running agent sessions an emergency stop leaves alive."},
		Entry{Path: "maint", Group: "host", Use: "Use this when the town's disks are filling and you need to see or run the maintenance that reclaims them."},
		Entry{Path: "maint apply", Group: "host", Use: "Use this to run the disk maintenance plan, after reading the plan it would apply."},
		Entry{Path: "maint plan", Group: "host", Use: "Use this to see what disk maintenance would delete or reclaim before anything is removed."},
		Entry{Path: "notify", Group: "internal", Use: "Use this when a message must reach the human outside the terminal through the notify command they configured."},
		Entry{Path: "notify test", Group: "internal", Use: "Use this when a notify command is configured and you need to know a message reaches the human.", Check: "the NOTIFY OK verdict line"},
		Entry{Path: "shutdown", Group: "shutdown", Use: "Use this when the town is done for the day and its polecat worktrees and branches should be cleaned up as it stops."},
		Entry{Path: "signal", Group: "internal", Use: "Use this when you need to see which handlers gt registers with Claude Code's hooks, or how to wire one."},
		Entry{Path: "signal stop", Group: "internal", Use: "Use this when you are wiring Claude Code's Stop hook, which runs it to hand a session the queued work or mail it stopped on."},
		Entry{Path: "thaw", Group: "shutdown", Use: "Use this to resume dispatch and restarts after an emergency stop, once it is safe for work to go on.", Check: "gt estop status shows no stop"},
		Entry{Path: "up", Group: "shutdown", Use: "Use this when Dolt, the daemon or other town services are not running and you need them back; --restore also starts crew and the polecats that hold work.", Check: "gt status shows Dolt and the daemon running"},
	)
}
