package taskmap

// Placements for batch P12, half A1 (gt-j669b4.16): the gt bird commands and
// gt birds, the five paths this piece owns.
//
// Group judgement: gt bird bind and gt bird list read as the session
// bookkeeping seats holds, but the sessions they name are the birds rather than
// polecat, crew or tmux seats, so they sit in birds. gt bird start execs claude
// in place of this process, so a wrapper runs it and an agent never runs it by
// hand: the internal rule puts it in internal.
func init() {
	Register(
		Entry{Path: "bird", Group: "birds", Use: "Use this to run claude sessions under bird names, so each session can be found and resumed later."},
		Entry{Path: "bird bind", Group: "birds", Use: "Use this to record which bead this bird session runs, so the name and the bead can each be found from the other.", Check: "gt bird list shows the bead against the name"},
		Entry{Path: "bird list", Group: "birds", Use: "Use this to see which bird sessions are still running and which bead each one runs."},
		Entry{Path: "bird start", Group: "internal", Use: "Use this when you are starting a claude session for a bead and want it run under a bird name, so it can be found and resumed later."},
		Entry{Path: "birds", Group: "birds", Use: "Use this when you need to check whether the birds have published their daily reports."},
	)
}
