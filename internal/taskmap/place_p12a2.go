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
		Entry{Path: "hooks", Group: "internal", Use: "Use this to find the step that edits, checks or regenerates the Claude Code hook settings the town writes for its agents."},
		Entry{Path: "hooks base", Group: "internal", Use: "Use this when the hook settings every agent shares must change, by editing the base config every target is merged from.", Check: "gt hooks diff reports no changes pending after gt hooks sync"},
		Entry{Path: "hooks diff", Group: "internal", Use: "Use this before syncing hook settings, to see which agents' settings files would change and how.", Check: "the exit code is 0 when nothing is pending and 1 when something would change"},
		Entry{Path: "hooks init", Group: "internal", Use: "Use this when no shared hook base exists yet and one must be derived from the settings files the agents already have.", Check: "gt hooks diff reports no changes pending"},
	)
}
