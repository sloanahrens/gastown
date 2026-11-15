package taskmap

// Placements for batch P10, half A1 (gt-j669b4.14): the gt config commands,
// all in host because what they read and change are the town's settings.
func init() {
	Register(
		Entry{Path: "config", Group: "host", Use: "Use this when a town setting must be read or changed, from which agents it can run to how the daemon behaves."},
		Entry{Path: "config agent", Group: "host", Use: "Use this when you need to see which agents the town can run, or to change one agent's command or remove it."},
		Entry{Path: "config agent get", Group: "host", Use: "Use this to read one agent's command and arguments before you rely on how it starts a session.", Check: "the printed header names the agent and the Command line gives its binary"},
		Entry{Path: "config agent list", Group: "host", Use: "Use this to see every agent the town offers, built-in and custom, before you name one in a dispatch or as a rig's default.", Check: "the printed list names each agent and ends with the town's Default"},
		Entry{Path: "config agent remove", Group: "host", Use: "Use this when a custom agent is no longer wanted and the town should stop offering it.", Check: "the command prints Removed, naming the agent"},
	)
}
