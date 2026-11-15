package taskmap

// Placements for batch P10, half B, part 1 (gt-j669b4.21): gt config set and
// gt config validate, which sit in host beside the rest of gt config
// (place_p10a1.go), and the Claude Code hook handlers under gt tap, which go in
// internal because an agent never runs them by hand.
//
// Group judgement: config set writes keys from several areas (the Dolt port,
// the scheduler, the lifecycle timers). It sits in host because it is the one
// generic setter beside config get; the spending preset has its own command,
// config cost-tier (place_p10a2.go), in cost.
func init() {
	Register(
		Entry{Path: "config set", Group: "host", Use: "Use this when one town setting must be changed by its dot-notation key, such as the Dolt port, the scheduler's spawn delay or the reaper's interval, rather than by editing the settings file by hand.", Check: "the command prints Set <key> = <value>"},
		Entry{Path: "config validate", Group: "host", Use: "Use this after you edit the town's config files and before you restart the daemon, to see whether every file still loads.", Check: "the command prints Town config loads (<layout> layout) and exits 0; a file that does not load is named and the exit is non-zero"},
		Entry{Path: "tap", Group: "internal", Use: "Use this when a Claude Code hook must check or act on a tool call before or after it runs, since the hook calls the handler and an agent does not."},
		Entry{Path: "tap guard", Group: "internal", Use: "Use this when a tool call must be refused before it runs because it breaks a town policy, such as a force push or a close that skips gt done."},
		Entry{Path: "tap guard bd-close-invariant", Group: "internal", Use: "Use this when a bd close must be refused because the bead's polecat branch still carries unmerged work, so the close goes through gt done instead."},
	)
}
