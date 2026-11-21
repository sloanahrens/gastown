package storypoints

const (
	// SizeFlagMinutes is the estimate at or above which SizeFlag reports a bead
	// likely to land over SizeFlagLines added lines (gt-uyt12d). On the 691
	// landed beads with an estimate and an added-lines count (2026-11-13 to
	// 2026-11-19), flagging at 10 minutes or more caught 64 percent of the
	// beads that landed over 300 added lines, at 66 percent precision.
	SizeFlagMinutes = 10.0

	// SizeFlagLines is the landing size the SizeFlag prediction is about.
	SizeFlagLines = 300
)

// SizeFlag reports whether an estimate predicts a landing over SizeFlagLines
// added lines. A shape-only estimate never qualifies because the text features
// behind it predict size too weakly (gt-uyt12d), and a time that is not finite
// and positive is not an estimate at all.
func SizeFlag(minutes float64, rung string) bool {
	return rung != "shape" && validMinutes(minutes) && minutes >= SizeFlagMinutes
}
