package taskmap

// The placements for gt red-hold, which reads the red hold that parks automatic
// dispatch (gt-p5kok4).
func init() {
	Register(
		Entry{Path: "red-hold", Group: "dispatch", Use: "Use this to inspect the red hold that parks automatic dispatch, and the beads that cure it."},
		Entry{Path: "red-hold status", Group: "dispatch", Use: "Use this to print why automatic dispatch is held, one line per reason.", Check: "gt red-hold status prints red hold: open, or one line per reason"},
	)
}
