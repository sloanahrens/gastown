package taskmap

// Placement for gt town cluster roll, the attended apply that carries a render
// change to the control cluster between image rolls.
func init() {
	Register(
		Entry{Path: "town cluster roll", Group: "host", Use: "Use this when a render change has to reach the control cluster between image rolls, to apply the daemon namespace under a landing hold; --dry-run prints the diff lines against the live cluster first.", Check: "gt town cluster roll --dry-run exits 0 and its diff lines name every field that differs"},
	)
}
