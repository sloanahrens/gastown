package taskmap

// Placements for batch P12, half A2 (gt-chip2n): gt birds due and the four
// paths of the gt hooks command this piece owns. All four hooks paths sit in
// internal: they edit and regenerate the Claude Code settings files the town
// writes for its agents, which is a step a person or the daemon runs, never one
// an agent takes during a task. gt birds due stays in birds beside the sibling
// piece's gt birds entry, since both report on the town's bird agents.
func init() {
	Register(
		Entry{Path: "birds due", Group: "birds", Use: "Use this when a bird's daily report is missing or stale and you need the late list with the request to send each reporter."},
		Entry{Path: "hooks", Group: "internal", Use: "Use this when the hook settings the town's agents run under look wrong and you need the subcommand that inspects them or brings them back in line."},
		Entry{Path: "hooks base", Group: "internal", Use: "Use this when a hook setting should apply to every agent at once, so it belongs in the shared base rather than in one agent's file.", Check: "gt hooks diff reports no changes pending after gt hooks sync"},
		Entry{Path: "hooks diff", Group: "internal", Use: "Use this when you need to know whether a pending hook setting change would alter any agent's settings file before it is synced.", Check: "the exit code is 0 when nothing is pending and 1 when something would change"},
		Entry{Path: "hooks init", Group: "internal", Use: "Use this when the town has no shared hook base yet and the agents' existing settings files should become the starting point for one.", Check: "gt hooks diff reports no changes pending"},
	)
}
