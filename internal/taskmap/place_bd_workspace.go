package taskmap

// Placement for gt bd, the town's view of the bd gateway's workspace. The
// parent commands exist only to reach the push, the map's one verb: what the bd
// container serves is repaired by pushing the town to it.
func init() {
	Register(
		Entry{Path: "bd", Group: "dolt", Use: "Use this to manage what the bd gateway container reads: the workspace of routes, redirects and beads metadata it runs bd against."},
		Entry{Path: "bd workspace", Group: "dolt", Use: "Use this to manage the workspace tree the bd gateway owns, which the push replaces in one request."},
		Entry{Path: "bd workspace push", Group: "dolt", Use: "Use this when a route flip, a rig add or a metadata write must reach the bd container at once: it sends the town's workspace to the gateway and prints the manifest hash it stored.", Check: "gt bd workspace push prints a line starting workspace"},
	)
}
