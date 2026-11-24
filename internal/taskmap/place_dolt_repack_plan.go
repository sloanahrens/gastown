package taskmap

// Placement for gt dolt repack-plan (gt-7isof4): the read that prints the
// bounded repack plan for the archive repository, beside the commands that
// inspect the Dolt server it backs up.
func init() {
	Register(
		Entry{Path: "dolt repack-plan", Group: "dolt", Use: "Use this when the archive repository's loose objects may need packing, to read the bounded repack command and whether it is due before running anything.", Check: "the plan prints due: yes or due: no with the count-objects reason"},
	)
}
