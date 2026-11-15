package taskmap

// Placements for batch P11, half A2 part 1 (gt-j669b4.26): the crew commands
// that start, stop, look at and check a crew member's session. A crew member
// is a persistent seat a person works in, so every entry sits in seats beside
// the crew rows in place_p11a1.go and place_p11a2.go.
//
// Group judgement: gt crew stop could sit in shutdown, whose description says
// it stops sessions. It sits in seats instead: it ends one crew member's
// session (or a rig's crew), the mirror of gt crew start, and shutdown is for
// the town and emergency holds. gt crew stale runs git, which would fit host,
// but it only reads crew clones' fetch times, so it sits in seats with the
// workspaces it reports on, as gt crew fresh does.
func init() {
	Register(
		Entry{Path: "crew restart", Group: "seats", Use: "Use this when a crew member has become confused or stuck and needs a clean start with a fresh session, without a handoff note.", Check: "gt crew status <name> shows the session running again"},
		Entry{Path: "crew stale", Group: "seats", Use: "Use this to find the crew clones that have not fetched within a day, and any whose state could not be read, before choosing which to refresh.", Check: "empty output means every crew clone was fetched within a day"},
		Entry{Path: "crew start", Group: "seats", Use: "Use this when a rig's crew members, or the ones you name, need a running session, creating any workspace that does not exist yet.", Check: "gt crew status <name> shows the session running"},
		Entry{Path: "crew status", Group: "seats", Use: "Use this to see one crew member's session state, git state, branch and mail inbox, or every crew member's across all rigs.", Check: "the output names the session state and the workspace's git status"},
		Entry{Path: "crew stop", Group: "seats", Use: "Use this when a crew member's session must end, by name, for a whole rig, or with --all, keeping its output for debugging unless --force skips the capture.", Check: "gt crew status <name> shows the session no longer running"},
	)
}
