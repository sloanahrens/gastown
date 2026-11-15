package taskmap

// Placements for batch P12, half B part 3 (gt-235do8): the two gt ring verbs
// that extend and read the lease, beside the forge and remove verbs
// place_p12bb.go places, and the container-gate slot, gt slot with reap.
//
// Group judgement: renew and status sit in host for the reason
// place_p12bb.go records there — the Ring is a signed record in
// mayor/town.json, verified under a key settings/config.json names. The slot
// pair is the judgement call. host: the gate bounds the host's Docker VM, and
// reap deletes containers left on that host, which is the same ground as
// gt doctor and the daemon compose and image commands, both already host.
// sandboxes was rejected because the gate serializes suites rather than
// isolating work; internal because a person starts both by hand, and the
// command's subject is the machine the town runs on, not gt's own plumbing.
func init() {
	Register(
		Entry{Path: "ring renew", Group: "host", Use: "Use this when the Ring is close to waning or has waned and automatic town work must keep running, to extend the lease by 36 hours from now.", Check: "gt ring status prints ring: valid and a wanes in line that starts at 36h00m"},
		Entry{Path: "ring status", Group: "host", Use: "Use this when automatic town work has stopped, or before you forge a Ring, to read whether the town's lease is valid, waned or absent.", Check: "the command prints ring: valid and exits 0; a waned or absent Ring is named and exits 2 or 3"},
		Entry{Path: "slot", Group: "host", Use: "Use this when container-backed test suites are queueing behind each other on the host's Docker VM, or a container a dead suite left behind is holding them up."},
		Entry{Path: "slot reap", Group: "host", Use: "Use this when a container the gate matches looks stale or a suite that died left its containers running, to delete the debris the gate judges dead.", Check: "a second gt slot reap prints No gate debris older than once no suite holds the slot"},
	)
}
