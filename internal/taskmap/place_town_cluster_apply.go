package taskmap

// Placement for gt town cluster apply, which applies a plan under the approval
// record that authorizes it. The command is not wired yet: a call with its
// flags returns an error and applies nothing.
func init() {
	Register(
		Entry{Path: "town cluster apply", Group: "host", Use: "Use this when a planned control-cluster change is approved and has to be applied under its approval record; the command is not wired yet, so a call returns an error and applies nothing.", Check: "gt town cluster apply --help lists --plan, --approval, --policy-kubeconfig and --apply-kubeconfig"},
	)
}
