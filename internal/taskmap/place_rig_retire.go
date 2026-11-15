package taskmap

// The entry for gt rig retire-mayor. It takes host, the group for the machine
// and infrastructure the town runs on: the mayor/rig it retires is a directory
// under the town root, and no task group names one.
func init() {
	Register(
		Entry{Path: "rig retire-mayor", Group: "host", Use: "Use this when a rig's old mayor checkout has to come out of service: it lists what still points at that checkout before anything is renamed.", Check: "gt rig retire-mayor <rig> prints no refusal lines"},
	)
}
