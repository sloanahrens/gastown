package taskmap

// Placement for gt dolt users (gt-06tohf): the read that says whether the
// server's accounts still carry empty passwords. It sits in dolt, beside the
// commands that inspect or operate that server.
func init() {
	Register(
		Entry{Path: "dolt users", Group: "dolt", Use: "Use this to see the Dolt server's accounts and which of them have an empty password, before exposing the server or after a change to its accounts.", Check: "each account prints user@host with password: empty or password: set, or --check exits 1 naming the empty ones"},
	)
}
