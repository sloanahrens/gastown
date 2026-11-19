package taskmap

// The entries for gt town state backup (gt-ya0nue): the check on the nightly
// backups the daemon writes, the chain whose archives the restore proof reads.
// Both take host, the group place_p7d.go records for the town-state family: the
// backups live on the machine the daemon runs on, beside the volume it mounts.
func init() {
	Register(
		Entry{Path: "town state backup", Group: "host", Use: "Use this when the nightly town-state backups the daemon writes have to be judged, to find the step that checks one night's archive."},
		Entry{Path: "town state backup verify", Group: "host", Use: "Use this when one night's town-state backup has to be judged before it counts as a night, to check its archive holds a town state and no credential.", Check: "the archive's file and byte counts print and the command exits 0; a refusal names the entry and the rule, never a value, and exits non-zero"},
	)
}
