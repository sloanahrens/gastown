package taskmap

// Placements for batch P12, half A1 (gt-j669b4.16): the gt bird commands and
// gt birds, the five paths this piece owns. All five sit in birds, the group
// for running the town's autonomous bird agents, since a bird is a claude
// session recorded against the bead it runs.
//
// Group judgement: gt bird start is the launcher a shell or wrapper runs, not
// a step an agent takes, which would argue for internal; it stays in birds
// because the launcher's job is to run a bird. gt bird bind and gt bird list
// read as the session bookkeeping seats holds, but the sessions they name are
// the birds rather than polecat, crew or tmux seats.
func init() {
	Register(
		Entry{Path: "bird", Group: "birds", Use: "Use this to find the bird command that fits the job when you are working with a claude session that runs under a bird name."},
		Entry{Path: "bird bind", Group: "birds", Use: "Use this to record which bead this bird session runs, so the name and the bead can each be found from the other.", Check: "gt bird list shows the bead against the name"},
		Entry{Path: "bird list", Group: "birds", Use: "Use this to see which bird sessions are still running and which bead each one runs."},
		Entry{Path: "bird start", Group: "birds", Use: "Use this when you are starting a claude session for a bead and want it run under a bird name, so it can be found and resumed later."},
		Entry{Path: "birds", Group: "birds", Use: "Use this when you need to know which bird agents' daily reports are late."},
	)
}
