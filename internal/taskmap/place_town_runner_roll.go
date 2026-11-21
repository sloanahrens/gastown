package taskmap

// Placement for gt town runner and its roll and resize subcommands, which
// change the gate runner without editing its manifests by hand.
func init() {
	Register(
		Entry{Path: "town runner", Group: "host", Use: "Use this to change the gate runner's configuration, such as rolling it to a new template, rather than editing its manifests by hand."},
		Entry{Path: "town runner roll", Group: "host", Use: "Use this when the gate runner has to roll to a new template and the gate runs in flight must not be killed; --dry-run prints the steps and their bounds first, then the drift lines against the live cluster."},
		Entry{Path: "town runner resize", Group: "host", Use: "Use this to change the gate runner's engine CPU in place, without a restart, then see whether the rendered template asks for the same value or reverts the pod on its next restart."},
	)
}
