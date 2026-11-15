package taskmap

// Placements for batch P10, half A2 part 1 (gt-j669b4.25): the gt config
// daemon commands and the town's default agent. Each reads or changes a town
// setting, so each sits in host beside the rest of gt config (place_p10a1.go,
// place_p10a2.go) and the gt daemon commands (place_p8.go).
//
// Group judgement: config default-agent is host rather than dispatch. It
// chooses the agent the town starts for a rig that names none, which is a
// setting on this host, not a hand-off of work; the dispatch group is for the
// commands that give work to an agent (gt sling and its neighbors).
func init() {
	Register(
		Entry{Path: "config daemon diff", Group: "host", Use: "Use this to see what the last write to the daemon's settings changed, comparing the newest backup against the file as it is now.", Check: "the command prints a unified diff, or '<file> is unchanged since <backup>' when the file matches its newest backup"},
		Entry{Path: "config daemon get", Group: "host", Use: "Use this to read one setting the daemon runs with, or the whole settings/config.json, before deciding what to change.", Check: "the value prints as indented JSON, and a path the file does not hold is an error naming the path"},
		Entry{Path: "config daemon set", Group: "host", Use: "Use this when one daemon setting must change and the change must pass the daemon's own validation and leave a named backup before it is saved.", Check: "the command prints a unified diff of the change and leaves a config.json.bak-<timestamp>-<label> copy, and prints nothing when the value is already set"},
		Entry{Path: "config daemon unset", Group: "host", Use: "Use this when a daemon setting must go back to its default by removing the key, rather than being set to an empty value.", Check: "the command prints a unified diff of the change and leaves a config.json.bak-<timestamp>-<label> copy"},
		Entry{Path: "config default-agent", Group: "host", Use: "Use this to see or change which agent the town starts for a rig that names no agent of its own.", Check: "the command prints Default agent: <name> with no argument and Default agent set to '<name>' with one"},
	)
}
