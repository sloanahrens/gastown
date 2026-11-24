package taskmap

// Placement for gt seat, the parent of the commands a polecat seat runs about
// itself (gt-7h8s5r). Its ship subcommand is placed by the bead that adds it.
func init() {
	Register(
		Entry{Path: "seat", Group: "seats", Use: "Use this to find the commands a polecat seat runs about itself, such as the one that posts its record when it ends."},
	)
}
