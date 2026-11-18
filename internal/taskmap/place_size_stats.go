package taskmap

// The placement for `land size-stats`, the added-lines buckets the landing
// record is read on: how the landed work divides by size, and the time and
// seats each size took.
func init() {
	Register(
		Entry{
			Path:  "land size-stats",
			Group: "review",
			Use:   "Use this when deciding whether large beads are worth splitting, to see how the landed work divides by added lines and how much more time and rework the larger buckets take.",
		},
	)
}
