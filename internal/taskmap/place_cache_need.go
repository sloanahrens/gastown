package taskmap

// The placement for gt cache and gt cache need (gt-yknkq7), the report an agent
// brings to the operator before any cache cap changes.
func init() {
	Register(
		Entry{Path: "cache", Group: "host", Use: "Use this when the town's caches may be larger or smaller than the work needs, to see what each one holds and needs before changing a cap."},
		Entry{Path: "cache need", Group: "host", Use: "Use this to see, job by job, the runs counted, the window they cover, the largest young and total bytes, the current cap and the size that cache needs, in text or --json.", Check: "each cache job prints a row with need_bytes, or no data when no usable run exists"},
	)
}
