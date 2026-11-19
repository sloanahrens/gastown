package taskmap

// Placement for gt town runner and its roll subcommand, which change the gate
// runner under a landing hold without killing a gate that is running.
func init() {
	Register(
		Entry{Path: "town runner", Group: "host", Use: "Use this to change the gate runner's configuration, such as rolling it to a new template, rather than editing its manifests by hand."},
		Entry{Path: "town runner roll", Group: "host", Use: "Use this when the gate runner has to roll to a new template and the gate runs in flight must not be killed; --dry-run prints the steps and their bounds first."},
	)
}
