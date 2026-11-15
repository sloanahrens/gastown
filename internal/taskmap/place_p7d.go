package taskmap

// Placements for batch P7, part 4 (gt-f04rl3): the rest of the gt town family,
// the control cluster's render, the loopback forward and its launchd job, and
// the town-state split and its day-one checks.
//
// Group judgement: no task group names the town root or the control cluster, so
// all five take host, the group for the machine and infrastructure the town runs
// on, as place_p7c.go records for town and town cluster. town state is not dolt:
// it moves the daemon-owned paths of the town root, not the beads database.
func init() {
	Register(
		Entry{Path: "town cluster render", Group: "host", Use: "Use this when the control cluster is being redeployed and its manifests must be regenerated from the current image values, to write one numbered file per object in apply order and print the kubectl line each mounted Secret needs.", Check: "the files under --out are numbered in apply order, every mounted Secret has its kubectl line or the bd gateway note, and no Secret document was written"},
		Entry{Path: "town forward", Group: "host", Use: "Use this when a control-cluster Service must be reachable from the Mac itself on a loopback port, to find the step that prints the launchd job forwarding it."},
		Entry{Path: "town forward plist", Group: "host", Use: "Use this when the loopback forward to a control-cluster Service has to survive a logout and restart itself after it exits, to print the launchd job that runs it for the operator to load.", Check: "once the printed job is loaded, 127.0.0.1 at the --port answers from the Mac"},
		Entry{Path: "town state", Group: "host", Use: "Use this when the town root is being cut over to its own volume or brought back from one, to find the step that inspects the split, verifies a root, exports or imports the daemon-owned paths, or plans and rolls back the copy."},
		Entry{Path: "town state check", Group: "host", Use: "Use this when the town root is about to move to its volume or has just moved, to record the git speed baseline before the move or run the day-one checks after it and see which one failed.", Check: "every row of the printed table passes and the command exits 0"},
	)
}
