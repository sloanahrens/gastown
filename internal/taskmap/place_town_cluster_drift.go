package taskmap

// Placement for gt town cluster drift, which reports how the live control
// cluster's Deployments differ from what the recorded render flags produce.
func init() {
	Register(
		Entry{Path: "town cluster drift", Group: "host", Use: "Use this when the live daemon and dashboard must be checked against the render after a deploy, to print every field that differs for a reason other than an image roll and exit 1 when one does.", Check: "gt town cluster drift exits 0 with nothing printed"},
	)
}
