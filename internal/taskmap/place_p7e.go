package taskmap

// Placements for batch P7, part 5 (gt-r0rt3q): the town-state archive pair and
// the migrate family that moves the same set into a named Docker volume.
//
// Group judgement: no task group names the town root or its volume cut-over, so
// all five take host, the group for the machine and infrastructure the town runs
// on, as place_p7d.go records for town state and its day-one check. The copy is
// not dolt: Dolt data has its own backup path and never crosses this stream.
func init() {
	Register(
		Entry{Path: "town state export", Group: "host", Use: "Use this when the daemon-owned paths of a town root have to cross a process boundary, to stream them to standard output as a tar file, with --rollback for the bounded replay back onto the Mac.", Check: "the summary on standard error names the counts, bytes and classes left out, and the tar file imports into an empty root with `gt town state import`"},
		Entry{Path: "town state import", Group: "host", Use: "Use this when a town-state archive has to be restored into the seed's empty volume or replayed onto a populated tree, to read the untrusted tar stream and write only the daemon-owned paths.", Check: "the summary names the files written and left out, and with --merge-newer a regular file was replaced only where the archive's copy is newer"},
		Entry{Path: "town state migrate", Group: "host", Use: "Use this when the daemon-owned set is to move off the host into a named Docker volume, to find the step that plans the copy, streams it, compares its checksums or prints the rollback."},
		Entry{Path: "town state migrate plan", Group: "host", Use: "Use this when the cut-over copy has not run yet, to see what would be copied, the volume's free space and each cut-over prerequisite met or unmet by name.", Check: "the plan reports every cut-over prerequisite met and shows the set fitting the volume's free space"},
		Entry{Path: "town state migrate copy", Group: "host", Use: "Use this when the town is being cut over to its volume, to stream the set into an empty volume or the files that differ onto a seeded one, refusing a populated volume the seed record does not name.", Check: "`gt town state migrate verify --volume <name>` reports every copied file and link target matching on both sides, and with --cutover the cut-over time is written"},
	)
}
