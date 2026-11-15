package taskmap

// Placements for batch P12, half B part 4 (gt-mj7am4): the two gt slot verbs
// that run a suite through the container-gate slot and read whether it is
// held, beside the slot parent and reap verb place_p12bc.go places, and the
// two gt theme verbs that set a rig's tmux theme and restyle its running
// sessions.
//
// Group judgement: slot run and slot status sit in host for the reason
// place_p12bc.go records for the slot parent: the gate bounds the host's
// Docker VM, and these verbs are how a suite queues for it and how its holder
// is read. theme and theme apply are the judgement call. host, because theme
// writes a rig setting the way config does, and place_p10a1.go places config
// in host; apply restyles the sessions already running and moves none of them,
// so seats was rejected. internal was rejected because a person runs all four
// by hand, and their subject is the machine and the rig, not gt's own plumbing.
func init() {
	Register(
		Entry{Path: "slot run", Group: "host", Use: "Use this when a container-backed test suite has to run on this host and must wait its turn for the gate rather than starve the suites already on the Docker VM.", Check: "the run prints a Container-gate slot acquired line with how long it waited, and while it holds the slot gt slot status prints Container-gate slot: held by <role>"},
		Entry{Path: "slot status", Group: "host", Use: "Use this when a container-backed suite is queued behind the gate, to see whether the slot is held and by whom before you stop or start anything.", Check: "the command prints Container-gate slot: free when no suite holds the slot"},
		Entry{Path: "theme", Group: "host", Use: "Use this when a rig's tmux status bar should show a different theme, or to read which theme the rig is set to now.", Check: "gt theme prints a Theme: line naming the rig's theme"},
		Entry{Path: "theme apply", Group: "host", Use: "Use this after a rig's theme has changed, so the sessions already running pick it up without a restart.", Check: "the run ends with Applied theme to <n> session(s)"},
	)
}
