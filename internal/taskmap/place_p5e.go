package taskmap

// Placements for batch P5, part 5 (gt-3u3nkj): gt polecat surviving-work and
// the session commands from gt session through gt session inject. Surviving-work
// tells whether a gone polecat's unmerged work still exists, so the bead can
// stay hooked. The session commands read, attach to or type into a polecat's
// tmux session.
//
// Group judgement: all five sit in seats, the group for inspecting or moving
// agent seats and sessions. Surviving-work is seats rather than roll because it
// reads a seat's leftover work and changes nothing. Session inject is seats
// rather than internal because its help tells the operator to use it by hand
// for file injection. The bare session entry is runnable (it has RunE), so it
// is an entry and not a namespace parent.
func init() {
	Register(
		Entry{Path: "polecat surviving-work", Group: "seats", Use: "Use this when a polecat is gone but its bead is still hooked, to learn whether the bead's unmerged work survives on a polecat branch before the bead is reset.", Check: "gt polecat surviving-work <bead-id> prints a branch and exits 0 when the work survives, or exits 3 when none does"},
		Entry{Path: "session", Group: "seats", Use: "Use this when you need to start, stop, attach to, read or list a polecat's tmux session and have not yet chosen which session subcommand to run."},
		Entry{Path: "session at", Group: "seats", Use: "Use this when you need to watch a polecat work live, or step in by hand, in its running session without stopping it.", Check: "the terminal shows the polecat's session, and Ctrl-B D detaches and leaves it running"},
		Entry{Path: "session capture", Group: "seats", Use: "Use this when you want to see how far a polecat has got without disturbing its session, before you decide to nudge it or stop it.", Check: "the last 100 lines of the polecat's terminal output print, or the count you pass"},
		Entry{Path: "session inject", Group: "seats", Use: "Use this when you need to type a file's contents, or raw tmux send-keys input, into a polecat's session; to send a message, use gt nudge instead.", Check: "gt session capture <rig>/<polecat> shows the injected text"},
	)
}
