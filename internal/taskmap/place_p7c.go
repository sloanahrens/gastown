package taskmap

// Placements for batch P7, part 3 (gt-dxznv4): the gt reaper commands that
// are runnable by hand, and the gt town parent and its cluster subtree.
//
// Group judgement: the reaper commands read and edit the beads databases, so
// they take dolt, as the other reaper placements do in place_p7b.go. town and
// town cluster take host: no task group names the town root or the control
// cluster, and host is the group for the machine and infrastructure the town
// runs on.
func init() {
	Register(
		Entry{Path: "reaper reap", Group: "dolt", Use: "Use this when wisps past the max age are still open although their molecule has closed, to close them, after a --dry-run has shown the count.", Check: "the reaped count matches the Reap candidates line that reaper scan printed for the same database"},
		Entry{Path: "reaper run", Group: "dolt", Use: "Use this when the beads databases need every reaper step in one pass, to run the scan, reap, purge and auto-close cycle across all databases by hand."},
		Entry{Path: "reaper scan", Group: "dolt", Use: "Use this when you need to know how many wisps, issues and mail the reaper would act on before you run any step, without changing any data.", Check: "each database prints its Reap, Purge, Mail and Stale candidate counts"},
		Entry{Path: "town", Group: "host", Use: "Use this when you need to see how the town root splits between what the daemon owns and what the session keeps, before you move or verify any of it."},
		Entry{Path: "town cluster", Group: "host", Use: "Use this when the daemon's control-cluster manifests need rendering to files for the operator to apply, to find the step that writes them without applying anything."},
	)
}
