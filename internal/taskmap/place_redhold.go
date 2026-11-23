package taskmap

// The placements for gt red-hold, which reads the red hold that parks automatic
// dispatch and asks the daemon to lift it (gt-p5kok4).
func init() {
	Register(
		Entry{Path: "red-hold", Group: "dispatch", Use: "Use this to inspect the red hold that parks automatic dispatch, and the beads that cure it."},
		Entry{Path: "red-hold status", Group: "dispatch", Use: "Use this to print why automatic dispatch is held, one line per reason.", Check: "gt red-hold status prints red hold: open, or one line per reason"},
		Entry{Path: "red-hold lift", Group: "dispatch", Use: "Use this after fixing what held automatic dispatch, to ask the daemon to lift the red hold; it lifts only if no trigger still holds.", Check: "gt red-hold lift --reason <what was fixed> prints lift requested, or nothing to lift and exits 1 when no hold is set"},
	)
}
