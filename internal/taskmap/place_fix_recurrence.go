package taskmap

// The placement for gt fix-recurrence, the read-only scan that prints the
// dependency edges a bead's prose claims to a closed fix (gt-1cf7l5).
func init() {
	Register(
		Entry{Path: "fix-recurrence", Group: "review", Use: "Use this to read the dependency edges a bead's prose claims to a closed bead it cites, as the fix-recurrence signal proposes them."},
		Entry{Path: "fix-recurrence scan", Group: "review", Use: "Use this when you want to see the edges a rig's bead prose proposes to closed fixes, before any edge is written.", Check: "gt fix-recurrence scan prints one line per proposal"},
	)
}
