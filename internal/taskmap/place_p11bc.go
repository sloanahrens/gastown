package taskmap

// Placements for batch P11, half B part 3 (gt-gbvejj): the commands that
// migrate old Dolt databases into the central data directory, mark the Dolt
// server deliberately paused, restart it and clear a hijacked port, open a SQL
// shell on it, and start it.
//
// Group judgement: all five sit in dolt. Each one acts on the Dolt server or
// the databases it serves, and the agent asking why bd reads the wrong
// database starts in dolt. pause and restart are the closest calls: restart
// stops a server and pause tells the daemon to leave one alone, which could
// read as shutdown, but shutdown covers agents, sessions and the town, and
// these two only ever touch the Dolt server beside them.
func init() {
	Register(
		Entry{Path: "dolt migrate", Group: "dolt", Use: "Use this when old Dolt databases still sit under a rig's .beads/dolt directory and need moving into the town's central data directory, previewed first with a dry run.", Check: "the run ends Migration complete, or prints No databases found to migrate"},
		Entry{Path: "dolt pause", Group: "dolt", Use: "Use this when the Dolt server is about to go down deliberately, for a full GC or a backup, and the daemon must leave it alone until the pause is lifted or lapses.", Check: "the run prints the pause message with its reason and the time it lapses"},
		Entry{Path: "dolt restart", Group: "dolt", Use: "Use this when the Dolt port is hijacked or serves the wrong data, so the server must be stopped, any stray server on the port cleared, and the correct one started again.", Check: "the run prints Dolt server restarted (PID N, port N)"},
		Entry{Path: "dolt sql", Group: "dolt", Use: "Use this when you need to query or change a rig's beads database by hand in a SQL shell instead of through bd."},
		Entry{Path: "dolt start", Group: "dolt", Use: "Use this when the town's Dolt server is not running and bd, the daemon or the agents need it up to serve the rig databases.", Check: "the run prints Dolt server started (PID N, port N)"},
	)
}
