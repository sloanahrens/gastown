package taskmap

// Placement for gt seat, the parent of the commands a polecat seat runs about
// itself (gt-7h8s5r), and for its ship subcommand.
func init() {
	Register(
		Entry{Path: "seat", Group: "seats", Use: "Use this to find the commands a polecat seat runs about itself, such as the one that posts its record when it ends."},
		Entry{Path: "seat ship", Group: "seats", Use: "Use this when a polecat seat has finished and must post its record as one comment on its bead, so its sandbox can exit at once.", Check: "the bead carries one comment starting SEAT-END v1"},
	)
}
