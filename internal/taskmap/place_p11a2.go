package taskmap

// Placements for batch P11, half A2: the crew commands that change a
// workspace or its session. A crew member is a persistent seat a person works
// in, so every entry sits in seats, beside the crew rows in place_p11a1.go.
//
// Group judgement: gt crew pristine runs git in a crew clone, which would fit
// host, but it acts only on the crew seat's own workspace, so it sits in seats
// as that workspace's upkeep. The other three are the seat's own lifecycle.
func init() {
	Register(
		Entry{Path: "crew pristine", Group: "seats", Use: "Use this when a crew workspace's checkout has fallen behind the remote and you want it pulled up to date, with any uncommitted changes called out.", Check: "each workspace prints ✓ git pull"},
		Entry{Path: "crew refresh", Group: "seats", Use: "Use this when a crew session is running low on context and should start over with its own handoff note waiting in its inbox.", Check: "gt crew status <name> shows the session running again"},
		Entry{Path: "crew remove", Group: "seats", Use: "Use this when a crew workspace is no longer needed and its seat should be taken away without discarding uncommitted work or killing a live session by accident.", Check: "gt crew list no longer shows the workspace"},
		Entry{Path: "crew rename", Group: "seats", Use: "Use this when a crew workspace carries the wrong name and its directory, session and state should all move to a new one.", Check: "gt crew list shows the workspace under its new name"},
	)
}
