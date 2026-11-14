package taskmap

// The placement for `land rework-stats`, the weekly rework share and polecat
// minutes that lever 6 of the rework program is judged on.
func init() {
	Register(
		Entry{
			Path:  "land rework-stats",
			Group: "review",
			Use:   "Use this to see, week by week, the share of landed beads that needed two or more om review rounds and the mean polecat minutes per landed bead.",
		},
	)
}
