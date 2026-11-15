package taskmap

// Placements for batch P12, half B part 5 (gt-mv8adt): the CLI color-mode verb
// beside the two theme verbs place_p12bd.go places, and the three tracker
// paths.
//
// Group judgement: theme cli sits in host with theme and theme apply, and with
// config, which place_p10a1.go also places in host: like both, it writes a
// setting into a town or rig-owned file, here settings/config.json, where theme
// writes the current rig's. The tracker paths are the judgement call, and all
// three are in dolt:
// the tracker is the store that holds a rig's issues once it flips off Dolt,
// status reads its health and its per-rig counts, and replay writes the
// issues it took back into the rig's database through bd. roll was rejected
// for replay: that group undoes, replays or promotes a landing, and this
// replays a store flip, not a landing. host was rejected because what an
// agent asks about here is the issues, not the machine.
func init() {
	Register(
		Entry{Path: "theme cli", Group: "host", Use: "Use this when gt's output is hard to read against your terminal's background, to force dark or light colors or return to auto-detection.", Check: "gt theme cli with no mode prints CLI Theme: and an Effective: line naming the mode in force"},
		Entry{Path: "tracker", Group: "dolt", Use: "Use this when the town's issues are served by the tracker rather than Dolt, to reach the commands that read its health and replay its writes."},
		Entry{Path: "tracker replay", Group: "dolt", Use: "Use this when a rig has flipped back to Dolt after a soak on the tracker, to apply the writes the tracker took since the flip so none are lost.", Check: "a --dry-run prints the bd calls and ends dry run: N bd calls, none run; would apply through seq N"},
		Entry{Path: "tracker status", Group: "dolt", Use: "Use this when the tracker is suspected of being down or slow, to see its URL, readiness, per-rig issue counts and slowest route on one screen.", Check: "the screen prints a Ready: line and the command exits 0 when the tracker answers ready"},
	)
}
