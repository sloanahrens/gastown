package taskmap

// Placement for gt town runner and its roll subcommand, which change the gate
// runner under a landing hold without killing a gate that is running.
func init() {
	Register(
		Entry{Path: "town runner", Group: "host", Use: "Use this to change the gate runner's configuration, such as rolling it to a new template, rather than editing its manifests by hand."},
		Entry{Path: "town runner roll", Group: "host", Use: "Use this when the gate runner's template has changed and must roll out, to hold landings, wait for the landing pass and the gate runs in flight to finish, then apply and release the hold; --dry-run prints the steps and their bounds, and a real roll refuses before taking the hold until its apply step is wired."},
	)
}
