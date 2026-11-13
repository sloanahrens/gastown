package taskmap

// The entry for gt sandbox set-resources (gt-mb54kg), which came after the P
// batches were cut and so has no line in testdata/unplaced.
func init() {
	Register(
		Entry{Path: "sandbox set-resources", Group: "sandboxes", Use: "Use this to change the CPU and memory requests or limits of a sandbox profile, checked and backed up, instead of editing the settings file by hand.", Check: "gt sandbox set-resources <profile> --requests cpu=... --dry-run prints the before and after"},
	)
}
