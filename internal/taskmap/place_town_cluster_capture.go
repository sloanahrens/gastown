package taskmap

// Placement for gt town cluster capture, which saves one live cluster object to
// a file.
func init() {
	Register(
		Entry{Path: "town cluster capture", Group: "host", Use: "Use this when one live cluster object is to be saved to a file; the command is not wired yet, so a call returns an error and writes nothing.", Check: "gt town cluster capture --help lists --namespace and --out"},
	)
}
