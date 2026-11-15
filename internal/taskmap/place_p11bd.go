package taskmap

// Placements for batch P11, half B part 4 (gt-ci7hdc): the commands that
// report whether the Dolt server is up, stop it, rank the statements keeping
// it busy, and lift a deliberate pause on it.
//
// Group judgement: all four sit in dolt, beside dolt start, restart and pause,
// which act on the same server. dolt stop is the closest call: stopping a
// server could read as shutdown, but it stops only the Dolt server, so it
// stays with the commands that bring that server up and down.
func init() {
	Register(
		Entry{Path: "dolt status", Group: "dolt", Use: "Use this to see whether the town's Dolt server is running, and on which PID and port, before you start, stop or restart it.", Check: "the first line says Dolt server is running (PID N)"},
		Entry{Path: "dolt stop", Group: "dolt", Use: "Use this when the town's Dolt server must go down, for example before a restore or to free its port, and you will bring it back up yourself.", Check: "the run prints Dolt server stopped (was PID N)"},
		Entry{Path: "dolt top", Group: "dolt", Use: "Use this when the Dolt server is busy or slow and you need to know which statement shape and which client host keep it busy before you cut that caller.", Check: "the rows rank by EST, or the run prints no statements sampled"},
		Entry{Path: "dolt unpause", Group: "dolt", Use: "Use this when a deliberate Dolt outage is over and the daemon must resume managing the server before the pause would lapse on its own.", Check: "the run prints Dolt pause lifted, or Dolt was not paused"},
	)
}
