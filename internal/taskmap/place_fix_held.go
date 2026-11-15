package taskmap

// The placement for gt fix-held, the read-only report of the fix-held rate over
// the bugs a window closed (gt-tiaczu).
func init() {
	Register(
		Entry{Path: "fix-held", Group: "review", Use: "Use this to see how many of the bugs a window closed held: the fix-held rate, the claims against them by relationship, and the fixes failed twice.", Check: "gt fix-held prints held N of M and the chronic list"},
	)
}
