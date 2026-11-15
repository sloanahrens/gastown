package taskmap

// Placements for batch P7, part 6 (gt-rc89ye): the rest of the town-state
// family — the split check, the migrate pair that proves a volume copy and
// prints the way back, the cluster seed and the rehearsal profile that runs on
// what the seed wrote.
//
// Group judgement: all five take host, the group the earlier town-state parts
// (place_p7d.go, place_p7e.go) record for this family. No task group names the
// town root, its volume cut-over or the claim a rehearsal pod mounts; host is
// the group for the machine and infrastructure the town runs on.
func init() {
	Register(
		Entry{Path: "town state verify", Group: "host", Use: "Use this when the town root has been copied, cut over or restored and you need to know every path still matches the daemon/session split manifest.", Check: "the command prints \"town state: ok\" and exits 0; a failure names each path no manifest entry covers or two entries that disagree on its owner"},
		Entry{Path: "town state migrate verify", Group: "host", Use: "Use this when a cut-over copy or a delta has streamed into the volume and you need proof every copied file and link there matches the host.", Check: "every copied file and link target is reported matching on both sides; a missing, changed, extra or non-regular entry is named by path and fails the command"},
		Entry{Path: "town state migrate rollback-plan", Group: "host", Use: "Use this when the volume copy has to be undone and you need the steps back in the order they run, from stopping the container to restoring the compose file.", Check: "the plan prints its steps numbered in order, naming the daemon container, the volume and the saved compose file it restores"},
		Entry{Path: "town state seed", Group: "host", Use: "Use this when a cluster town-state claim has to be filled from the live Mac town before a rehearsal or the live cut-over, through the seed helper Pod.", Check: "both sides are compared by sha256 and link target and the claim is listed afterwards; a rehearsal reports the seeded settings file as profiled"},
		Entry{Path: "town state profile", Group: "host", Use: "Use this when a seeded town in a rehearsal pod would otherwise start the live daemon's patrols, to turn every patrol off and drop the container supervisor.", Check: "`--check` prints \"rehearsal profile: ok\" and exits 0, and otherwise names each patrol or the supervisor still on"},
	)
}
