package taskmap

// Placement for gt town cluster rollback, which undoes a plan's Deployments to
// their recorded revisions. It checks no approval itself: its caller does.
func init() {
	Register(
		Entry{Path: "town cluster rollback", Group: "host", Use: "Use this when an applied control-cluster plan has to be undone, to return each of its Deployments to the revision plan.json records; it checks no approval itself, so run it only for an approved rollback.", Check: "gt town cluster rollback --help lists --plan and --kubeconfig"},
	)
}
