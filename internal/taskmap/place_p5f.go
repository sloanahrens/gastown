package taskmap

// Placements for batch P5, part 6 (gt-72lqd2): gt session list, gt session
// restart, gt session start, gt session status and gt session stop. The bare
// session command and its other subcommands are placed in place_p5e.go.
//
// Group judgement: start, list, status and restart sit in seats, the group for
// inspecting or moving agent seats and sessions. Stop sits in shutdown instead,
// because stopping a session is the job its help names, and shutdown is the
// group for stopping agents or sessions. Its sandbox and park behavior is a
// seat concern, but a reader looks it up to end a session, not to manage a seat.
func init() {
	Register(
		Entry{Path: "session list", Group: "seats", Use: "Use this when you need to see which polecat sessions exist, running or stopped, or only those in one rig.", Check: "gt session list prints an Active Sessions heading, then two lines per session: a filled dot ● for a running one or a hollow dot ○ for a stopped one, with its rig/polecat, then its session id"},
		Entry{Path: "session restart", Group: "seats", Use: "Use this when a polecat's running session is stuck or stale and needs a fresh one in its place.", Check: "gt session status <rig>/<polecat> shows State: running with a new Uptime"},
		Entry{Path: "session start", Group: "seats", Use: "Use this when a polecat has no running session and must start one to pick up its work, optionally naming the issue it should work on.", Check: "gt session status <rig>/<polecat> shows State: running"},
		Entry{Path: "session status", Group: "seats", Use: "Use this when you need to know whether a polecat's session is running, and how long it has been up, before you nudge, restart or stop it.", Check: "gt session status <rig>/<polecat> prints a State line, and an Uptime line when the session is running"},
		Entry{Path: "session stop", Group: "shutdown", Use: "Use this when a polecat's session must stay off because the operator stopped it on purpose and a restart would undo that.", Check: "gt session status <rig>/<polecat> shows State: stopped"},
	)
}
