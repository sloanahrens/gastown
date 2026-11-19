package taskmap

// The entry for gt sandbox logs (gt-fldibt), which reads the log lines a
// sandbox seat's gateway keeps for it.
func init() {
	Register(
		Entry{Path: "sandbox logs", Group: "sandboxes", Use: "Use this when you need to read what a sandbox seat's supervisor or OCSF policy lines said, which no other gt command prints.", Check: "gt sandbox logs <rig>/<polecat> prints the gateway's log text"},
	)
}
