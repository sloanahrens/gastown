package taskmap

// The entry for gt sandbox per-seat (gt-dmeph9), the way back of the per-seat
// Forgejo switch.
func init() {
	Register(
		Entry{Path: "sandbox per-seat", Group: "sandboxes", Use: "Use this to switch a sandbox profile's per-seat Forgejo push off, checked and backed up, when the switch must be undone even while the gateway is down.", Check: "gt sandbox per-seat <profile> off sets forgejo_per_seat false on that profile"},
	)
}
