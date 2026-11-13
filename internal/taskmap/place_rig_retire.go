package taskmap

// The entry for gt rig retire-mayor, which came after the P batches were cut
// and so has no line in testdata/unplaced.
func init() {
	Register(
		Entry{Path: "rig retire-mayor", Group: "host", Use: "Use this when a rig's old mayor checkout has to come out of service: it lists what still points at that checkout before anything is renamed.", Check: "gt rig retire-mayor <rig> prints no refusal lines"},
	)
}
