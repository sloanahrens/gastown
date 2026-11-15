package taskmap

// Placements for batch P11, half A1: the crew commands. A crew member is a
// persistent seat a person works in, so every entry sits in seats beside the
// agents and polecat verbs.
//
// Group judgement: gt crew fresh runs git, which would fit host, but it acts
// only on the caller's own crew clone, so it sits in seats with the workspace
// it updates. The rest are the workspace's own lifecycle, plainly seats.
func init() {
	Register(
		Entry{Path: "crew", Group: "seats", Use: "Use this when you are not sure which crew verb fits the job of setting up or looking after a person's workspace in a rig."},
		Entry{Path: "crew add", Group: "seats", Use: "Use this when a person needs a workspace of their own in a rig, before there is any session to attach to.", Check: "gt crew list shows the new workspace"},
		Entry{Path: "crew at", Group: "seats", Use: "Use this when you need to work in a crew member's terminal session, whether or not it is already running.", Check: "gt crew list shows the session running"},
		Entry{Path: "crew fresh", Group: "seats", Use: "Use this when your own crew clone's main may be behind origin and you want it brought up to date where that is safe.", Check: `the printed line ends "and moved"`},
		Entry{Path: "crew list", Group: "seats", Use: "Use this to see which crew workspaces a rig has and whether each one's session is running."},
	)
}
